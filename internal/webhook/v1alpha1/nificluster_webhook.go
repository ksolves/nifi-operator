/*
Copyright 2026 Ksolves India Limited.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/controller"
)

// nificlusterlog is for logging in this package.
var nificlusterlog = logf.Log.WithName("nificluster-resource")

// defaultCPURequest, defaultMemoryRequest, defaultCPULimit and defaultMemoryLimit are applied to
// any NodeGroup that doesn't specify its own resource requests/limits, so a NiFiCluster created
// without Resources set doesn't end up with unbounded (Burstable/BestEffort-only) NiFi pods.
var (
	defaultCPURequest    = resource.MustParse("500m")
	defaultMemoryRequest = resource.MustParse("1Gi")
	defaultCPULimit      = resource.MustParse("2")
	defaultMemoryLimit   = resource.MustParse("2Gi")

	// tlsMaxCertificateValidityDays caps how long a TLS-enabled node group's certificate
	// validity can be. reconcileTLSRotation (internal/controller) rotates a pod at roughly
	// half of this period, so an absurdly large value would effectively disable rotation for
	// years - defeating the whole point of automatic renewal.
	tlsMaxCertificateValidityDays int32 = 3650 // 10 years
)

// nfsCIFSProvisioners lists StorageClass provisioner names known to back NFS/CIFS storage,
// which Feature 8 (PVC-only storage) excludes.
var nfsCIFSProvisioners = []string{
	"nfs", // covers nfs.csi.k8s.io, nfs-subdir-external-provisioner, and similar
	"cifs",
	"smb.csi.k8s.io",
}

// SetupNiFiClusterWebhookWithManager registers the webhook for NiFiCluster in the manager.
func SetupNiFiClusterWebhookWithManager(mgr ctrl.Manager, opts WebhookOptions) error {
	v := newClusterValidator(mgr, opts)
	return ctrl.NewWebhookManagedBy(mgr).For(&nifiv1alpha1.NiFiCluster{}).
		WithValidator(&v).
		WithDefaulter(&NiFiClusterCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-nifi-ksolves-com-v1alpha1-nificluster,mutating=true,failurePolicy=fail,sideEffects=None,timeoutSeconds=5,groups=nifi.ksolves.com,resources=nificlusters,verbs=create;update,versions=v1alpha1,name=mnificluster-v1alpha1.kb.io,admissionReviewVersions=v1

// NiFiClusterCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind NiFiCluster when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type NiFiClusterCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &NiFiClusterCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind NiFiCluster.
//
// Every field with a static default (Replicas, ServiceType, TerminationGracePeriodSeconds,
// LogStorage, PodDisruptionBudget.Enabled) is already handled by the CRD's own
// +kubebuilder:default markers - that's native API server behavior, no webhook involvement
// needed. The one thing that can't be expressed as a static CRD default is resource
// requests/limits (there's no single sensible default resource.Quantity to put on the type
// itself), so that's defaulted here, together with TLS on for new node groups (defaultNodeGroupTLS).
func (d *NiFiClusterCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	nificluster, ok := obj.(*nifiv1alpha1.NiFiCluster)

	if !ok {
		return fmt.Errorf("expected an NiFiCluster object but got %T", obj)
	}
	nificlusterlog.Info("Defaulting for NiFiCluster", "name", nificluster.GetName())

	creating := isCreate(ctx)
	for i := range nificluster.Spec.NodeGroups {
		defaultNodeGroupResources(&nificluster.Spec.NodeGroups[i])
		if creating {
			defaultNodeGroupTLS(&nificluster.Spec.NodeGroups[i])
		}
	}
	// No image: the operator's default node image, written into the spec so the cluster keeps
	// it when a later operator release changes the default.
	if nificluster.Spec.Image.Repository == "" && nificluster.Spec.Image.Tag == "" {
		nificluster.Spec.Image = controller.DefaultNiFiImage
	}

	return nil
}

// isCreate reports whether ctx carries a create admission request. Without a request (a direct
// call, not through the API server) it reports false, so nothing is defaulted that could change
// an existing object.
func isCreate(ctx context.Context) bool {
	req, err := admission.RequestFromContext(ctx)
	return err == nil && req.Operation == admissionv1.Create
}

// defaultNodeGroupTLS turns TLS on for a node group created without a tls block. Only on create:
// on an existing node group nil means TLS off, and defaulting it then would switch a running
// cluster to TLS.
func defaultNodeGroupTLS(nodeGroup *nifiv1alpha1.NodeGroup) {
	if nodeGroup.TLS == nil {
		nodeGroup.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
	}
}

// defaultNodeGroupResources fills in CPU/memory requests and limits for a NodeGroup that didn't
// specify its own, leaving any resource type the user did set untouched. A default never crosses
// the value the user set on the other side: requests.memory 4Gi alone gets a 4Gi limit, not the 2Gi
// default, which Kubernetes would reject (request above limit) so no pod could ever be created.
func defaultNodeGroupResources(nodeGroup *nifiv1alpha1.NodeGroup) {
	if nodeGroup.Resources.Requests == nil {
		nodeGroup.Resources.Requests = corev1.ResourceList{}
	}
	if nodeGroup.Resources.Limits == nil {
		nodeGroup.Resources.Limits = corev1.ResourceList{}
	}
	defaultResource(nodeGroup.Resources, corev1.ResourceCPU, defaultCPURequest, defaultCPULimit)
	defaultResource(nodeGroup.Resources, corev1.ResourceMemory, defaultMemoryRequest, defaultMemoryLimit)
}

// defaultResource fills in a missing request or limit for one resource. Both maps must be non-nil.
func defaultResource(resources corev1.ResourceRequirements, name corev1.ResourceName, request, limit resource.Quantity) {
	userRequest, hasRequest := resources.Requests[name]
	userLimit, hasLimit := resources.Limits[name]
	if !hasRequest {
		if hasLimit && userLimit.Cmp(request) < 0 {
			request = userLimit
		}
		resources.Requests[name] = request
	}
	if !hasLimit {
		if hasRequest && userRequest.Cmp(limit) > 0 {
			limit = userRequest
		}
		resources.Limits[name] = limit
	}
}

// +kubebuilder:webhook:path=/validate-nifi-ksolves-com-v1alpha1-nificluster,mutating=false,failurePolicy=fail,sideEffects=None,timeoutSeconds=5,groups=nifi.ksolves.com,resources=nificlusters,verbs=create;update,versions=v1alpha1,name=vnificluster-v1alpha1.kb.io,admissionReviewVersions=v1

// NiFiClusterCustomValidator struct is responsible for validating the NiFiCluster resource
// when it is created or updated. Deletion isn't validated - there's nothing to reject there.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type NiFiClusterCustomValidator struct {
	// Client is used to look up a NodeGroup's StorageClass, to check its provisioner isn't
	// NFS/CIFS-backed (Feature 8). Left nil-safe: if a StorageClass lookup fails (e.g. transient
	// API error, or the StorageClass simply doesn't exist yet), that check is skipped rather
	// than blocking admission - the eventual PVC-provisioning failure is a normal, existing
	// Kubernetes failure mode covering that.
	Client client.Client
	// Reader reads user-referenced Secrets uncached: the manager's cache strips the data of Secrets
	// the operator doesn't own. Falls back to Client when nil (unit tests).
	Reader client.Reader
	// TrustedImageRepositories are the image repositories a NiFiCluster may use without its editor
	// being allowed to create pods (see imageGrants).
	TrustedImageRepositories []string
}

// WebhookOptions configure the NiFiCluster and NiFiNodeGroup validators.
type WebhookOptions struct {
	// TrustedImageRepositories: see NiFiClusterCustomValidator.
	TrustedImageRepositories []string
}

func newClusterValidator(mgr ctrl.Manager, opts WebhookOptions) NiFiClusterCustomValidator {
	return NiFiClusterCustomValidator{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), TrustedImageRepositories: opts.TrustedImageRepositories,
	}
}

var _ webhook.CustomValidator = &NiFiClusterCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type NiFiCluster.
func (v *NiFiClusterCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	nificluster, ok := obj.(*nifiv1alpha1.NiFiCluster)
	if !ok {
		return nil, fmt.Errorf("expected a NiFiCluster object but got %T", obj)
	}
	nificlusterlog.Info("Validation for NiFiCluster upon creation", "name", nificluster.GetName())

	warnings, err := v.validateWithNodeGroupObjects(ctx, nil, nificluster)
	imageWarnings, imageErrs := validateImage(nil, nificluster)
	warnings = append(warnings, securityContextWarnings(nificluster)...)
	warnings = append(warnings, pausedAnnotationWarnings(nificluster)...)
	return append(warnings, imageWarnings...), joinErrors(joinErrors(err, v.authorizeExposure(ctx, nil, nificluster)), imageErrs)
}

// validateImage requires both or neither of spec.image's repository and tag (the defaulter fills
// in neither), and a NiFi version, read from the tag, that the cluster's settings support:
//   - Kubernetes coordination needs NiFi 2.11.0 or later, whose native clustering older NiFi
//     lacks;
//   - ZooKeeper coordination needs NiFi 1.14.0 or later.
//
// A tag without a version is only warned about. On update the check runs only when the image or the
// coordination backend changed, so an unrelated edit never fails on an image already running.
func validateImage(oldCluster, cluster *nifiv1alpha1.NiFiCluster) (admission.Warnings, field.ErrorList) {
	image := cluster.Spec.Image
	if oldCluster != nil && oldCluster.Spec.Image == image && backendName(oldCluster) == backendName(cluster) {
		return nil, nil
	}
	path := field.NewPath("spec", "image")
	if (image.Repository == "") != (image.Tag == "") {
		return nil, field.ErrorList{field.Required(path, "set both repository and tag, or neither for the default node image")}
	}
	if image.Tag == "" {
		return nil, nil
	}
	version, ok := controller.NiFiVersionFromTag(image.Tag)
	minimum := controller.MinimumNiFiVersionFor(cluster)
	if !ok {
		return admission.Warnings{fmt.Sprintf("spec.image.tag %q names no NiFi version; the operator needs NiFi %s or later "+
			"with %s coordination", image.Tag, versionText(minimum), backendName(cluster))}, nil
	}
	tooOld := func(minimum [3]int, reason string) field.ErrorList {
		return field.ErrorList{field.Invalid(path.Child("tag"), image.Tag, fmt.Sprintf("NiFi %s is too old %s: "+
			"the operator needs NiFi %s or later (see docs/compatibility.md)", versionText(version), reason, versionText(minimum)))}
	}
	if controller.VersionLess(version, minimum) {
		reason := "for Kubernetes coordination, the only coordination the Community edition supports " +
			"(NiFi before 2.11.0 needs external ZooKeeper, which it does not)"
		if backendName(cluster) == "ZooKeeper" {
			reason = "for this operator"
		}
		return nil, tooOld(minimum, reason)
	}
	if oldCluster != nil {
		if old, ok := controller.NiFiVersion(oldCluster); ok && old[0] != version[0] {
			return admission.Warnings{fmt.Sprintf("spec.image moves NiFi %s to %s: nodes of two major versions do not form "+
				"one cluster, and NiFi 2 changed the flow format; create a new cluster and move the flow to it "+
				"(docs/compatibility.md)", versionText(old), versionText(version))}, nil
		}
	}
	return nil, nil
}

// backendName is the cluster's coordination backend as written in the spec, Kubernetes when unset.
func backendName(cluster *nifiv1alpha1.NiFiCluster) string {
	if c := cluster.Spec.Coordination; c != nil && c.Backend == nifiv1alpha1.CoordinationBackendZooKeeper {
		return "ZooKeeper"
	}
	return "Kubernetes"
}

func versionText(v [3]int) string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// validateWithNodeGroupObjects validates the cluster with its NiFiNodeGroups merged in, as the
// controller reconciles it: cross-group rules (uniform TLS) then cover
// both kinds of node group. Requester authorization still only looks at the NiFiCluster's own
// fields; each NiFiNodeGroup's are checked when it is written.
//
// oldCluster is nil on create. The node limit only rejects growth past it, so a cluster that is
// already over it (through /scale, which bypasses this webhook) can still shrink, be deleted and
// have its finalizer removed.
func (v *NiFiClusterCustomValidator) validateWithNodeGroupObjects(
	ctx context.Context, oldCluster, cluster *nifiv1alpha1.NiFiCluster,
) (admission.Warnings, error) {
	objects, err := v.nodeGroupObjects(ctx, cluster, "")
	if err != nil {
		return nil, err
	}
	var errs field.ErrorList
	inline := map[string]bool{}
	for i, g := range cluster.Spec.NodeGroups {
		inline[g.Name] = true
		for _, o := range objects {
			if o.Name == g.Name {
				errs = append(errs, field.Invalid(field.NewPath("spec", "nodeGroups").Index(i).Child("name"), g.Name,
					"a NiFiNodeGroup of this cluster already has this name"))
			}
		}
	}
	var others []nifiv1alpha1.NiFiNodeGroup
	for _, o := range objects {
		if !inline[o.Name] {
			others = append(others, o)
		}
	}
	merged := withNodeGroups(cluster, others, nil, "")
	var oldMerged *nifiv1alpha1.NiFiCluster
	if oldCluster != nil {
		oldMerged = withNodeGroups(oldCluster, others, nil, "")
	}
	errs = append(errs, validateNodeLimit(field.NewPath("spec", "nodeGroups"), oldMerged, merged)...)
	errs = append(errs, validateLoadBalancerTLS(field.NewPath("spec", "serviceType"), oldMerged, merged)...)
	warnings, verr := v.validate(ctx, merged)
	if len(merged.Spec.NodeGroups) == 0 {
		warnings = append(warnings, "the cluster has no node groups yet: add spec.nodeGroups or NiFiNodeGroups "+
			"with clusterRef "+cluster.Name)
	}
	return warnings, joinErrors(verr, errs)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type NiFiCluster.
func (v *NiFiClusterCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	nificluster, ok := newObj.(*nifiv1alpha1.NiFiCluster)
	if !ok {
		return nil, fmt.Errorf("expected a NiFiCluster object for the newObj but got %T", newObj)
	}
	oldCluster, ok := oldObj.(*nifiv1alpha1.NiFiCluster)
	if !ok {
		return nil, fmt.Errorf("expected a NiFiCluster object for the oldObj but got %T", oldObj)
	}
	nificlusterlog.Info("Validation for NiFiCluster upon update", "name", nificluster.GetName())

	warnings, err := v.validateWithNodeGroupObjects(ctx, oldCluster, nificluster)
	warnings = append(warnings, removedNodeGroupWarnings(oldCluster, nificluster)...)
	warnings = append(warnings, zooKeeperConnectStringWarnings(oldCluster, nificluster)...)
	warnings = append(warnings, pausedAnnotationWarnings(nificluster)...)

	err = joinErrors(err, v.authorizeExposure(ctx, oldCluster, nificluster))
	err = joinErrors(err, validateSplitRepositoryStorageImmutable(oldCluster, nificluster))
	err = joinErrors(err, v.validateStorageChanges(ctx, oldCluster, nificluster))
	imageWarnings, imageErrs := validateImage(oldCluster, nificluster)
	warnings = append(warnings, imageWarnings...)
	warnings = append(warnings, securityContextWarnings(nificluster)...)
	err = joinErrors(err, imageErrs)
	return warnings, err
}

// joinErrors combines validate's aggregate error with further field errors into one Invalid
// admission error.
func joinErrors(err error, more field.ErrorList) error {
	if len(more) == 0 {
		return err
	}
	if err == nil {
		return more.ToAggregate()
	}
	return fmt.Errorf("%w; %w", err, more.ToAggregate())
}

// validateSplitRepositoryStorageImmutable rejects flipping SplitRepositoryStorage's Enabled flag
// on a node group that already exists in oldCluster - the operator has no mechanism to migrate a
// node group's already-written data (flow definition, content, provenance history) from the
// combined Storage PVC to the new per-repository layout (or back), so doing so silently orphans
// that data instead of making it disappear outright, which is worse: it looks like dataloss with
// no clear cause. A newly-added node group (not present in oldCluster) is unaffected - it has no
// prior state to orphan, so any value is fine on first creation. The per-repository sizes are
// checked by validateStorageChanges.
func validateSplitRepositoryStorageImmutable(oldCluster, newCluster *nifiv1alpha1.NiFiCluster) field.ErrorList {
	oldByName := make(map[string]bool, len(oldCluster.Spec.NodeGroups))
	for _, ng := range oldCluster.Spec.NodeGroups {
		oldByName[ng.Name] = controller.SplitRepositoryStorageEnabled(&ng)
	}

	var errs field.ErrorList
	nodeGroupsPath := field.NewPath("spec").Child("nodeGroups")
	for i, ng := range newCluster.Spec.NodeGroups {
		oldValue, existed := oldByName[ng.Name]
		if existed && oldValue != controller.SplitRepositoryStorageEnabled(&ng) {
			errs = append(errs, field.Forbidden(nodeGroupsPath.Index(i).Child("splitRepositoryStorage").Child("enabled"),
				"cannot be changed after the node group is created - the operator cannot migrate "+
					"existing repository data to or from the per-repository storage layout"))
		}
	}
	return errs
}

// validateStorageChanges checks changes to a node group's volumes: neither the sizes nor the
// StorageClass of an existing node group can change. Sizes are compared as quantities, so 1Gi
// and 1024Mi count as the same. When a cluster's only node group is renamed, the new group is
// compared with the old one: a rename keeps the sizes too.
func (v *NiFiClusterCustomValidator) validateStorageChanges(
	_ context.Context, oldCluster, newCluster *nifiv1alpha1.NiFiCluster,
) field.ErrorList {
	oldByName := make(map[string]*nifiv1alpha1.NodeGroup, len(oldCluster.Spec.NodeGroups))
	for i := range oldCluster.Spec.NodeGroups {
		oldByName[oldCluster.Spec.NodeGroups[i].Name] = &oldCluster.Spec.NodeGroups[i]
	}

	var errs field.ErrorList
	for i := range newCluster.Spec.NodeGroups {
		ng := &newCluster.Spec.NodeGroups[i]
		old, existed := oldByName[ng.Name]
		renamed := false
		if !existed && len(oldCluster.Spec.NodeGroups) == 1 && len(newCluster.Spec.NodeGroups) == 1 {
			// The only node group renamed: its sizes are compared with the one it replaces, so a
			// rename can't change them either. The StorageClass of its new volumes is free.
			old, existed, renamed = &oldCluster.Spec.NodeGroups[0], true, true
		}
		if !existed {
			continue
		}
		path := field.NewPath("spec").Child("nodeGroups").Index(i)
		if !renamed && ptr.Deref(old.StorageClassName, "") != ptr.Deref(ng.StorageClassName, "") {
			errs = append(errs, field.Forbidden(path.Child("storageClassName"), "cannot be changed on an existing "+
				"node group: its PersistentVolumeClaims keep the StorageClass they were created with. Create a new node group"))
		}
		sizes := map[string][2]string{
			"storage":    {old.Storage, ng.Storage},
			"logStorage": {old.LogStorage, ng.LogStorage},
		}
		if o, n := old.SplitRepositoryStorage, ng.SplitRepositoryStorage; o != nil && n != nil {
			sizes["splitRepositoryStorage.flowFileRepositoryStorage"] = [2]string{o.FlowFileRepositoryStorage, n.FlowFileRepositoryStorage}
			sizes["splitRepositoryStorage.contentRepositoryStorage"] = [2]string{o.ContentRepositoryStorage, n.ContentRepositoryStorage}
			sizes["splitRepositoryStorage.provenanceRepositoryStorage"] = [2]string{o.ProvenanceRepositoryStorage, n.ProvenanceRepositoryStorage}
			sizes["splitRepositoryStorage.databaseRepositoryStorage"] = [2]string{o.DatabaseRepositoryStorage, n.DatabaseRepositoryStorage}
			sizes["splitRepositoryStorage.extensionsStorage"] = [2]string{o.ExtensionsStorage, n.ExtensionsStorage}
		}
		for _, name := range sortedSizeKeys(sizes) {
			fieldPath := path
			for _, part := range strings.Split(name, ".") {
				fieldPath = fieldPath.Child(part)
			}
			if compareQuantity(sizes[name][0], sizes[name][1]) != 0 {
				hint := "expand the PVCs with kubectl patch pvc"
				if renamed {
					hint = fmt.Sprintf("a renamed node group keeps the sizes of %q, the one it replaces", old.Name)
				}
				errs = append(errs, field.Forbidden(fieldPath, fmt.Sprintf("cannot change from %s to %s: "+
					"storage size changes are not supported in the Community edition; %s",
					sizes[name][0], sizes[name][1], hint)))
			}
		}
	}
	return errs
}

// compareQuantity compares two quantity strings like resource.Quantity.Cmp; unparsable or equal
// values compare as 0 (the CRD rejects unparsable ones).
func compareQuantity(a, b string) int {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return 0
	}
	return qa.Cmp(qb)
}

// sortedSizeKeys returns m's keys in sorted order, for deterministic error output.
func sortedSizeKeys(m map[string][2]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// reconcilePausedAnnotation paused reconciliation in earlier operator versions; it is ignored now.
const reconcilePausedAnnotation = "nifi.ksolves.com/reconcile-paused"

// pausedAnnotationWarnings warns that the reconcile-paused annotation no longer pauses anything.
func pausedAnnotationWarnings(cluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	if _, set := cluster.Annotations[reconcilePausedAnnotation]; !set {
		return nil
	}
	return admission.Warnings{fmt.Sprintf("metadata.annotations[%s] is ignored: pausing reconciliation is not "+
		"available in the Community edition, and the operator keeps reconciling this cluster", reconcilePausedAnnotation)}
}

// zooKeeperConnectStringWarnings warns when the ZooKeeper connect string of a running cluster
// changes. The pods roll onto the new ensemble one by one, and while some are on each, the nodes
// form two separate NiFi clusters. It's a warning, not a rejection: moving to a new
// ensemble that holds the same data is legitimate.
func zooKeeperConnectStringWarnings(oldCluster, newCluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	connect := func(c *nifiv1alpha1.NiFiCluster) string {
		if co := c.Spec.Coordination; co != nil && co.ZooKeeper != nil {
			return co.ZooKeeper.ConnectString
		}
		return ""
	}
	before, after := connect(oldCluster), connect(newCluster)
	if before == "" || after == "" || before == after {
		return nil
	}
	return admission.Warnings{fmt.Sprintf("spec.coordination.zookeeper.connectString changes from %q to %q: the NiFi "+
		"pods roll onto the new ensemble one at a time and, until all have, nodes on different ensembles form "+
		"separate clusters. Make sure the new ensemble holds this cluster's znodes, or scale to zero first.", before, after)}
}

// removedNodeGroupWarnings warns about every node group present in oldCluster but not in
// newCluster. Removing a group is legitimate, so this doesn't reject; but a rename looks exactly
// like a removal plus an addition, and the controller then deletes the old StatefulSet while its
// PVCs are retained (WhenDeleted: Retain), so the renamed group silently starts with empty
// repositories.
func removedNodeGroupWarnings(oldCluster, newCluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	current := make(map[string]bool, len(newCluster.Spec.NodeGroups))
	for _, nodeGroup := range newCluster.Spec.NodeGroups {
		current[nodeGroup.Name] = true
	}
	var warnings admission.Warnings
	for _, nodeGroup := range oldCluster.Spec.NodeGroups {
		if !current[nodeGroup.Name] {
			warnings = append(warnings, fmt.Sprintf(
				"nodeGroup %q removed: its StatefulSet will be deleted but its PVCs are retained; "+
					"a renamed group does not reuse them and starts with empty repositories", nodeGroup.Name))
		}
	}
	return warnings
}

// validate runs every create/update validation rule and returns a single aggregated error, so a
// user sees every problem with their spec at once rather than fixing one field at a time.
// Warnings are returned for problems that don't block admission (e.g. a referenced Secret that
// may simply not have been created yet by a GitOps tool or secret syncer).
func (v *NiFiClusterCustomValidator) validate(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (admission.Warnings, error) {
	var allErrs field.ErrorList
	var warnings admission.Warnings
	nodeGroupsPath := field.NewPath("spec").Child("nodeGroups")

	seenNames := make(map[string]bool, len(cluster.Spec.NodeGroups))
	for i, nodeGroup := range cluster.Spec.NodeGroups {
		nodeGroupPath := nodeGroupsPath.Index(i)
		if seenNames[nodeGroup.Name] {
			allErrs = append(allErrs, field.Duplicate(nodeGroupPath.Child("name"), nodeGroup.Name))
		}
		seenNames[nodeGroup.Name] = true

		allErrs = append(allErrs, v.validateNodeGroup(ctx, cluster, nodeGroupPath, &nodeGroup)...)
	}

	allErrs = append(allErrs, validateUniformTLS(nodeGroupsPath, cluster.Spec.NodeGroups)...)

	specPath := field.NewPath("spec")
	if errs := validateConfigOverrides(specPath.Child("configOverrides"), cluster, cluster.Spec.ConfigOverrides); len(errs) > 0 {
		allErrs = append(allErrs, errs...)
	}

	if c := cluster.Spec.Coordination; c != nil && c.ZooKeeper != nil {
		allErrs = append(allErrs, validateZooKeeper(specPath.Child("coordination", "zookeeper"), c.ZooKeeper)...)
	}

	{
		svc := cluster.Spec.Service
		if svc == nil {
			svc = &nifiv1alpha1.ClusterServiceSpec{}
		}
		errs, warns := validateClusterService(specPath.Child("service"), cluster.Spec.ServiceType, svc)
		allErrs = append(allErrs, errs...)
		warnings = append(warnings, warns...)
	}

	for i, host := range cluster.Spec.ProxyHosts {
		if err := validateProxyHost(host); err != nil {
			allErrs = append(allErrs, field.Invalid(specPath.Child("proxyHosts").Index(i), host, err.Error()))
		}
	}

	if auth := cluster.Spec.Authentication; auth != nil && auth.SingleUser != nil {
		authPath := specPath.Child("authentication").Child("singleUser")
		errs, warns := v.validateSingleUserAuth(ctx, authPath, cluster.Namespace, auth.SingleUser)
		allErrs = append(allErrs, errs...)
		warnings = append(warnings, warns...)
		// The operator owns, rewrites and deletes <cluster>-auth-credentials itself, so pointing
		// secretName at it would have the operator fight the user over it.
		if name := controller.AuthSecretName(cluster.Name); auth.SingleUser.SecretName == name {
			allErrs = append(allErrs, field.Invalid(authPath.Child("secretName"), name,
				"is the operator-managed login Secret for this cluster; name a Secret you manage yourself"))
		}
	}

	warnings = append(warnings, coordinationServiceAccountWarnings(cluster)...)
	warnings = append(warnings, pvcDeleteWarnings(cluster)...)
	warnings = append(warnings, repositoryWarnings(cluster)...)
	warnings = append(warnings, tlsDisabledWarnings(cluster)...)

	if len(allErrs) == 0 {
		return warnings, nil
	}
	return warnings, allErrs.ToAggregate()
}

// repositoryWarnings flags repository settings that can't work as intended on the node group's
// volumes: a provenance cap above the volume holding the provenance repository, and an
// archive allowed to fill the content repository's disk almost completely.
func repositoryWarnings(cluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	var warnings admission.Warnings
	for i := range cluster.Spec.NodeGroups {
		g := &cluster.Spec.NodeGroups[i]
		if size, ok := controller.ProvenanceMaxStorageBytes(cluster, g); ok {
			volume, name := g.Storage, "storage"
			if controller.SplitRepositoryStorageEnabled(g) {
				volume, name = g.SplitRepositoryStorage.ProvenanceRepositoryStorage, "splitRepositoryStorage.provenanceRepositoryStorage"
			}
			if q, err := resource.ParseQuantity(volume); err == nil && size > q.Value() {
				warnings = append(warnings, fmt.Sprintf("node group %q: repositories.provenance.maxStorageSize is larger than "+
					"its volume (%s %s); NiFi stops writing provenance when the disk is full instead", g.Name, name, volume))
			}
		}
		if pct, ok := controller.ArchiveMaxUsagePercentage(cluster, g); ok && pct >= 90 {
			warnings = append(warnings, fmt.Sprintf("node group %q: repositories.contentArchive.maxUsagePercentage %d leaves "+
				"little room for new content; NiFi applies backpressure once the content disk is full", g.Name, pct))
		}
	}
	return warnings
}

// pvcDeleteWarnings names every node group whose PVCs are deleted with it or on scale-down,
// since the data on them can't be recovered afterwards.
func pvcDeleteWarnings(cluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	var warnings admission.Warnings
	for i := range cluster.Spec.NodeGroups {
		g := &cluster.Spec.NodeGroups[i]
		p := controller.EffectivePVCRetention(cluster, g)
		if p.WhenDeleted == nifiv1alpha1.PVCDelete {
			warnings = append(warnings, fmt.Sprintf("node group %q: persistentVolumeClaimRetentionPolicy.whenDeleted is Delete: "+
				"deleting the NiFiCluster or removing this node group deletes its repositories and logs, which can't be recovered", g.Name))
		}
		if p.WhenScaled == nifiv1alpha1.PVCDelete {
			warnings = append(warnings, fmt.Sprintf("node group %q: persistentVolumeClaimRetentionPolicy.whenScaled is Delete: "+
				"a scale-down deletes the removed nodes' repositories and logs after offloading them; a node that cannot be "+
				"confirmed offloaded holds up the scale-down instead of risking its data", g.Name))
		}
	}
	return warnings
}

// tlsDisabledWarnings warns when TLS is off: NiFi then serves plain HTTP, and every login it
// offers (single-user included) still requires HTTPS to issue a token, so no one can actually
// authenticate - every request reaches NiFi as the anonymous user (NIFI-248 A3).
func tlsDisabledWarnings(cluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	if len(cluster.Spec.NodeGroups) == 0 || tlsEnabled(&cluster.Spec.NodeGroups[0]) {
		return nil
	}
	return admission.Warnings{"spec.nodeGroups[].tls.enabled is false: NiFi serves plain HTTP with no way to log in, " +
		"so every request reaches it as the anonymous user; set tls.enabled: true unless this cluster is fully isolated from untrusted traffic"}
}

// coordinationServiceAccountWarnings warns, for Kubernetes coordination, about node groups whose
// pods run as their own ServiceAccount without asking the operator to bind the coordination Role
// to it: those pods cannot elect a coordinator or keep component state until someone grants it.
// The CoordinationRBACReady condition then reports whether it has been granted.
func coordinationServiceAccountWarnings(cluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	if !controller.UsesKubernetesCoordination(cluster) ||
		(cluster.Spec.Coordination != nil && cluster.Spec.Coordination.BindPodServiceAccounts) {
		return nil
	}
	var warnings admission.Warnings
	role := controller.NiFiRoleName(cluster)
	for _, sa := range controller.NodeGroupServiceAccounts(cluster) {
		warnings = append(warnings, fmt.Sprintf(
			"ServiceAccount %q (podOverrides.serviceAccountName) needs the coordination Role to use Leases and ConfigMaps; "+
				"unless you have granted it, run: kubectl -n %s create rolebinding %s-%s --role=%s --serviceaccount=%s:%s "+
				"(or set coordination.bindPodServiceAccounts, if the operator admin allows it). "+
				"The CoordinationRBACReady condition reports whether it is granted.",
			sa, cluster.Namespace, role, sa, role, cluster.Namespace, sa))
	}
	return warnings
}

// validateNodeGroup checks everything scoped to a single node group entry.
func (v *NiFiClusterCustomValidator) validateNodeGroup(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroupPath *field.Path, nodeGroup *nifiv1alpha1.NodeGroup,
) field.ErrorList {
	var errs field.ErrorList

	if _, err := resource.ParseQuantity(nodeGroup.Storage); err != nil {
		errs = append(errs, field.Invalid(nodeGroupPath.Child("storage"), nodeGroup.Storage, err.Error()))
	}
	if nodeGroup.LogStorage != "" {
		if _, err := resource.ParseQuantity(nodeGroup.LogStorage); err != nil {
			errs = append(errs, field.Invalid(nodeGroupPath.Child("logStorage"), nodeGroup.LogStorage, err.Error()))
		}
	}

	errs = append(errs, validateSplitRepositoryStorageSizes(nodeGroupPath.Child("splitRepositoryStorage"), nodeGroup.SplitRepositoryStorage)...)

	// extraSANs go onto every node certificate as DNS names: only plain hostnames, so a wildcard
	// or an IP address (which belongs in an IP SAN) never ends up on a cluster cert.
	if tls := nodeGroup.TLS; tls != nil {
		for i, san := range tls.ExtraSANs {
			sanPath := nodeGroupPath.Child("tls").Child("extraSANs").Index(i)
			if net.ParseIP(san) != nil {
				errs = append(errs, field.Invalid(sanPath, san, "must be a DNS name, not an IP address"))
				continue
			}
			for _, msg := range validation.IsDNS1123Subdomain(san) {
				errs = append(errs, field.Invalid(sanPath, san, msg))
			}
		}
	}

	if pdb := nodeGroup.PodDisruptionBudget; pdb != nil && pdb.MinAvailable != nil {
		if *pdb.MinAvailable < 0 {
			errs = append(errs, field.Invalid(nodeGroupPath.Child("podDisruptionBudget").Child("minAvailable"),
				*pdb.MinAvailable, "must be non-negative"))
		} else if *pdb.MinAvailable >= nodeGroup.Replicas {
			errs = append(errs, field.Invalid(nodeGroupPath.Child("podDisruptionBudget").Child("minAvailable"),
				*pdb.MinAvailable, "must be less than replicas, otherwise no voluntary disruption would ever be allowed"))
		}
	}

	// Kubernetes rejects a pod whose request is above its limit only when the StatefulSet creates
	// it, long after this NiFiCluster was accepted: catch it here, where the user sees it.
	names := make([]string, 0, len(nodeGroup.Resources.Requests))
	for name := range nodeGroup.Resources.Requests {
		names = append(names, string(name))
	}
	sort.Strings(names)
	for _, n := range names {
		name := corev1.ResourceName(n)
		request := nodeGroup.Resources.Requests[name]
		if limit, ok := nodeGroup.Resources.Limits[name]; ok && request.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(nodeGroupPath.Child("resources").Child("requests").Key(string(name)),
				request.String(), fmt.Sprintf("must be less than or equal to the %s limit (%s)", name, limit.String())))
		}
	}

	if err := v.validateStorageClassNotNFSOrCIFS(ctx, nodeGroup.StorageClassName); err != nil {
		errs = append(errs, field.Invalid(nodeGroupPath.Child("storageClassName"),
			ptrString(nodeGroup.StorageClassName), err.Error()))
	}

	errs = append(errs, validateConfigOverrides(nodeGroupPath.Child("configOverrides"), cluster, nodeGroup.ConfigOverrides)...)
	errs = append(errs, validatePodOverrides(nodeGroupPath.Child("podOverrides"), nodeGroup.PodOverrides)...)

	if tls := nodeGroup.TLS; tls != nil && tls.Enabled && tls.CertificateValidityDays != nil {
		days := *tls.CertificateValidityDays
		switch {
		case days <= 0:
			errs = append(errs, field.Invalid(nodeGroupPath.Child("tls").Child("certificateValidityDays"),
				days, "must be a positive number of days"))
		case days > tlsMaxCertificateValidityDays:
			errs = append(errs, field.Invalid(nodeGroupPath.Child("tls").Child("certificateValidityDays"),
				days, fmt.Sprintf(
					"must be at most %d days - a longer validity period defeats the point of automatic "+
						"rotation (a pod would need to run for years before ever renewing its certificate)",
					tlsMaxCertificateValidityDays)))
		}
	}

	return errs
}

// validateUniformTLS requires every node group to use the same TLS setting. All node groups join
// one NiFi cluster, and its cluster protocol is either TLS-secured on every node or on none: a
// node without a keystore cannot talk to a node that requires one.
func validateUniformTLS(path *field.Path, nodeGroups []nifiv1alpha1.NodeGroup) field.ErrorList {
	var errs field.ErrorList
	for i := 1; i < len(nodeGroups); i++ {
		if tlsEnabled(&nodeGroups[i]) != tlsEnabled(&nodeGroups[0]) {
			errs = append(errs, field.Invalid(path.Index(i).Child("tls").Child("enabled"), tlsEnabled(&nodeGroups[i]),
				fmt.Sprintf("must match nodeGroups[0] (%q): all node groups form one NiFi cluster, whose cluster "+
					"protocol is TLS-secured on every node or on none", nodeGroups[0].Name)))
		}
		// All node groups share one CA (tlsCASecretName) and so one keystore password - a
		// per-node-group value here would silently only take effect for whichever node group
		// happens to create the CA Secret first.
		if secretName := tlsKeystorePasswordSecretName(&nodeGroups[i]); secretName != tlsKeystorePasswordSecretName(&nodeGroups[0]) {
			errs = append(errs, field.Invalid(path.Index(i).Child("tls").Child("keystorePasswordSecretName"), secretName,
				fmt.Sprintf("must match nodeGroups[0] (%q): all node groups share one CA and so one keystore password",
					nodeGroups[0].Name)))
		}
	}
	return errs
}

// validateZooKeeper checks spec.coordination.zookeeper beyond what the CRD schema can express:
// every connect-string entry is host:port (a chroot suffix belongs in rootNode, where the
// operator can keep it unique per cluster), and the root node is a znode path ZooKeeper accepts.
func validateZooKeeper(path *field.Path, zk *nifiv1alpha1.ZooKeeperSpec) field.ErrorList {
	var errs field.ErrorList
	for _, server := range strings.Split(zk.ConnectString, ",") {
		if strings.Contains(server, "/") {
			errs = append(errs, field.Invalid(path.Child("connectString"), zk.ConnectString,
				"must not contain a chroot path; set rootNode instead"))
			break
		}
		host, port, found := strings.Cut(strings.TrimSpace(server), ":")
		if !found {
			errs = append(errs, field.Invalid(path.Child("connectString"), zk.ConnectString,
				fmt.Sprintf("entry %q must be host:port", server)))
			continue
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			errs = append(errs, field.Invalid(path.Child("connectString"), zk.ConnectString,
				fmt.Sprintf("entry %q has an invalid port", server)))
		}
		if len(validation.IsDNS1123Subdomain(host)) > 0 && len(validation.IsValidIP(path, host)) > 0 {
			errs = append(errs, field.Invalid(path.Child("connectString"), zk.ConnectString,
				fmt.Sprintf("entry %q has an invalid host", server)))
		}
	}
	if root := zk.RootNode; root != "" {
		if root == "/zookeeper" || strings.HasPrefix(root, "/zookeeper/") {
			errs = append(errs, field.Invalid(path.Child("rootNode"), root, "/zookeeper is reserved by ZooKeeper"))
		}
		for _, segment := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
			if segment == "." || segment == ".." {
				errs = append(errs, field.Invalid(path.Child("rootNode"), root, "path segments \".\" and \"..\" are not allowed"))
				break
			}
		}
	}
	return errs
}

// validateClusterService checks spec.service: source ranges must be CIDRs, and settings that only
// apply to a LoadBalancer are flagged on other Service types.
func validateClusterService(
	path *field.Path, serviceType corev1.ServiceType, svc *nifiv1alpha1.ClusterServiceSpec,
) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings
	for i, cidr := range svc.LoadBalancerSourceRanges {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			errs = append(errs, field.Invalid(path.Child("loadBalancerSourceRanges").Index(i), cidr, "must be a CIDR such as 203.0.113.0/24"))
		}
	}
	if serviceType != corev1.ServiceTypeLoadBalancer && (svc.LoadBalancerClass != nil || len(svc.LoadBalancerSourceRanges) > 0) {
		warnings = append(warnings, "spec.service: loadBalancerClass and loadBalancerSourceRanges only apply when serviceType is LoadBalancer; they are ignored")
	}
	if serviceType != corev1.ServiceTypeLoadBalancer && svc.ExternalTrafficPolicy != "" {
		warnings = append(warnings, "spec.service.externalTrafficPolicy only applies to LoadBalancer; it is ignored")
	}
	if serviceType == corev1.ServiceTypeLoadBalancer && len(svc.LoadBalancerSourceRanges) == 0 {
		warnings = append(warnings, "spec.service.loadBalancerSourceRanges is empty: the LoadBalancer accepts connections from any address")
	}
	return errs, warnings
}

// validateLoadBalancerTLS rejects serviceType LoadBalancer on a cluster without TLS, which would
// serve NiFi's UI and login over plain HTTP outside the cluster. Both clusters are merged views;
// oldCluster is nil on create. Checked on create and when serviceType or TLS changes, so an
// unrelated edit of a cluster stored that way still goes through; never on a cluster being deleted.
func validateLoadBalancerTLS(path *field.Path, oldCluster, cluster *nifiv1alpha1.NiFiCluster) field.ErrorList {
	if cluster.DeletionTimestamp != nil || cluster.Spec.ServiceType != corev1.ServiceTypeLoadBalancer || clusterTLS(cluster) {
		return nil
	}
	// A cluster that had no node group yet ran nothing: its first one counts as a create.
	if oldCluster != nil && len(oldCluster.Spec.NodeGroups) > 0 &&
		oldCluster.Spec.ServiceType == cluster.Spec.ServiceType && clusterTLS(oldCluster) == clusterTLS(cluster) {
		return nil
	}
	return field.ErrorList{field.Forbidden(path, "serviceType LoadBalancer needs TLS on the node groups "+
		"(tls.enabled: true): without it NiFi's UI and login are served over plain HTTP outside the cluster. "+
		"Turn TLS on, or use ClusterIP")}
}

// clusterTLS reports whether the cluster's node groups run TLS (they all agree; see validateUniformTLS).
func clusterTLS(cluster *nifiv1alpha1.NiFiCluster) bool {
	return len(cluster.Spec.NodeGroups) > 0 && tlsEnabled(&cluster.Spec.NodeGroups[0])
}

// validateNodeLimit rejects a change that takes a cluster past MaxClusterNodes NiFi nodes, or
// further past it. Both clusters are merged views (their NiFiNodeGroup objects count too); oldCluster
// is nil on create. A cluster being deleted is never rejected.
func validateNodeLimit(path *field.Path, oldCluster, cluster *nifiv1alpha1.NiFiCluster) field.ErrorList {
	if cluster.DeletionTimestamp != nil {
		return nil
	}
	total := totalReplicas(cluster.Spec.NodeGroups)
	if oldCluster != nil && total <= totalReplicas(oldCluster.Spec.NodeGroups) {
		return nil
	}
	if total > nifiv1alpha1.MaxClusterNodes {
		return field.ErrorList{field.Invalid(path, total, fmt.Sprintf(
			"NiFiCluster %q would have %d NiFi nodes; at most %d are allowed in total, across all node groups "+
				"and NiFiNodeGroups", cluster.Name, total, nifiv1alpha1.MaxClusterNodes))}
	}
	return nil
}

func totalReplicas(nodeGroups []nifiv1alpha1.NodeGroup) int32 {
	var total int32
	for i := range nodeGroups {
		total += nodeGroups[i].Replicas
	}
	return total
}

func tlsEnabled(nodeGroup *nifiv1alpha1.NodeGroup) bool {
	return nodeGroup.TLS != nil && nodeGroup.TLS.Enabled
}

func tlsKeystorePasswordSecretName(nodeGroup *nifiv1alpha1.NodeGroup) string {
	if nodeGroup.TLS == nil {
		return ""
	}
	return nodeGroup.TLS.KeystorePasswordSecretName
}

// validateConfigOverrides rejects any NifiProperties key the operator manages itself (see
// controller.ReservedNifiPropertyKeys) - a shared list with buildNifiProperties, so a key added
// there is automatically protected here too - and, in both NifiProperties and BootstrapConf, any
// key or value that would not survive as one literal Java Properties entry (see
// controller.OverrideKeyError/OverrideValueError): a line break in a value, or an escape or
// separator in a key, would otherwise smuggle a reserved key or a java.arg past this check.
// Sorted before iterating so multiple rejected keys come back in a stable, deterministic order.
func validateConfigOverrides(
	path *field.Path, cluster *nifiv1alpha1.NiFiCluster, overrides *nifiv1alpha1.ConfigOverridesSpec,
) field.ErrorList {
	if overrides == nil {
		return nil
	}
	var errs field.ErrorList

	for _, k := range sortedKeys(overrides.NifiProperties) {
		keyPath := path.Child("nifiProperties").Key(k)
		switch {
		case controller.TypedRepositoryKey(cluster, k):
			errs = append(errs, field.Forbidden(keyPath, "this property is set through spec.repositories (or a node group's "+
				"repositories); set it in one place only"))
		case controller.IsReservedNifiPropertyKey(cluster, k):
			errs = append(errs, field.Forbidden(keyPath, "this property is managed by the operator and cannot be overridden"))
		}
		errs = append(errs, validateOverrideEntry(keyPath, k, overrides.NifiProperties[k])...)
	}
	// BootstrapConf has no reserved keys (the operator doesn't set anything there itself).
	for _, k := range sortedKeys(overrides.BootstrapConf) {
		errs = append(errs, validateOverrideEntry(path.Child("bootstrapConf").Key(k), k, overrides.BootstrapConf[k])...)
	}

	// Logback/Authorizers are full-file replacements, not key=value merges (see their own doc
	// comments) - an explicitly-set-but-empty value would replace a working config file with
	// nothing, which NiFi can't boot from, so that's rejected here; the XML's own correctness
	// beyond that is not validated.
	if overrides.Logback != nil && *overrides.Logback == "" {
		errs = append(errs, field.Invalid(path.Child("logback"), "", "must not be empty when set"))
	}
	if overrides.Authorizers != nil && *overrides.Authorizers == "" {
		errs = append(errs, field.Invalid(path.Child("authorizers"), "", "must not be empty when set"))
	}

	return errs
}

// validateOverrideEntry checks one override key/value pair against the Java Properties rules in
// controller.OverrideKeyError/OverrideValueError.
func validateOverrideEntry(keyPath *field.Path, key, value string) field.ErrorList {
	var errs field.ErrorList
	if msg := controller.OverrideKeyError(key); msg != "" {
		errs = append(errs, field.Invalid(keyPath, key, "key "+msg))
	}
	if msg := controller.OverrideValueError(value); msg != "" {
		errs = append(errs, field.Invalid(keyPath, value, "value "+msg))
	}
	return errs
}

// sortedKeys returns m's keys in sorted order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validatePodOverrides rejects anything in PodOverrides that would collide with something the
// operator manages itself: an Env name matching controller.ReservedPodEnvNames, an
// InitContainer/Sidecar named the same as the operator's own main container, or a Volume named
// the same as one of the operator's own volumes (see controller.ReservedVolumeNames) - the same
// reserved-set pattern (and same shared-source-of-truth reasoning) as validateConfigOverrides.
func validatePodOverrides(path *field.Path, overrides *nifiv1alpha1.PodOverridesSpec) field.ErrorList {
	if overrides == nil {
		return nil
	}
	var errs field.ErrorList

	for i, e := range overrides.Env {
		if _, reserved := controller.ReservedPodEnvNames[e.Name]; reserved {
			errs = append(errs, field.Forbidden(path.Child("env").Index(i).Child("name"),
				"this environment variable is set by the operator and cannot be overridden"))
		}
	}
	for i, c := range overrides.InitContainers {
		if c.Name == controller.NifiContainerName || c.Name == controller.InitContainerName {
			errs = append(errs, field.Forbidden(path.Child("initContainers").Index(i).Child("name"),
				fmt.Sprintf("%q is the name of one of the operator's own containers and cannot be reused", c.Name)))
		}
	}
	for i, c := range overrides.Sidecars {
		if c.Name == controller.NifiContainerName || c.Name == controller.InitContainerName {
			errs = append(errs, field.Forbidden(path.Child("sidecars").Index(i).Child("name"),
				fmt.Sprintf("%q is the name of one of the operator's own containers and cannot be reused", c.Name)))
		}
	}
	for i, v := range overrides.Volumes {
		if _, reserved := controller.ReservedVolumeNames[v.Name]; reserved {
			errs = append(errs, field.Forbidden(path.Child("volumes").Index(i).Child("name"),
				"this volume name is managed by the operator and cannot be reused"))
		}
	}
	return errs
}

// validateSplitRepositoryStorageSizes checks each of SplitRepositoryStorageSpec's optional size
// fields as a valid resource.Quantity when set - the same guard-if-set-then-ParseQuantity pattern
// already used for NodeGroup.Storage/LogStorage above. An unset field is left alone here (it
// falls back to an internal default in repositoryVolumeClaimTemplates), so only fields the user
// actually provided a value for are validated.
func validateSplitRepositoryStorageSizes(path *field.Path, split *nifiv1alpha1.SplitRepositoryStorageSpec) field.ErrorList {
	if split == nil {
		return nil
	}
	var errs field.ErrorList
	sizes := []struct {
		field string
		value string
	}{
		{"flowFileRepositoryStorage", split.FlowFileRepositoryStorage},
		{"contentRepositoryStorage", split.ContentRepositoryStorage},
		{"provenanceRepositoryStorage", split.ProvenanceRepositoryStorage},
		{"databaseRepositoryStorage", split.DatabaseRepositoryStorage},
		{"extensionsStorage", split.ExtensionsStorage},
	}
	for _, s := range sizes {
		if s.value == "" {
			continue
		}
		if _, err := resource.ParseQuantity(s.value); err != nil {
			errs = append(errs, field.Invalid(path.Child(s.field), s.value, err.Error()))
		}
	}
	return errs
}

// validateProxyHost checks that host is a plausible "hostname" or "hostname:port" value, suitable
// for NiFi's nifi.web.proxy.host allow-list. Accepts an optional ":port" suffix since that's a
// valid form for the Host header NiFi actually compares against (e.g. a load balancer's IP:port).
func validateProxyHost(host string) error {
	h := host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		h = host[:idx]
		if _, err := strconv.Atoi(host[idx+1:]); err != nil {
			return fmt.Errorf("invalid port in %q", host)
		}
	}
	if errs := validation.IsDNS1123Subdomain(h); len(errs) > 0 {
		return fmt.Errorf("invalid hostname %q: %s", h, strings.Join(errs, "; "))
	}
	return nil
}

// validateSingleUserAuth enforces that exactly one of (Username+Password) or SecretName is set -
// the CRD schema alone can't express "exactly one of A or B", so this is a webhook-only check -
// and that the password meets NiFi's minimum length in either mode. When SecretName is used and
// the client is available, also checks the Secret actually has non-empty "username"/"password"
// keys, so a typo'd or malformed Secret is caught here rather than surfacing later as a
// confusing CreateContainerConfigError at pod creation. A Secret that doesn't exist yet is only
// a warning: GitOps tools and secret syncers (External Secrets, Vault) routinely create it after
// or alongside the NiFiCluster.
func (v *NiFiClusterCustomValidator) validateSingleUserAuth(
	ctx context.Context, path *field.Path, namespace string, auth *nifiv1alpha1.SingleUserAuthSpec,
) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings

	hasLiteral := auth.Username != "" || auth.Password != ""
	hasSecretRef := auth.SecretName != ""

	switch {
	case hasLiteral && hasSecretRef:
		errs = append(errs, field.Invalid(path, "",
			"exactly one of username/password or secretName must be set, not both"))
		return errs, nil
	case !hasLiteral && !hasSecretRef:
		errs = append(errs, field.Invalid(path, "",
			"exactly one of username/password or secretName must be set"))
		return errs, nil
	}

	if hasLiteral {
		// Anyone who can read the NiFiCluster (the viewer role included) can read this password.
		warnings = append(warnings, "singleUser.username/password are stored in plain text in the NiFiCluster, "+
			"readable by anyone who can get it; use singleUser.secretName for anything beyond development")
		if auth.Username == "" {
			errs = append(errs, field.Required(path.Child("username"), "required when password is set"))
		}
		if auth.Password == "" {
			errs = append(errs, field.Required(path.Child("password"), "required when username is set"))
		} else if len(auth.Password) < singleUserMinPasswordLength {
			// Never echo the password itself back in the error.
			errs = append(errs, field.Invalid(path.Child("password"), "(redacted)", passwordTooShortMsg))
		} else if len(auth.Password) > singleUserMaxPasswordBytes {
			errs = append(errs, field.Invalid(path.Child("password"), "(redacted)", passwordTooLongMsg))
		}
		return errs, warnings
	}

	if v.Client == nil {
		return errs, nil
	}
	// The checks below reveal whether the Secret exists, which keys it has and how long the
	// password is. Only run them for a requester who may read the Secret anyway, so the webhook
	// is no oracle for Secrets they can't see; authorizeExposure denies the rest.
	if !v.requesterMayGetSecret(ctx, namespace, auth.SecretName) {
		return errs, nil
	}
	reader := v.Reader
	if reader == nil {
		reader = v.Client
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Name: auth.SecretName, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return errs, admission.Warnings{fmt.Sprintf(
				"authentication Secret %q not found in namespace %q; NiFi pods will not start until it exists",
				auth.SecretName, namespace)}
		}
		nificlusterlog.Error(err, "unable to look up authentication Secret for validation; skipping key check",
			"secretName", auth.SecretName)
		return errs, nil
	}
	if len(secret.Data["username"]) == 0 {
		errs = append(errs, field.Invalid(path.Child("secretName"), auth.SecretName,
			`referenced Secret has no non-empty "username" key`))
	}
	if len(secret.Data["password"]) == 0 {
		errs = append(errs, field.Invalid(path.Child("secretName"), auth.SecretName,
			`referenced Secret has no non-empty "password" key`))
	} else if len(secret.Data["password"]) < singleUserMinPasswordLength {
		errs = append(errs, field.Invalid(path.Child("secretName"), auth.SecretName,
			`referenced Secret's "password" key: `+passwordTooShortMsg))
	} else if len(secret.Data["password"]) > singleUserMaxPasswordBytes {
		errs = append(errs, field.Invalid(path.Child("secretName"), auth.SecretName,
			`referenced Secret's "password" key: `+passwordTooLongMsg))
	}
	return errs, nil
}

// singleUserMinPasswordLength is NiFi's own minimum for single-user login passwords: shorter ones
// make `nifi.sh set-single-user-credentials` fail, and with it the node container's entrypoint.
const singleUserMinPasswordLength = 12

var passwordTooShortMsg = fmt.Sprintf("must be at least %d characters (NiFi single-user login minimum)",
	singleUserMinPasswordLength)

// singleUserMaxPasswordBytes is bcrypt's limit: NiFi refuses to hash a longer password (checked
// against NiFi 2.12), and so does the operator.
const singleUserMaxPasswordBytes = 72

var passwordTooLongMsg = fmt.Sprintf("must be at most %d bytes (bcrypt, which NiFi stores the login with)",
	singleUserMaxPasswordBytes)

// validateStorageClassNotNFSOrCIFS rejects a NodeGroup's StorageClassName if that StorageClass's
// provisioner is known to back NFS/CIFS storage (Feature 8: PVC-only, no NFS/CIFS).
func (v *NiFiClusterCustomValidator) validateStorageClassNotNFSOrCIFS(ctx context.Context, storageClassName *string) error {
	if storageClassName == nil || *storageClassName == "" || v.Client == nil {
		return nil
	}

	sc := &storagev1.StorageClass{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: *storageClassName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // let PVC provisioning fail on its own if the StorageClass never shows up
		}
		nificlusterlog.Error(err, "unable to look up StorageClass for validation; skipping NFS/CIFS check",
			"storageClassName", *storageClassName)
		return nil
	}

	provisioner := strings.ToLower(sc.Provisioner)
	for _, banned := range nfsCIFSProvisioners {
		if strings.Contains(provisioner, banned) {
			return fmt.Errorf("StorageClass %q uses provisioner %q, which appears to be NFS/CIFS-backed; "+
				"NFS/CIFS storage backends are not supported (PVC-only storage)", *storageClassName, sc.Provisioner)
		}
	}
	return nil
}

// ptrString safely dereferences a *string for use in a field.ErrorList entry.
func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type NiFiCluster.
func (v *NiFiClusterCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	nificluster, ok := obj.(*nifiv1alpha1.NiFiCluster)
	if !ok {
		return nil, fmt.Errorf("expected a NiFiCluster object but got %T", obj)
	}
	nificlusterlog.Info("Validation for NiFiCluster upon deletion", "name", nificluster.GetName())

	// No validation needed on delete - nothing about deleting a NiFiCluster can be invalid.
	return nil, nil
}

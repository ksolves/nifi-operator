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

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// openShiftSCCClusterRolePrefix is the prefix of the ClusterRoles OpenShift ships for every
// built-in SecurityContextConstraints (e.g. system:openshift:scc:anyuid), each granting "use" on
// that one SCC.
const openShiftSCCClusterRolePrefix = "system:openshift:scc:"

// verbGet is the RBAC "get" verb.
const verbGet = "get"

// nifiPodServiceAccount returns the ServiceAccount the cluster's NiFi pods run as: the user's own
// spec.serviceAccountName when set, otherwise the one the operator creates (see
// buildNiFiServiceAccount).
func nifiPodServiceAccount(cluster *nifiv1alpha1.NiFiCluster) string {
	if cluster.Spec.ServiceAccountName != "" {
		return cluster.Spec.ServiceAccountName
	}
	return nifiServiceAccountName(cluster)
}

// buildNiFiServiceAccount renders the dedicated ServiceAccount for the cluster's NiFi pods, used
// only when spec.serviceAccountName is unset. Its token stays auto-mounted: NiFi's Kubernetes
// client reads both its credentials and its own namespace from the mounted token directory.
func buildNiFiServiceAccount(cluster *nifiv1alpha1.NiFiCluster) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nifiServiceAccountName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
	}
}

// nifiCoordinationRules are the permissions NiFi's Kubernetes coordination needs in its own
// namespace, per the NiFi Administration Guide ("Kubernetes Clustering"):
//   - Leases: leader election for the Cluster Coordinator and Primary Node roles. "patch" is not
//     in the guide but is required in practice (Stackable grants it for the same reason).
//   - ConfigMaps: the KubernetesConfigMapStateProvider stores each component's cluster state in
//     its own ConfigMap, with names the operator does not control, so - unlike the CA Secret's
//     `get` (ResourceNames: one name) - this grant can't be scoped to one cluster's own
//     ConfigMaps: one NiFiCluster's pod can read/write another's in the same namespace (NIFI-248
//     B2; see docs/security.md's "Known limitations"). The entrypoint script - the one file this
//     would let a compromised pod rewrite into running its own code - is kept out of ConfigMaps
//     entirely for exactly this reason (entrypointSecretName).
//
// The operator itself must hold every one of these permissions to be allowed to grant them
// (Kubernetes RBAC escalation prevention) - see the kubebuilder:rbac markers in
// nificluster_controller.go.
func nifiCoordinationRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"create", verbGet, "update", "patch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"create", "delete", verbGet, "list", "patch", "update"},
		},
	}
}

// buildNiFiRole renders the namespaced Role granting nifiCoordinationRules.
func buildNiFiRole(cluster *nifiv1alpha1.NiFiCluster) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nifiRoleName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		Rules: nifiCoordinationRules(),
	}
}

// nodeGroupServiceAccounts returns, sorted, the distinct podOverrides.serviceAccountName values
// of the cluster's node groups that differ from the cluster's own pod ServiceAccount.
func nodeGroupServiceAccounts(cluster *nifiv1alpha1.NiFiCluster) []string {
	seen := map[string]bool{nifiPodServiceAccount(cluster): true}
	var out []string
	for i := range cluster.Spec.NodeGroups {
		o := cluster.Spec.NodeGroups[i].PodOverrides
		if o == nil || o.ServiceAccountName == "" || seen[o.ServiceAccountName] {
			continue
		}
		seen[o.ServiceAccountName] = true
		out = append(out, o.ServiceAccountName)
	}
	sort.Strings(out)
	return out
}

// bindsNodeGroupServiceAccounts reports whether the coordination Role is also bound to the node
// groups' own ServiceAccounts: only when both the operator admin
// (--allow-custom-service-account-binding) and the cluster (coordination.bindPodServiceAccounts)
// opt in.
func (r *NiFiClusterReconciler) bindsNodeGroupServiceAccounts(cluster *nifiv1alpha1.NiFiCluster) bool {
	c := cluster.Spec.Coordination
	return r.AllowCustomServiceAccountBinding && c != nil && c.BindPodServiceAccounts
}

// buildNiFiRoleBinding binds buildNiFiRole to the NiFi pods' ServiceAccount, and with
// withNodeGroups to every node group's own ServiceAccount too.
func buildNiFiRoleBinding(cluster *nifiv1alpha1.NiFiCluster, withNodeGroups bool) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nifiRoleName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     nifiRoleName(cluster),
		},
		Subjects: serviceAccountSubjects(cluster, withNodeGroups),
	}
}

// serviceAccountSubjects lists the RoleBinding subjects for the cluster's pod ServiceAccount and,
// with withNodeGroups, the node groups' own.
func serviceAccountSubjects(cluster *nifiv1alpha1.NiFiCluster, withNodeGroups bool) []rbacv1.Subject {
	names := []string{nifiPodServiceAccount(cluster)}
	if withNodeGroups {
		names = append(names, nodeGroupServiceAccounts(cluster)...)
	}
	subjects := make([]rbacv1.Subject, 0, len(names))
	for _, n := range names {
		subjects = append(subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: n, Namespace: cluster.Namespace})
	}
	return subjects
}

// buildCSICARole renders the Role granting `get` on exactly this cluster's CA Secret. RBAC
// resourceNames can't match a name pattern, so a per-cluster Role is what keeps the CSI driver
// from needing `get` on every Secret in the cluster. The operator holds Secret `get` itself, so
// it is allowed to grant it.
func buildCSICARole(cluster *nifiv1alpha1.NiFiCluster) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:        csiCAReaderName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: []string{tlsCASecretName(cluster)},
			Verbs:         []string{verbGet},
		}},
	}
}

// buildCSICARoleBinding binds buildCSICARole to the CSI driver's ServiceAccount, which lives in
// the operator's namespace, not the cluster's.
func buildCSICARoleBinding(cluster *nifiv1alpha1.NiFiCluster, csiSA types.NamespacedName) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        csiCAReaderName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     csiCAReaderName(cluster),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      csiSA.Name,
			Namespace: csiSA.Namespace,
		}},
	}
}

// reconcileCSICAAccess grants the CSI driver read access to this cluster's CA Secret while any
// node group uses TLS, and removes the grant otherwise. Both objects are owned by the NiFiCluster
// and garbage-collected with it.
func (r *NiFiClusterReconciler) reconcileCSICAAccess(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	if r.CSIDriverServiceAccount.Name == "" || !clusterNeedsTLS(cluster) {
		meta := metav1.ObjectMeta{Name: csiCAReaderName(cluster), Namespace: cluster.Namespace}
		if err := r.deleteOwned(ctx, cluster, &rbacv1.RoleBinding{ObjectMeta: meta}); err != nil {
			return fmt.Errorf("deleting unused CSI CA RoleBinding: %w", err)
		}
		if err := r.deleteOwned(ctx, cluster, &rbacv1.Role{ObjectMeta: meta}); err != nil {
			return fmt.Errorf("deleting unused CSI CA Role: %w", err)
		}
		return nil
	}

	role := buildCSICARole(cluster)
	if err := r.createOrUpdateOwned(ctx, cluster, role, func(existing client.Object) {
		existing.SetLabels(role.Labels)
		existing.SetAnnotations(role.Annotations)
		existing.(*rbacv1.Role).Rules = role.Rules
	}); err != nil {
		return fmt.Errorf("reconciling CSI CA Role: %w", err)
	}
	return r.reconcileRoleBinding(ctx, cluster, buildCSICARoleBinding(cluster, r.CSIDriverServiceAccount))
}

// buildNiFiSCCRoleBinding binds the NiFi pods' ServiceAccount to OpenShift's built-in ClusterRole
// for the named SecurityContextConstraints, so the pods are admitted under it rather than
// restricted-v2 (which rejects the pod's fixed fsGroup). The operator must itself hold "use" on
// that SCC to be allowed to create this binding.
func buildNiFiSCCRoleBinding(cluster *nifiv1alpha1.NiFiCluster, sccName string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nifiSCCRoleBindingName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     openShiftSCCClusterRolePrefix + sccName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      nifiPodServiceAccount(cluster),
			Namespace: cluster.Namespace,
		}},
	}
}

// reconcileNiFiRBAC ensures the NiFi pods' ServiceAccount (unless the user supplies their own),
// the coordination Role and its RoleBinding, and - on OpenShift with an SCC configured - the SCC
// RoleBinding. All of them are owned by the NiFiCluster and garbage-collected with it.
func (r *NiFiClusterReconciler) reconcileNiFiRBAC(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	if cluster.Spec.ServiceAccountName == "" {
		sa := buildNiFiServiceAccount(cluster)
		if err := r.createOrUpdateOwned(ctx, cluster, sa, func(existing client.Object) {
			existing.SetLabels(sa.Labels)
			existing.SetAnnotations(sa.Annotations)
		}); err != nil {
			return fmt.Errorf("reconciling ServiceAccount: %w", err)
		}
	} else if err := r.deleteOwned(ctx, cluster, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: nifiServiceAccountName(cluster), Namespace: cluster.Namespace},
	}); err != nil {
		return fmt.Errorf("deleting unused ServiceAccount: %w", err)
	}

	// Keep the coordination Role while any StatefulSet still runs a Kubernetes-coordinated pod
	// template, not only while spec asks for Kubernetes: a switch to ZooKeeper waits for its
	// migration approval with those pods still running, and they still renew Leases and write
	// component state until the migration actually replaces them.
	stillUsed, err := r.kubernetesCoordinationStillRunning(ctx, cluster)
	if err != nil {
		return err
	}
	if needsKubernetesAPI(cluster) || stillUsed {
		role := buildNiFiRole(cluster)
		if err := r.createOrUpdateOwned(ctx, cluster, role, func(existing client.Object) {
			existing.SetLabels(role.Labels)
			existing.SetAnnotations(role.Annotations)
			existing.(*rbacv1.Role).Rules = role.Rules
		}); err != nil {
			return fmt.Errorf("reconciling Role: %w", err)
		}
		if err := r.reconcileRoleBinding(ctx, cluster, buildNiFiRoleBinding(cluster, r.bindsNodeGroupServiceAccounts(cluster))); err != nil {
			return err
		}
	} else {
		// No Kubernetes API access for NiFi pods coordinated through ZooKeeper (least privilege).
		meta := metav1.ObjectMeta{Name: nifiRoleName(cluster), Namespace: cluster.Namespace}
		if err := r.deleteOwned(ctx, cluster, &rbacv1.RoleBinding{ObjectMeta: meta}); err != nil {
			return fmt.Errorf("deleting unused RoleBinding: %w", err)
		}
		if err := r.deleteOwned(ctx, cluster, &rbacv1.Role{ObjectMeta: meta}); err != nil {
			return fmt.Errorf("deleting unused Role: %w", err)
		}
	}

	sccBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: nifiSCCRoleBindingName(cluster), Namespace: cluster.Namespace},
	}
	// Only the operator-managed ServiceAccount is ever bound to the SCC. Binding a user-supplied
	// spec.serviceAccountName would let anyone allowed to create a NiFiCluster grant the SCC to
	// any ServiceAccount in the namespace (e.g. another workload's), using the operator's own
	// "use" permission - a confused-deputy escalation. A user-supplied ServiceAccount needs its
	// SCC granted by whoever administers the namespace.
	if r.NiFiSCC == "" || cluster.Spec.ServiceAccountName != "" {
		if err := r.deleteOwned(ctx, cluster, sccBinding); err != nil {
			return fmt.Errorf("deleting unused SCC RoleBinding: %w", err)
		}
		return nil
	}
	return r.reconcileRoleBinding(ctx, cluster, buildNiFiSCCRoleBinding(cluster, r.NiFiSCC))
}

// kubernetesCoordinationStillRunning reports whether any of the cluster's StatefulSets was built
// for Kubernetes coordination (see coordinationBackendAnnotation).
func (r *NiFiClusterReconciler) kubernetesCoordinationStillRunning(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (bool, error) {
	list := &appsv1.StatefulSetList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		labelAppManagedBy: managedByValue,
		labelAppInstance:  cluster.Name,
	}); err != nil {
		return false, fmt.Errorf("listing StatefulSets: %w", err)
	}
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], cluster) &&
			list.Items[i].Annotations[coordinationBackendAnnotation] == string(nifiv1alpha1.CoordinationBackendKubernetes) {
			return true, nil
		}
	}
	return false, nil
}

// reconcileRoleBinding creates or updates a RoleBinding. roleRef is immutable, so a binding whose
// roleRef no longer matches (e.g. the configured SCC changed) is deleted and recreated rather than
// updated in place.
func (r *NiFiClusterReconciler) reconcileRoleBinding(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, desired *rbacv1.RoleBinding,
) error {
	existing := &rbacv1.RoleBinding{}
	found, err := r.getOwned(ctx, cluster, client.ObjectKeyFromObject(desired), existing)
	if err != nil {
		return err
	}
	if found && existing.RoleRef != desired.RoleRef {
		if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting RoleBinding %q to change its roleRef: %w", desired.Name, err)
		}
	}

	if err := r.createOrUpdate(ctx, cluster, desired, func(obj client.Object) {
		rb := obj.(*rbacv1.RoleBinding)
		rb.Labels = desired.Labels
		rb.Annotations = desired.Annotations
		rb.RoleRef = desired.RoleRef
		rb.Subjects = desired.Subjects
	}); err != nil {
		return fmt.Errorf("reconciling RoleBinding %q: %w", desired.Name, err)
	}
	return nil
}

// errNotOwned means an object the operator wants to manage already exists but is not controlled
// by this NiFiCluster.
var errNotOwned = errors.New("exists and is not managed by this NiFiCluster")

// errStaleOwner means an object the operator wants to manage is still controlled by a deleted
// NiFiCluster of the same name (a different UID): the cluster was deleted and recreated before
// Kubernetes garbage collection removed the old dependents. It resolves itself once they are
// collected; the reconcile error makes controller-runtime retry with backoff.
var errStaleOwner = errors.New("still belongs to a previous NiFiCluster of the same name; waiting for garbage collection")

// getOwned reads key into obj and reports whether it exists. An object that exists but is not
// controlled by cluster yields errNotOwned: RBAC objects are never adopted, since taking over a
// pre-existing Role or RoleBinding would let whoever created it decide what the NiFi pods (or
// someone else) are granted. Reads go to the cache first; a cache miss is confirmed against the
// API server, because the cache only holds objects carrying the operator's managed-by label (see
// CacheByObject) and an unlabelled object of the same name would otherwise be invisible until
// Create failed with an unhelpful AlreadyExists.
func (r *NiFiClusterReconciler) getOwned(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, key client.ObjectKey, obj client.Object,
) (bool, error) {
	err := r.Get(ctx, key, obj)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, key, obj)
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting %T %q: %w", obj, key.Name, err)
	}
	return true, ownership(cluster, obj)
}

// ownership returns nil when cluster controls obj, errStaleOwner when a deleted NiFiCluster of
// the same name still does, and errNotOwned otherwise.
func ownership(cluster *nifiv1alpha1.NiFiCluster, obj client.Object) error {
	if metav1.IsControlledBy(obj, cluster) {
		return nil
	}
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		kind = fmt.Sprintf("%T", obj)
	}
	if ref := metav1.GetControllerOf(obj); ref != nil && ref.Kind == nifiClusterKind && ref.Name == cluster.Name &&
		ref.APIVersion == nifiv1alpha1.GroupVersion.String() {
		return fmt.Errorf("%s %s/%s %w", kind, obj.GetNamespace(), obj.GetName(), errStaleOwner)
	}
	return fmt.Errorf("%s %s/%s %w; rename or remove it", kind, obj.GetNamespace(), obj.GetName(), errNotOwned)
}

// createOrUpdateOwned is createOrUpdate for objects that must never be adopted: it refuses (with
// errNotOwned) when an object of the same name exists but is not controlled by cluster.
func (r *NiFiClusterReconciler) createOrUpdateOwned(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, obj client.Object, mutate func(client.Object),
) error {
	if _, err := r.getOwned(ctx, cluster, client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object)); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, cluster, obj, mutate)
}

// deleteOwned deletes obj if it exists and is controlled by cluster, leaving anything else of the
// same name (e.g. a user-created object) alone.
func (r *NiFiClusterReconciler) deleteOwned(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, cluster) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

// CacheByObject restricts the manager's informer cache for ServiceAccounts, Roles and
// RoleBindings to the ones this operator manages (managed-by label). The reconciler Owns() them,
// and an unfiltered cache would hold every such object in every watched namespace - in a large
// multi-tenant cluster, a lot of memory for objects the operator never touches. Every
// ServiceAccount, Role and RoleBinding the operator creates carries the label (clusterLabels).
//
// Secrets are cached in every namespace (the operator watches user-referenced ones for rotation),
// but only the operator's own keep their data in memory: foreign Secrets are stored with Data and
// StringData stripped, so a dump of the operator's memory doesn't hold every Secret
// in the cluster. Code that needs a user-referenced Secret's content reads it through APIReader.
func CacheByObject() map[client.Object]cache.ByObject {
	managed := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{labelAppManagedBy: managedByValue})}
	return map[client.Object]cache.ByObject{
		&corev1.ServiceAccount{}: managed,
		&rbacv1.Role{}:           managed,
		&rbacv1.RoleBinding{}:    managed,
		&corev1.Secret{}:         {Transform: stripForeignSecretData},
	}
}

// stripForeignSecretData is the Secret cache transform: Secrets created by the operator (managed-by
// label, or controlled by a NiFiCluster) keep their data; every other Secret is cached as metadata
// only.
func stripForeignSecretData(obj any) (any, error) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return obj, nil
	}
	if secret.Labels[labelAppManagedBy] == managedByValue {
		return secret, nil
	}
	if ref := metav1.GetControllerOf(secret); ref != nil && ref.Kind == nifiClusterKind &&
		strings.HasPrefix(ref.APIVersion, nifiv1alpha1.GroupVersion.Group+"/") {
		return secret, nil
	}
	secret.Data = nil
	secret.StringData = nil
	return secret, nil
}

// secretReader returns the reader for Secrets a user may own (auth secretName, ConfigOverrides
// secretName): the uncached APIReader, since the cache strips their data.
func (r *NiFiClusterReconciler) secretReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// UsesKubernetesCoordination reports whether the cluster's NiFi pods coordinate through Leases and
// ConfigMaps, and so need the coordination Role (for the webhook).
func UsesKubernetesCoordination(cluster *nifiv1alpha1.NiFiCluster) bool {
	return needsKubernetesAPI(cluster)
}

// NodeGroupServiceAccounts is nodeGroupServiceAccounts, for the webhook.
func NodeGroupServiceAccounts(cluster *nifiv1alpha1.NiFiCluster) []string {
	return nodeGroupServiceAccounts(cluster)
}

// NiFiRoleName is the name of the cluster's coordination Role, for the webhook.
func NiFiRoleName(cluster *nifiv1alpha1.NiFiCluster) string {
	return nifiRoleName(cluster)
}

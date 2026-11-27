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
	"maps"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/controller"
)

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// podsResource is the resource of the `create pods` grants: what a field that decides what the NiFi
// pods run needs.
const podsResource = "pods"

// exposureGrant is an action the operator performs on a user's behalf - such as reading a
// ConfigOverrides secretName - which the requesting user must be allowed to perform themselves. verb defaults to "create"; name, when set, scopes the check to one object.
type exposureGrant struct {
	path                                               *field.Path
	verb, group, resource, subresource, name, describe string
}

// authorizeExposure stops the operator from acting as a confused deputy. It holds permissions
// (reading Secrets, shaping and running pods, binding Roles) that a user able to create a
// NiFiCluster may lack; so when a field that makes the operator use them is added or changed, the
// requesting user must pass a SubjectAccessReview for the same action in the NiFiCluster's
// namespace.
//
// Checked only for admission requests (the request carries the user); unchanged fields are not
// re-checked, so editing an unrelated field never fails because of who edits it.
func (v *NiFiClusterCustomValidator) authorizeExposure(
	ctx context.Context, oldCluster, cluster *nifiv1alpha1.NiFiCluster,
) field.ErrorList {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || v.Client == nil {
		return nil
	}

	grants := configOverrideSecretGrants(oldCluster, cluster)
	grants = append(grants, podShapingGrants(oldCluster, cluster)...)
	grants = append(grants, imageGrants(oldCluster, cluster, v.TrustedImageRepositories)...)
	grants = append(grants, bootstrapConfGrants(oldCluster, cluster)...)
	grants = append(grants, coordinationBindingGrants(oldCluster, cluster)...)

	var errs field.ErrorList
	for _, g := range grants {
		verb := g.verb
		if verb == "" {
			verb = "create"
		}
		extra := map[string]authorizationv1.ExtraValue{}
		for k, val := range req.UserInfo.Extra {
			extra[k] = authorizationv1.ExtraValue(val)
		}
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: req.UserInfo.Username, Groups: req.UserInfo.Groups, UID: req.UserInfo.UID, Extra: extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: cluster.Namespace, Verb: verb, Group: g.group, Resource: g.resource, Subresource: g.subresource, Name: g.name,
			},
		}}
		if err := v.Client.Create(ctx, sar); err != nil {
			// Fail closed: without an answer, the operator must not act on the user's behalf.
			errs = append(errs, field.InternalError(g.path, fmt.Errorf("checking whether you may %s: %w", g.describe, err)))
			continue
		}
		if !sar.Status.Allowed {
			errs = append(errs, field.Forbidden(g.path, fmt.Sprintf(
				"you are not allowed to %s in namespace %q, so the operator will not do it for you", g.describe, cluster.Namespace)))
		}
	}
	return errs
}

// configOverrideSecretGrants returns a `get` grant for every ConfigOverrides secretName, and the
// single-user and TLS keystore password Secret names, that is new or changed. The operator reads that Secret and hands its values to NiFi, so without this
// check anyone allowed to edit a NiFiCluster could read any Secret in the namespace through the
// operator.
func configOverrideSecretGrants(oldCluster, cluster *nifiv1alpha1.NiFiCluster) []exposureGrant {
	var grants []exposureGrant
	add := func(path *field.Path, name, oldName string) {
		if name != "" && name != oldName {
			grants = append(grants, exposureGrant{
				path: path, verb: "get", resource: "secrets", name: name,
				describe: fmt.Sprintf("read Secret %q", name),
			})
		}
	}
	oldGroups := map[string]*nifiv1alpha1.ConfigOverridesSpec{}
	var oldClusterOverrides *nifiv1alpha1.ConfigOverridesSpec
	if oldCluster != nil {
		oldClusterOverrides = oldCluster.Spec.ConfigOverrides
		for i := range oldCluster.Spec.NodeGroups {
			oldGroups[oldCluster.Spec.NodeGroups[i].Name] = oldCluster.Spec.NodeGroups[i].ConfigOverrides
		}
	}
	specPath := field.NewPath("spec")
	add(specPath.Child("configOverrides", "secretName"), secretNameOf(cluster.Spec.ConfigOverrides), secretNameOf(oldClusterOverrides))
	// The single-user login Secret is read by the webhook (and its pods) the same way.
	var oldAuth string
	if oldCluster != nil {
		oldAuth = authSecretNameOf(oldCluster)
	}
	add(specPath.Child("authentication", "singleUser", "secretName"), authSecretNameOf(cluster), oldAuth)
	// So is the TLS keystore password Secret, whose password the operator copies into the
	// cluster's CA Secret. It is uniform across node groups (validateUniformTLS) and read from the
	// first one.
	if len(cluster.Spec.NodeGroups) > 0 {
		var oldKeystore string
		if oldCluster != nil && len(oldCluster.Spec.NodeGroups) > 0 {
			oldKeystore = tlsKeystorePasswordSecretName(&oldCluster.Spec.NodeGroups[0])
		}
		add(specPath.Child("nodeGroups").Index(0).Child("tls", "keystorePasswordSecretName"),
			tlsKeystorePasswordSecretName(&cluster.Spec.NodeGroups[0]), oldKeystore)
	}
	for i := range cluster.Spec.NodeGroups {
		ng := &cluster.Spec.NodeGroups[i]
		add(specPath.Child("nodeGroups").Index(i).Child("configOverrides", "secretName"),
			secretNameOf(ng.ConfigOverrides), secretNameOf(oldGroups[ng.Name]))
	}
	return grants
}

// secretNameOf nil-safely reads ConfigOverridesSpec.SecretName.
func secretNameOf(co *nifiv1alpha1.ConfigOverridesSpec) string {
	if co == nil {
		return ""
	}
	return co.SecretName
}

// authSecretNameOf nil-safely reads spec.authentication.singleUser.secretName.
func authSecretNameOf(cluster *nifiv1alpha1.NiFiCluster) string {
	if a := cluster.Spec.Authentication; a != nil && a.SingleUser != nil {
		return a.SingleUser.SecretName
	}
	return ""
}

// requesterMayGetSecret reports whether the user making the admission request may get Secret
// name in namespace. Outside an admission request (or without a client) there is no user to
// check, so it answers true; a failed review answers false.
func (v *NiFiClusterCustomValidator) requesterMayGetSecret(ctx context.Context, namespace, name string) bool {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || v.Client == nil {
		return true
	}
	extra := map[string]authorizationv1.ExtraValue{}
	for k, val := range req.UserInfo.Extra {
		extra[k] = authorizationv1.ExtraValue(val)
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: req.UserInfo.Username, Groups: req.UserInfo.Groups, UID: req.UserInfo.UID, Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: "get", Resource: "secrets", Name: name},
	}}
	if err := v.Client.Create(ctx, sar); err != nil {
		return false
	}
	return sar.Status.Allowed
}

// podShapingGrants returns a `create pods` grant when podOverrides (init containers, sidecars,
// volumes, ServiceAccount, ...) or a ServiceAccount override is new or changed. Through them a
// NiFiCluster editor decides what the StatefulSet controller runs, so without this check anyone
// allowed to edit a NiFiCluster could run arbitrary pods - mount any Secret or hostPath, or use any
// ServiceAccount in the namespace - without being allowed to create pods.
func podShapingGrants(oldCluster, cluster *nifiv1alpha1.NiFiCluster) []exposureGrant {
	oldGroups := map[string]*nifiv1alpha1.NodeGroup{}
	oldSA := ""
	if oldCluster != nil {
		oldSA = oldCluster.Spec.ServiceAccountName
		for i := range oldCluster.Spec.NodeGroups {
			oldGroups[oldCluster.Spec.NodeGroups[i].Name] = &oldCluster.Spec.NodeGroups[i]
		}
	}
	specPath := field.NewPath("spec")
	grant := func(path *field.Path) []exposureGrant {
		return []exposureGrant{{path: path, resource: podsResource, describe: "create pods"}}
	}
	if cluster.Spec.ServiceAccountName != "" && cluster.Spec.ServiceAccountName != oldSA {
		return grant(specPath.Child("serviceAccountName"))
	}
	// The security contexts decide what the NiFi pods may do on the node, like podOverrides.
	var oldPodSC *corev1.PodSecurityContext
	var oldContainerSC *corev1.SecurityContext
	if oldCluster != nil {
		oldPodSC, oldContainerSC = oldCluster.Spec.PodSecurityContext, oldCluster.Spec.ContainerSecurityContext
	}
	if cluster.Spec.PodSecurityContext != nil && !equality.Semantic.DeepEqual(oldPodSC, cluster.Spec.PodSecurityContext) {
		return grant(specPath.Child("podSecurityContext"))
	}
	if cluster.Spec.ContainerSecurityContext != nil &&
		!equality.Semantic.DeepEqual(oldContainerSC, cluster.Spec.ContainerSecurityContext) {
		return grant(specPath.Child("containerSecurityContext"))
	}
	for i := range cluster.Spec.NodeGroups {
		ng := &cluster.Spec.NodeGroups[i]
		if ng.PodOverrides == nil {
			continue
		}
		var oldOverrides *nifiv1alpha1.PodOverridesSpec
		if old := oldGroups[ng.Name]; old != nil {
			oldOverrides = old.PodOverrides
		}
		if !equality.Semantic.DeepEqual(oldOverrides, ng.PodOverrides) {
			return grant(specPath.Child("nodeGroups").Index(i).Child("podOverrides"))
		}
	}
	return nil
}

// imageGrants returns a `create pods` grant when spec.image.repository is new or changed and is
// not one of the operator's trusted image repositories (--trusted-image-repositories). The image
// runs with the cluster's ServiceAccount, sensitive-props key and TLS material, so without this
// check anyone allowed to edit a NiFiCluster could run any code there. A tag change in a
// trusted repository needs no check; with no trusted repositories, every new image does.
func imageGrants(oldCluster, cluster *nifiv1alpha1.NiFiCluster, trusted []string) []exposureGrant {
	repo := cluster.Spec.Image.Repository
	if repo == "" || (oldCluster != nil && oldCluster.Spec.Image.Repository == repo) || trustedImageRepository(repo, trusted) {
		return nil
	}
	return []exposureGrant{{
		path: field.NewPath("spec", "image", "repository"), resource: podsResource,
		describe: "create pods (the image repository is not one the operator trusts)",
	}}
}

// trustedImageRepository reports whether repo is one of trusted, comparing Docker Hub names in
// their short form. An entry ending in "/" trusts every repository under that prefix.
func trustedImageRepository(repo string, trusted []string) bool {
	repo = normalizeImageRepository(repo)
	for _, t := range trusted {
		t = normalizeImageRepository(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if repo == t || (strings.HasSuffix(t, "/") && strings.HasPrefix(repo, t)) {
			return true
		}
	}
	return false
}

// normalizeImageRepository strips the implicit Docker Hub registry and library/ namespace, so
// "docker.io/library/nifi", "index.docker.io/nifi" and "nifi" compare equal. The registry is only
// stripped when what follows cannot itself be read as a registry host, so "docker.io/quay.io/x"
// never turns into "quay.io/x".
func normalizeImageRepository(repo string) string {
	for _, p := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		after, ok := strings.CutPrefix(repo, p)
		if !ok {
			continue
		}
		if first, _, _ := strings.Cut(after, "/"); strings.ContainsAny(first, ".:") || first == "localhost" {
			return repo
		}
		repo = after
		break
	}
	if strings.Count(repo, "/") == 1 {
		return strings.TrimPrefix(repo, "library/")
	}
	return repo
}

// bootstrapConfGrants returns a `create pods` grant when configOverrides.bootstrapConf changes, at
// the cluster or in any node group. bootstrap.conf decides the java binary, the lib directory and
// the JVM arguments (java.arg.*: -javaagent, -XX:OnError, ...), so it runs arbitrary code in the
// NiFi pod just like a sidecar would.
func bootstrapConfGrants(oldCluster, cluster *nifiv1alpha1.NiFiCluster) []exposureGrant {
	bootstrapConf := func(o *nifiv1alpha1.ConfigOverridesSpec) map[string]string {
		if o == nil {
			return nil
		}
		return o.BootstrapConf
	}
	grant := func(path *field.Path) []exposureGrant {
		return []exposureGrant{{path: path.Child("configOverrides", "bootstrapConf"), resource: podsResource, describe: "create pods"}}
	}
	oldGroups := map[string]*nifiv1alpha1.NodeGroup{}
	var oldClusterConf map[string]string
	if oldCluster != nil {
		oldClusterConf = bootstrapConf(oldCluster.Spec.ConfigOverrides)
		for i := range oldCluster.Spec.NodeGroups {
			oldGroups[oldCluster.Spec.NodeGroups[i].Name] = &oldCluster.Spec.NodeGroups[i]
		}
	}
	specPath := field.NewPath("spec")
	if !maps.Equal(oldClusterConf, bootstrapConf(cluster.Spec.ConfigOverrides)) {
		return grant(specPath)
	}
	for i := range cluster.Spec.NodeGroups {
		ng := &cluster.Spec.NodeGroups[i]
		var oldConf map[string]string
		if old := oldGroups[ng.Name]; old != nil {
			oldConf = bootstrapConf(old.ConfigOverrides)
		}
		if !maps.Equal(oldConf, bootstrapConf(ng.ConfigOverrides)) {
			return grant(specPath.Child("nodeGroups").Index(i))
		}
	}
	return nil
}

// coordinationBindingGrants returns `update leases` and `update configmaps` grants when
// coordination.bindPodServiceAccounts asks the operator to bind the coordination Role to node
// groups' own ServiceAccounts - newly, or for a changed set of them. The operator holds
// those permissions, so without this check anyone allowed to edit a NiFiCluster could have it grant
// them to any ServiceAccount in the namespace.
func coordinationBindingGrants(oldCluster, cluster *nifiv1alpha1.NiFiCluster) []exposureGrant {
	c := cluster.Spec.Coordination
	if c == nil || !c.BindPodServiceAccounts || !controller.UsesKubernetesCoordination(cluster) {
		return nil
	}
	sas := controller.NodeGroupServiceAccounts(cluster)
	if len(sas) == 0 {
		return nil
	}
	if oldCluster != nil && oldCluster.Spec.Coordination != nil && oldCluster.Spec.Coordination.BindPodServiceAccounts &&
		equality.Semantic.DeepEqual(controller.NodeGroupServiceAccounts(oldCluster), sas) {
		return nil
	}
	path := field.NewPath("spec", "coordination", "bindPodServiceAccounts")
	return []exposureGrant{
		{path: path, verb: "update", group: "coordination.k8s.io", resource: "leases",
			describe: "update Leases (needed to bind the coordination Role to the node groups' ServiceAccounts)"},
		{path: path, verb: "update", resource: "configmaps",
			describe: "update ConfigMaps (needed to bind the coordination Role to the node groups' ServiceAccounts)"},
	}
}

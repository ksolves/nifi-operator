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
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// SetupNiFiNodeGroupWebhookWithManager registers the webhooks for NiFiNodeGroup.
func SetupNiFiNodeGroupWebhookWithManager(mgr ctrl.Manager, opts WebhookOptions) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&nifiv1alpha1.NiFiNodeGroup{}).
		WithValidator(&NiFiNodeGroupCustomValidator{cluster: newClusterValidator(mgr, opts)}).
		WithDefaulter(&NiFiNodeGroupCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-nifi-ksolves-com-v1alpha1-nifinodegroup,mutating=true,failurePolicy=fail,sideEffects=None,timeoutSeconds=5,groups=nifi.ksolves.com,resources=nifinodegroups,verbs=create;update,versions=v1alpha1,name=mnifinodegroup-v1alpha1.kb.io,admissionReviewVersions=v1

// NiFiNodeGroupCustomDefaulter names the node group after the object and gives it the same
// default resources, and on create the same TLS default, as an inline node group.
type NiFiNodeGroupCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &NiFiNodeGroupCustomDefaulter{}

// Default implements webhook.CustomDefaulter.
func (d *NiFiNodeGroupCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	ng, ok := obj.(*nifiv1alpha1.NiFiNodeGroup)
	if !ok {
		return fmt.Errorf("expected a NiFiNodeGroup object but got %T", obj)
	}
	if ng.Spec.Name == "" {
		ng.Spec.Name = ng.Name
	}
	defaultNodeGroupResources(&ng.Spec.NodeGroup)
	if isCreate(ctx) {
		defaultNodeGroupTLS(&ng.Spec.NodeGroup)
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-nifi-ksolves-com-v1alpha1-nifinodegroup,mutating=false,failurePolicy=fail,sideEffects=None,timeoutSeconds=5,groups=nifi.ksolves.com,resources=nifinodegroups,verbs=create;update,versions=v1alpha1,name=vnifinodegroup-v1alpha1.kb.io,admissionReviewVersions=v1

// NiFiNodeGroupCustomValidator validates a NiFiNodeGroup as a node group of its cluster: the
// NiFiCluster validator runs on the cluster with every node group merged in, so the same per-group
// rules, cross-group rules and requester authorization checks apply as to an inline node group.
// Writes through the /scale subresource bypass admission; the replica bounds there come from the
// CRD schema (minimum 1).
type NiFiNodeGroupCustomValidator struct {
	cluster NiFiClusterCustomValidator
}

var _ webhook.CustomValidator = &NiFiNodeGroupCustomValidator{}

// ValidateCreate implements webhook.CustomValidator. A cluster has one node group in the
// Community edition, so no NiFiNodeGroup can be created; those of earlier versions can still be
// updated, scaled and deleted.
func (v *NiFiNodeGroupCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if _, ok := obj.(*nifiv1alpha1.NiFiNodeGroup); !ok {
		return nil, fmt.Errorf("expected a NiFiNodeGroup object but got %T", obj)
	}
	return nil, field.ErrorList{field.Forbidden(field.NewPath("metadata", "name"),
		"NiFiNodeGroup is not available in the Community edition: a NiFiCluster has one node group, in spec.nodeGroups")}.ToAggregate()
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *NiFiNodeGroupCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	ng, ok := newObj.(*nifiv1alpha1.NiFiNodeGroup)
	if !ok {
		return nil, fmt.Errorf("expected a NiFiNodeGroup object but got %T", newObj)
	}
	old, ok := oldObj.(*nifiv1alpha1.NiFiNodeGroup)
	if !ok {
		return nil, fmt.Errorf("expected a NiFiNodeGroup object but got %T", oldObj)
	}
	return v.validate(ctx, old, ng)
}

// ValidateDelete implements webhook.CustomValidator; deleting a node group is always allowed.
func (v *NiFiNodeGroupCustomValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *NiFiNodeGroupCustomValidator) validate(ctx context.Context, old, ng *nifiv1alpha1.NiFiNodeGroup) (admission.Warnings, error) {
	specPath := field.NewPath("spec")
	cluster := &nifiv1alpha1.NiFiCluster{}
	err := v.cluster.reader().Get(ctx, client.ObjectKey{Namespace: ng.Namespace, Name: ng.Spec.ClusterRef}, cluster)
	if apierrors.IsNotFound(err) {
		// GitOps tools may apply the node group before its cluster: only the node group's own
		// rules can be checked yet. The cluster's webhook checks it against the cluster later.
		cluster = &nifiv1alpha1.NiFiCluster{}
		cluster.Name, cluster.Namespace = ng.Spec.ClusterRef, ng.Namespace
		errs := v.cluster.validateNodeGroup(ctx, cluster, specPath, &ng.Spec.NodeGroup)
		// The permission checks need no cluster, only the namespace, and must run now: the
		// cluster's webhook never re-checks a node group, so a node group applied first would
		// otherwise run its podOverrides or read its Secrets unchecked once the cluster appears.
		var oldCluster *nifiv1alpha1.NiFiCluster
		if old != nil {
			oldCluster = withNodeGroups(cluster, nil, &old.Spec.NodeGroup, old.Name)
		}
		errs = append(errs, nodeGroupPaths(v.cluster.authorizeExposure(ctx, oldCluster, withNodeGroups(cluster, nil, &ng.Spec.NodeGroup, ng.Name)), 0)...)
		return append(admission.Warnings{fmt.Sprintf("NiFiCluster %q does not exist (yet): this node group joins it once it does",
			ng.Spec.ClusterRef)}, nodeGroupSecurityContextWarnings(specPath.Child("podOverrides"), &ng.Spec.NodeGroup)...), errs.ToAggregate()
	}
	if err != nil {
		return nil, fmt.Errorf("reading NiFiCluster %q: %w", ng.Spec.ClusterRef, err)
	}

	for _, g := range cluster.Spec.NodeGroups {
		if g.Name == ng.Name {
			return nil, field.ErrorList{field.Duplicate(field.NewPath("metadata", "name"), ng.Name)}.ToAggregate()
		}
	}
	others, err := v.cluster.nodeGroupObjects(ctx, cluster, ng.Name)
	if err != nil {
		return nil, err
	}

	// The cluster as it would be with this change, and as it is without it.
	newCluster := withNodeGroups(cluster, others, &ng.Spec.NodeGroup, ng.Name)
	var oldCluster *nifiv1alpha1.NiFiCluster
	if old != nil {
		oldCluster = withNodeGroups(cluster, others, &old.Spec.NodeGroup, old.Name)
	} else {
		oldCluster = withNodeGroups(cluster, others, nil, "")
	}

	errs := v.cluster.validateNodeGroup(ctx, newCluster, specPath, &ng.Spec.NodeGroup)
	if len(newCluster.Spec.NodeGroups) > 1 {
		errs = append(errs, nodeGroupPaths(validateUniformTLS(field.NewPath("spec", "nodeGroups"),
			[]nifiv1alpha1.NodeGroup{newCluster.Spec.NodeGroups[0], ng.Spec.NodeGroup}), 1)...)
	}
	if ng.DeletionTimestamp == nil {
		errs = append(errs, validateNodeLimit(specPath.Child("replicas"), oldCluster, newCluster)...)
		errs = append(errs, validateLoadBalancerTLS(specPath.Child("tls", "enabled"), oldCluster, newCluster)...)
	}
	errs = append(errs, nodeGroupPaths(v.cluster.authorizeExposure(ctx, oldCluster, newCluster), len(newCluster.Spec.NodeGroups)-1)...)
	if old != nil {
		errs = append(errs, nodeGroupPaths(v.cluster.validateStorageChanges(ctx, oldCluster, newCluster), len(newCluster.Spec.NodeGroups)-1)...)
		errs = append(errs, nodeGroupPaths(validateSplitRepositoryStorageImmutable(oldCluster, newCluster), len(newCluster.Spec.NodeGroups)-1)...)
	}
	// Only this node group's own PVC policy: the cluster's webhook warns about the others.
	self := newCluster.DeepCopy()
	self.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{ng.Spec.NodeGroup}
	warnings := append(coordinationServiceAccountWarnings(self), pvcDeleteWarnings(self)...)
	warnings = append(warnings, nodeGroupSecurityContextWarnings(specPath.Child("podOverrides"), &ng.Spec.NodeGroup)...)
	return warnings, errs.ToAggregate()
}

// reader is the uncached reader for lookups during admission, falling back to the client.
func (v *NiFiClusterCustomValidator) reader() client.Reader {
	if v.Reader != nil {
		return v.Reader
	}
	return v.Client
}

// nodeGroupObjects returns the NiFiNodeGroups referencing cluster, except the one named skip.
func (v *NiFiClusterCustomValidator) nodeGroupObjects(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, skip string) ([]nifiv1alpha1.NiFiNodeGroup, error) {
	r := v.reader()
	if r == nil {
		return nil, nil
	}
	list := &nifiv1alpha1.NiFiNodeGroupList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing NiFiNodeGroups: %w", err)
	}
	var out []nifiv1alpha1.NiFiNodeGroup
	for i := range list.Items {
		o := list.Items[i]
		if o.Spec.ClusterRef == cluster.Name && o.Name != skip && o.DeletionTimestamp.IsZero() {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// withNodeGroups returns a copy of cluster with the given NiFiNodeGroups, and extra (named name)
// when not nil, appended to its inline node groups - the cluster the controller reconciles.
func withNodeGroups(
	cluster *nifiv1alpha1.NiFiCluster, objects []nifiv1alpha1.NiFiNodeGroup, extra *nifiv1alpha1.NodeGroup, name string,
) *nifiv1alpha1.NiFiCluster {
	out := cluster.DeepCopy()
	for i := range objects {
		g := *objects[i].Spec.NodeGroup.DeepCopy()
		g.Name = objects[i].Name
		out.Spec.NodeGroups = append(out.Spec.NodeGroups, g)
	}
	if extra != nil {
		g := *extra.DeepCopy()
		g.Name = name
		out.Spec.NodeGroups = append(out.Spec.NodeGroups, g)
	}
	return out
}

// nodeGroupPaths rewrites the field paths of errs that point into this NiFiNodeGroup's entry of
// the merged cluster (spec.nodeGroups[index]...) to the NiFiNodeGroup's own spec, so its author
// sees spec.podOverrides, not a list they never wrote.
func nodeGroupPaths(errs field.ErrorList, index int) field.ErrorList {
	prefix := field.NewPath("spec", "nodeGroups").Index(index).String()
	for _, e := range errs {
		if rest, ok := strings.CutPrefix(e.Field, prefix); ok {
			e.Field = "spec" + rest
		}
	}
	return errs
}

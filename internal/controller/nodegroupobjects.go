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
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// NiFiNodeGroup objects are node groups of a NiFiCluster defined as objects of their
// own, so each can be scaled through its /scale subresource. The NiFiCluster reconcile merges them
// with the cluster's inline nodeGroups into one in-memory list and runs unchanged on it; it never
// writes that merged list back to the NiFiCluster's spec.
const (
	nodeGroupReadyConditionType    = "Ready"
	nodeGroupConflictConditionType = "Conflict"
)

// listNodeGroupObjects returns the NiFiNodeGroups referencing cluster, sorted by name. An error
// must stop the reconcile: continuing with a partial list would prune the missing groups'
// StatefulSets.
func (r *NiFiClusterReconciler) listNodeGroupObjects(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) ([]nifiv1alpha1.NiFiNodeGroup, error) {
	list := &nifiv1alpha1.NiFiNodeGroupList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing NiFiNodeGroups: %w", err)
	}
	var out []nifiv1alpha1.NiFiNodeGroup
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef == cluster.Name {
			out = append(out, list.Items[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mergeNodeGroups appends to cluster.Spec.NodeGroups (in memory only) every NiFiNodeGroup that
// is not being deleted and whose name no inline node group already uses, and returns the names of
// those it left out because of such a conflict. A NiFiNodeGroup being deleted is left out, so its
// StatefulSet is pruned like an inline node group removed from the spec.
func mergeNodeGroups(cluster *nifiv1alpha1.NiFiCluster, objects []nifiv1alpha1.NiFiNodeGroup) (conflicts map[string]bool) {
	inline := map[string]bool{}
	for i := range cluster.Spec.NodeGroups {
		inline[cluster.Spec.NodeGroups[i].Name] = true
	}
	conflicts = map[string]bool{}
	merged := append([]nifiv1alpha1.NodeGroup(nil), cluster.Spec.NodeGroups...)
	for i := range objects {
		o := &objects[i]
		if !o.DeletionTimestamp.IsZero() {
			continue
		}
		if inline[o.Name] {
			conflicts[o.Name] = true
			continue
		}
		ng := *o.Spec.NodeGroup.DeepCopy()
		ng.Name = o.Name
		merged = append(merged, ng)
	}
	cluster.Spec.NodeGroups = merged
	return conflicts
}

// nodeGroupSelector is the label selector, in string form, of a node group's pods - the same
// labels as its StatefulSet's selector.
func nodeGroupSelector(cluster *nifiv1alpha1.NiFiCluster, nodeGroup string) string {
	labels := map[string]string{
		labelAppName:     appNameValue,
		labelAppInstance: cluster.Name,
		labelNodeGroup:   nodeGroup,
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// updateClusterStatus writes cluster's status. The API server's answer replaces the object in
// memory, spec included, which would drop the NiFiNodeGroups merged into Spec.NodeGroups; they
// are put back so the rest of the reconcile still sees every node group.
func (r *NiFiClusterReconciler) updateClusterStatus(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	groups := cluster.Spec.NodeGroups
	err := r.Status().Update(ctx, cluster)
	cluster.Spec.NodeGroups = groups
	return err
}

// syncNodeGroupObjects gives every NiFiNodeGroup of cluster an owner reference to it (so deleting
// the cluster deletes its node groups) and writes their status: desired and ready replicas, the
// pod selector for the /scale subresource, and the Ready and Conflict conditions.
func (r *NiFiClusterReconciler) syncNodeGroupObjects(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, objects []nifiv1alpha1.NiFiNodeGroup,
	conflicts map[string]bool, statuses []nifiv1alpha1.NodeGroupStatus,
) error {
	byName := map[string]nifiv1alpha1.NodeGroupStatus{}
	for _, s := range statuses {
		byName[s.Name] = s
	}
	for i := range objects {
		o := &objects[i]
		if !o.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.ensureNodeGroupOwner(ctx, cluster, o); err != nil {
			return err
		}

		status := o.Status.DeepCopy()
		status.Selector = nodeGroupSelector(cluster, o.Name)
		if conflicts[o.Name] {
			status.Replicas, status.ReadyReplicas = 0, 0
			apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type: nodeGroupConflictConditionType, Status: metav1.ConditionTrue, Reason: "InlineNodeGroupExists",
				ObservedGeneration: o.Generation,
				Message: fmt.Sprintf("NiFiCluster %q already has an inline node group named %q; this NiFiNodeGroup is not "+
					"reconciled until one of them is renamed or removed", cluster.Name, o.Name),
			})
			apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type: nodeGroupReadyConditionType, Status: metav1.ConditionFalse, Reason: "Conflict",
				ObservedGeneration: o.Generation, Message: "Not reconciled: see the Conflict condition.",
			})
		} else {
			s := byName[o.Name]
			// Observed, not desired: /scale reads it as the current replica count, which is what an
			// HPA compares its desired count against.
			observed, err := r.observedReplicas(ctx, cluster, o.Name)
			if err != nil {
				return err
			}
			status.Replicas = observed
			status.ReadyReplicas = s.ReadyReplicas
			apimeta.RemoveStatusCondition(&status.Conditions, nodeGroupConflictConditionType)
			ready := metav1.Condition{
				Type: nodeGroupReadyConditionType, Status: metav1.ConditionTrue, Reason: "NodesReady",
				ObservedGeneration: o.Generation,
				Message:            fmt.Sprintf("%d/%d nodes ready", s.ReadyReplicas, o.Spec.Replicas),
			}
			if s.ReadyReplicas < o.Spec.Replicas || observed != o.Spec.Replicas {
				ready.Status, ready.Reason = metav1.ConditionFalse, "NodesNotReady"
			}
			apimeta.SetStatusCondition(&status.Conditions, ready)
		}
		if apiequality.Semantic.DeepEqual(status, &o.Status) {
			continue
		}
		o.Status = *status
		if err := r.Status().Update(ctx, o); err != nil {
			return fmt.Errorf("updating NiFiNodeGroup %q status: %w", o.Name, err)
		}
	}
	return nil
}

// observedReplicas returns how many pods the StatefulSet of cluster's node group group has, or 0
// while it does not exist.
func (r *NiFiClusterReconciler) observedReplicas(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, group string) (int32, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: statefulSetName(cluster, &nifiv1alpha1.NodeGroup{Name: group})}, sts)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("getting StatefulSet of NiFiNodeGroup %q: %w", group, err)
	}
	return sts.Status.Replicas, nil
}

// markNodeGroupsWithoutCluster sets Conflict (ClusterNotFound) on every NiFiNodeGroup whose
// clusterRef names the NiFiCluster cluster, which does not exist: without it such a node group has
// no status at all and nothing says why it never runs. The cluster's next reconcile,
// once it exists, clears the condition.
func (r *NiFiClusterReconciler) markNodeGroupsWithoutCluster(ctx context.Context, cluster types.NamespacedName) error {
	stub := &nifiv1alpha1.NiFiCluster{}
	// The cache may not have seen a just-created cluster yet: ask the API server before claiming
	// it is missing.
	if err := r.APIReader.Get(ctx, cluster, stub); !apierrors.IsNotFound(err) {
		return err
	}
	stub = &nifiv1alpha1.NiFiCluster{}
	stub.Name, stub.Namespace = cluster.Name, cluster.Namespace
	objects, err := r.listNodeGroupObjects(ctx, stub)
	if err != nil {
		return err
	}
	for i := range objects {
		o := &objects[i]
		if !o.DeletionTimestamp.IsZero() {
			continue
		}
		status := o.Status.DeepCopy()
		status.Replicas, status.ReadyReplicas, status.Selector = 0, 0, ""
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: nodeGroupConflictConditionType, Status: metav1.ConditionTrue, Reason: "ClusterNotFound",
			ObservedGeneration: o.Generation,
			Message: fmt.Sprintf("NiFiCluster %q does not exist in namespace %q; this NiFiNodeGroup runs once it does",
				cluster.Name, cluster.Namespace),
		})
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: nodeGroupReadyConditionType, Status: metav1.ConditionFalse, Reason: "Conflict",
			ObservedGeneration: o.Generation, Message: "Not reconciled: see the Conflict condition.",
		})
		if apiequality.Semantic.DeepEqual(status, &o.Status) {
			continue
		}
		o.Status = *status
		if err := r.Status().Update(ctx, o); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("updating NiFiNodeGroup %q status: %w", o.Name, err))
		}
	}
	return nil
}

// ensureNodeGroupOwner makes the NiFiCluster an owner of the NiFiNodeGroup, so Kubernetes deletes
// the node group with its cluster.
func (r *NiFiClusterReconciler) ensureNodeGroupOwner(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, o *nifiv1alpha1.NiFiNodeGroup) error {
	for _, ref := range o.OwnerReferences {
		if ref.UID == cluster.UID {
			return nil
		}
	}
	base := o.DeepCopy()
	if err := controllerutil.SetOwnerReference(cluster, o, r.Scheme); err != nil {
		return err
	}
	if err := r.Patch(ctx, o, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("setting the owner of NiFiNodeGroup %q: %w", o.Name, err)
	}
	return nil
}

// clusterOfNodeGroup maps a NiFiNodeGroup event to its NiFiCluster's reconcile.
func clusterOfNodeGroup(_ context.Context, obj client.Object) []reconcile.Request {
	ng, ok := obj.(*nifiv1alpha1.NiFiNodeGroup)
	if !ok || ng.Spec.ClusterRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ng.Namespace, Name: ng.Spec.ClusterRef}}}
}

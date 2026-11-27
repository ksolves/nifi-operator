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
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// EffectivePVCRetention resolves a node group's PVC retention policy field by field: the node
// group's own value, then the cluster's, then Retain. Retain on both is what every
// earlier operator version hard-coded, so a cluster that sets nothing keeps its data as before.
func EffectivePVCRetention(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) nifiv1alpha1.PVCRetentionPolicy {
	out := nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCRetain, WhenScaled: nifiv1alpha1.PVCRetain}
	for _, p := range []*nifiv1alpha1.PVCRetentionPolicy{cluster.Spec.PersistentVolumeClaimRetentionPolicy, nodeGroup.PersistentVolumeClaimRetentionPolicy} {
		if p == nil {
			continue
		}
		if p.WhenDeleted != "" {
			out.WhenDeleted = p.WhenDeleted
		}
		if p.WhenScaled != "" {
			out.WhenScaled = p.WhenScaled
		}
	}
	return out
}

// statefulSetPVCRetention maps a node group's effective policy onto the StatefulSet field.
func statefulSetPVCRetention(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
	p := EffectivePVCRetention(cluster, nodeGroup)
	toSTS := func(t nifiv1alpha1.PVCRetentionType) appsv1.PersistentVolumeClaimRetentionPolicyType {
		if t == nifiv1alpha1.PVCDelete {
			return appsv1.DeletePersistentVolumeClaimRetentionPolicyType
		}
		return appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	}
	return &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: toSTS(p.WhenDeleted),
		WhenScaled:  toSTS(p.WhenScaled),
	}
}

// keepsDataOnDelete reports whether any PVC of cluster outlives it. Only then is there a flow left
// that the sensitive-props key and NiFi's component state belong to; when every PVC goes with the
// cluster, both go too.
//
// The decision follows the PVCs themselves, not the spec: which PVCs survive is set by
// the policy the live StatefulSets carry, which lags the spec on a not yet reconciled
// cluster; a NiFiNodeGroup being deleted is no longer in the merged spec; and the PVCs of a node
// group removed earlier have no StatefulSet left at all. Kubernetes marks a PVC it will delete
// with its StatefulSet (whenDeleted: Delete) or its Pod (whenScaled: Delete, once scaled away) as
// owner, so a PVC with neither is one that stays. Until the StatefulSet controller has set those
// owners, the PVC counts as staying: an extra kept key is harmless, a lost one is not.
func (r *NiFiClusterReconciler) keepsDataOnDelete(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	// Cached: a stale entry can only miss an owner reference the StatefulSet controller has just
	// set, which keeps the data - never the other way round. reconcileDelete runs every few seconds
	// while the pods terminate, so an uncached namespace-wide list would add up.
	if err := r.List(ctx, pvcs, client.InNamespace(cluster.Namespace)); err != nil {
		return true, fmt.Errorf("listing PVCs: %w", err)
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.DeletionTimestamp == nil && mayBeClusterPVC(cluster, pvc) && !deletedWithWorkload(pvc) {
			return true, nil
		}
	}
	return false, nil
}

// claimTemplateNames are the volumeClaimTemplates a node group's StatefulSet can have.
var claimTemplateNames = []string{
	dataVolumeName, logsVolumeName, flowfileVolumeName, contentVolumeName,
	provenanceVolumeName, databaseVolumeName, extensionsVolumeName,
}

// mayBeClusterPVC reports whether pvc belongs to cluster: labelled by its claim templates, or,
// for claims made before the templates carried labels (operator versions before 0.6.0), named
// <template>-<statefulset>-<ordinal> for one of its current node groups. Only StatefulSet names,
// not a name prefix, which could not tell cluster "prod" from "prod-2"; an unlabelled claim of a
// node group removed before the delete is therefore not counted. A StatefulSet name itself can be
// shared - cluster "a" group "b-c" and cluster "a-b" group "c" - but that only ever keeps a key.
func mayBeClusterPVC(cluster *nifiv1alpha1.NiFiCluster, pvc *corev1.PersistentVolumeClaim) bool {
	if labelledClusterPVC(cluster, pvc) {
		return true
	}
	for i := range cluster.Spec.NodeGroups {
		sts := statefulSetName(cluster, &cluster.Spec.NodeGroups[i])
		for _, template := range claimTemplateNames {
			ordinal, ok := strings.CutPrefix(pvc.Name, template+"-"+sts+"-")
			if ok && ordinalPattern.MatchString(ordinal) {
				return true
			}
		}
	}
	return false
}

// labelledClusterPVC reports whether pvc carries the labels cluster's claim templates set.
func labelledClusterPVC(cluster *nifiv1alpha1.NiFiCluster, pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Labels[labelAppInstance] == cluster.Name && pvc.Labels[labelAppManagedBy] == managedByValue
}

var ordinalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// deletedWithWorkload reports whether Kubernetes will garbage-collect pvc: StatefulSet PVC
// retention makes the StatefulSet or a scaled-away Pod its owner when the policy says Delete.
func deletedWithWorkload(pvc *corev1.PersistentVolumeClaim) bool {
	for _, ref := range pvc.OwnerReferences {
		if ref.APIVersion == "apps/v1" && ref.Kind == "StatefulSet" || ref.APIVersion == "v1" && ref.Kind == "Pod" {
			return true
		}
	}
	return false
}

// componentIDPattern matches a NiFi component ID, a UUID.
var componentIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// deleteComponentState removes the component-state ConfigMaps NiFi wrote for a deleted cluster
// whose PVCs are deleted too. NiFi gives them no owner reference; they are kept on the
// Retain path so a recreated cluster resumes its processors' state (see cleanupCoordinationState).
func (r *NiFiClusterReconciler) deleteComponentState(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	cms := &corev1.ConfigMapList{}
	if err := r.List(ctx, cms, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("listing ConfigMaps: %w", err)
	}
	prefix := leaseNamePrefix(cluster) + "-nifi-component-"
	for i := range cms.Items {
		// The rest must be the component's UUID: cluster "foo" must not take
		// "foo-nifi-component-x-nifi-component-<id>" of cluster "foo-nifi-component-x".
		id, ok := strings.CutPrefix(cms.Items[i].Name, prefix)
		if !ok || !componentIDPattern.MatchString(id) {
			continue
		}
		if err := r.Delete(ctx, &cms.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting component-state ConfigMap %q: %w", cms.Items[i].Name, err)
		}
	}
	return nil
}

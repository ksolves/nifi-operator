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
	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

const (
	labelAppName      = "app.kubernetes.io/name"
	labelAppInstance  = "app.kubernetes.io/instance"
	labelAppManagedBy = "app.kubernetes.io/managed-by"
	labelNodeGroup    = "nifi.ksolves.com/node-group"

	managedByValue = "nifi-operator"
	appNameValue   = "nifi"
)

// commonLabels returns the labels every object generated for the given node group must carry,
// so pod selectors, Service endpoints and ownership are all consistent.
func commonLabels(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	labels := map[string]string{
		labelAppName:      appNameValue,
		labelAppInstance:  cluster.Name,
		labelAppManagedBy: managedByValue,
		labelNodeGroup:    nodeGroup.Name,
	}
	return mergeStringMaps(cluster.Spec.Labels, nodeGroup.Labels, labels)
}

// clusterLabels returns the labels for objects that belong to the whole cluster rather than one
// node group. They deliberately carry no node-group label, which is what keeps
// pruneRemovedNodeGroups from ever deleting them. A user-set node-group label in spec.labels is
// dropped for the same reason: it would make pruneRemovedNodeGroups treat cluster-wide objects as
// node-group objects.
func clusterLabels(cluster *nifiv1alpha1.NiFiCluster) map[string]string {
	labels := mergeStringMaps(cluster.Spec.Labels, map[string]string{
		labelAppName:      appNameValue,
		labelAppInstance:  cluster.Name,
		labelAppManagedBy: managedByValue,
	})
	if labels != nil {
		delete(labels, labelNodeGroup)
	}
	return labels
}

// mergedAnnotations merges cluster-level and node-group-level annotations, with node-group-level
// annotations taking precedence.
func mergedAnnotations(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	return mergeStringMaps(cluster.Spec.Annotations, nodeGroup.Annotations)
}

// mergeStringMaps merges maps in order, with later maps taking precedence over earlier ones. Nil
// maps are skipped. Returns nil if every input map is empty, so callers can assign the result
// directly to an ObjectMeta field without producing an empty-but-non-nil map.
func mergeStringMaps(maps ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// podLabels are the labels on a node group's pod template: only the operator's own, never
// spec.labels or a node group's labels. Those are object metadata, and a pod-template change rolls
// every NiFi node.
func podLabels(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	return map[string]string{
		labelAppName:      appNameValue,
		labelAppInstance:  cluster.Name,
		labelAppManagedBy: managedByValue,
		labelNodeGroup:    nodeGroup.Name,
	}
}

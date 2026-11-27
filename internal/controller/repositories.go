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
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// nifi.properties keys of the typed repository settings.
const (
	nifiPropArchiveEnabled         = "nifi.content.repository.archive.enabled"
	nifiPropArchiveMaxRetention    = "nifi.content.repository.archive.max.retention.period"
	nifiPropArchiveMaxUsage        = "nifi.content.repository.archive.max.usage.percentage"
	nifiPropProvenanceMaxTime      = "nifi.provenance.repository.max.storage.time"
	nifiPropProvenanceMaxSize      = "nifi.provenance.repository.max.storage.size"
	nifiPropFlowFileCheckpointIntv = "nifi.flowfile.repository.checkpoint.interval"
)

// repositoryProperties returns the nifi.properties entries one Repositories spec sets. Only set
// fields produce an entry, so NiFi's own defaults stay for everything else.
func repositoryProperties(spec *nifiv1alpha1.RepositoriesSpec) map[string]string {
	props := map[string]string{}
	if spec == nil {
		return props
	}
	if a := spec.ContentArchive; a != nil {
		if a.Enabled != nil {
			props[nifiPropArchiveEnabled] = fmt.Sprintf("%t", *a.Enabled)
		}
		if a.MaxRetentionPeriod != "" {
			props[nifiPropArchiveMaxRetention] = a.MaxRetentionPeriod
		}
		if a.MaxUsagePercentage != nil {
			props[nifiPropArchiveMaxUsage] = fmt.Sprintf("%d%%", *a.MaxUsagePercentage)
		}
	}
	if p := spec.Provenance; p != nil {
		if p.MaxStorageTime != "" {
			props[nifiPropProvenanceMaxTime] = p.MaxStorageTime
		}
		if q, err := resource.ParseQuantity(p.MaxStorageSize); p.MaxStorageSize != "" && err == nil {
			props[nifiPropProvenanceMaxSize] = nifiDataSize(q.Value())
		}
	}
	if f := spec.FlowFile; f != nil && f.CheckpointInterval != "" {
		props[nifiPropFlowFileCheckpointIntv] = f.CheckpointInterval
	}
	return props
}

// effectiveRepositoryProperties resolves a node group's repository settings field by field: the
// node group's own value over the cluster's.
func effectiveRepositoryProperties(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	props := repositoryProperties(cluster.Spec.Repositories)
	for k, v := range repositoryProperties(nodeGroup.Repositories) {
		props[k] = v
	}
	return props
}

// TypedRepositoryKey reports whether the cluster or any of its node groups sets key through a
// typed Repositories field. Such a key is then reserved from configOverrides cluster-wide, so the
// typed field is the one place it is set; a key nobody types stays free to override, as before.
func TypedRepositoryKey(cluster *nifiv1alpha1.NiFiCluster, key string) bool {
	if _, ok := repositoryProperties(cluster.Spec.Repositories)[key]; ok {
		return true
	}
	for i := range cluster.Spec.NodeGroups {
		if _, ok := repositoryProperties(cluster.Spec.NodeGroups[i].Repositories)[key]; ok {
			return true
		}
	}
	return false
}

// nifiDataSize writes bytes in NiFi's data size format, in the largest unit that divides it
// exactly. NiFi's units are binary (1 KB = 1024 B), so "10Gi" becomes "10 GB".
func nifiDataSize(bytes int64) string {
	for _, u := range []struct {
		name string
		size int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if bytes >= u.size && bytes%u.size == 0 {
			return fmt.Sprintf("%d %s", bytes/u.size, u.name)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

// ProvenanceMaxStorageBytes returns a node group's effective provenance size cap in bytes, and
// whether one is set.
func ProvenanceMaxStorageBytes(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) (int64, bool) {
	for _, spec := range []*nifiv1alpha1.RepositoriesSpec{nodeGroup.Repositories, cluster.Spec.Repositories} {
		if spec == nil || spec.Provenance == nil || spec.Provenance.MaxStorageSize == "" {
			continue
		}
		if q, err := resource.ParseQuantity(spec.Provenance.MaxStorageSize); err == nil {
			return q.Value(), true
		}
	}
	return 0, false
}

// ArchiveMaxUsagePercentage returns a node group's effective content archive usage cap, and
// whether one is set.
func ArchiveMaxUsagePercentage(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) (int32, bool) {
	for _, spec := range []*nifiv1alpha1.RepositoriesSpec{nodeGroup.Repositories, cluster.Spec.Repositories} {
		if spec != nil && spec.ContentArchive != nil && spec.ContentArchive.MaxUsagePercentage != nil {
			return *spec.ContentArchive.MaxUsagePercentage, true
		}
	}
	return 0, false
}

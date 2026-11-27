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
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func TestNiFiDataSize(t *testing.T) {
	for bytes, want := range map[int64]string{
		10 << 30:    "10 GB",
		1536 << 20:  "1536 MB",
		2 << 40:     "2 TB",
		10000000000: "9765625 KB",
		1001:        "1001 B",
	} {
		if got := nifiDataSize(bytes); got != want {
			t.Errorf("nifiDataSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}

// only set fields render, the node group wins field by field, and the values reach
// nifi.properties over a configOverrides entry for the same key.
func TestRepositoryPropertiesRendering(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		Repositories: &nifiv1alpha1.RepositoriesSpec{
			ContentArchive: &nifiv1alpha1.ContentArchiveSpec{Enabled: ptr.To(true), MaxRetentionPeriod: "12 hours", MaxUsagePercentage: ptr.To(int32(60))},
			Provenance:     &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageTime: "30 days", MaxStorageSize: "10Gi"},
		},
		ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{NifiProperties: map[string]string{
			nifiPropProvenanceMaxTime:   "1 day", // reserved by the typed field: ignored
			"nifi.queue.swap.threshold": "40000",
		}},
	}}
	group := &nifiv1alpha1.NodeGroup{Name: "g", Repositories: &nifiv1alpha1.RepositoriesSpec{
		ContentArchive: &nifiv1alpha1.ContentArchiveSpec{MaxRetentionPeriod: "2 days"},
		FlowFile:       &nifiv1alpha1.FlowFileRepositorySpec{CheckpointInterval: "20 secs"},
	}}
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{*group}

	got := buildNifiProperties(cluster, group)
	for _, line := range []string{
		"nifi.content.repository.archive.enabled=true",
		"nifi.content.repository.archive.max.retention.period=2 days",
		"nifi.content.repository.archive.max.usage.percentage=60%",
		"nifi.provenance.repository.max.storage.time=30 days",
		"nifi.provenance.repository.max.storage.size=10 GB",
		"nifi.flowfile.repository.checkpoint.interval=20 secs",
		"nifi.queue.swap.threshold=40000",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("nifi.properties lacks %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "max.storage.time=1 day") {
		t.Error("a configOverrides entry beat the typed field")
	}

	if got := buildNifiProperties(&nifiv1alpha1.NiFiCluster{}, &nifiv1alpha1.NodeGroup{Name: "g"}); strings.Contains(got, "archive.") ||
		strings.Contains(got, "max.storage") || strings.Contains(got, "checkpoint.interval") {
		t.Errorf("unset repositories rendered keys:\n%s", got)
	}
}

func TestTypedRepositoryKeyReservation(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
		{Name: "a"},
		{Name: "b", Repositories: &nifiv1alpha1.RepositoriesSpec{FlowFile: &nifiv1alpha1.FlowFileRepositorySpec{CheckpointInterval: "1 min"}}},
	}}}
	if !IsReservedNifiPropertyKey(cluster, nifiPropFlowFileCheckpointIntv) {
		t.Error("a key typed on one node group must be reserved cluster-wide")
	}
	if IsReservedNifiPropertyKey(cluster, nifiPropProvenanceMaxTime) {
		t.Error("a key nobody types must stay free to override")
	}
}

func TestRepositoryResolvers(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{Repositories: &nifiv1alpha1.RepositoriesSpec{
		Provenance:     &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageSize: "20Gi"},
		ContentArchive: &nifiv1alpha1.ContentArchiveSpec{MaxUsagePercentage: ptr.To(int32(50))},
	}}}
	group := &nifiv1alpha1.NodeGroup{Repositories: &nifiv1alpha1.RepositoriesSpec{
		Provenance: &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageSize: "5Gi"},
	}}
	if b, ok := ProvenanceMaxStorageBytes(cluster, group); !ok || b != 5<<30 {
		t.Errorf("ProvenanceMaxStorageBytes = %d, %v; want the group's 5Gi", b, ok)
	}
	if p, ok := ArchiveMaxUsagePercentage(cluster, group); !ok || p != 50 {
		t.Errorf("ArchiveMaxUsagePercentage = %d, %v; want the cluster's 50", p, ok)
	}
	if _, ok := ProvenanceMaxStorageBytes(&nifiv1alpha1.NiFiCluster{}, &nifiv1alpha1.NodeGroup{}); ok {
		t.Error("nothing set must report no cap")
	}
}

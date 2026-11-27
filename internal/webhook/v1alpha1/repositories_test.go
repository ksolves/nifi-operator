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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// a provenance cap above the volume holding it, or an archive filling the disk, is warned about.
func TestRepositoryWarnings(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "a", Storage: "10Gi"}}}}
	if w := repositoryWarnings(cluster); len(w) != 0 {
		t.Fatalf("no repositories warned: %v", w)
	}

	cluster.Spec.Repositories = &nifiv1alpha1.RepositoriesSpec{Provenance: &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageSize: "20Gi"}}
	w := repositoryWarnings(cluster)
	if len(w) != 1 {
		t.Fatalf("want 1 warning, got %v", w)
	}
	if !strings.Contains(w[0], `"a"`) || !strings.Contains(w[0], "maxStorageSize") {
		t.Errorf("group a's maxStorageSize not named: %q", w[0])
	}

	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{
		Name: "b", Storage: "10Gi",
		SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true, ProvenanceRepositoryStorage: "30Gi"},
	}}
	if w := repositoryWarnings(cluster); len(w) != 0 {
		t.Errorf("split provenance volume large enough, still warned: %v", w)
	}

	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{
		Name: "c", Storage: "10Gi",
		Repositories: &nifiv1alpha1.RepositoriesSpec{Provenance: &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageSize: "5Gi"}},
	}}
	if w := repositoryWarnings(cluster); len(w) != 0 {
		t.Errorf("group override below its volume, still warned: %v", w)
	}

	cluster.Spec.Repositories = &nifiv1alpha1.RepositoriesSpec{ContentArchive: &nifiv1alpha1.ContentArchiveSpec{MaxUsagePercentage: ptr.To(int32(95))}}
	w = repositoryWarnings(cluster)
	if len(w) != 1 || !strings.Contains(w[0], "maxUsagePercentage") {
		t.Errorf("want 1 maxUsagePercentage warning, got %v", w)
	}
	cluster.Spec.Repositories.ContentArchive.MaxUsagePercentage = ptr.To(int32(80))
	if w := repositoryWarnings(cluster); len(w) != 0 {
		t.Errorf("80%% archive usage warned: %v", w)
	}
}

// a key set through a typed repositories field is reserved from configOverrides cluster-wide.
func TestValidateConfigOverridesRepositories(t *testing.T) {
	path := field.NewPath("spec", "configOverrides")
	const timeKey = "nifi.provenance.repository.max.storage.time"
	overrides := &nifiv1alpha1.ConfigOverridesSpec{NifiProperties: map[string]string{timeKey: "1 day"}}

	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups:   []nifiv1alpha1.NodeGroup{{Name: "a", Storage: "10Gi"}},
		Repositories: &nifiv1alpha1.RepositoriesSpec{Provenance: &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageTime: "30 days"}},
	}}
	errs := validateConfigOverrides(path, cluster, overrides)
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	if !strings.Contains(errs[0].Detail, "spec.repositories") {
		t.Errorf("error doesn't point at spec.repositories: %q", errs[0].Detail)
	}

	cluster.Spec.Repositories = nil
	if errs := validateConfigOverrides(path, cluster, overrides); len(errs) != 0 {
		t.Errorf("untyped key rejected: %v", errs)
	}

	cluster.Spec.NodeGroups[0].Repositories = &nifiv1alpha1.RepositoriesSpec{
		ContentArchive: &nifiv1alpha1.ContentArchiveSpec{Enabled: ptr.To(true)},
	}
	archive := &nifiv1alpha1.ConfigOverridesSpec{NifiProperties: map[string]string{"nifi.content.repository.archive.enabled": "false"}}
	if errs := validateConfigOverrides(path, cluster, archive); len(errs) != 1 {
		t.Errorf("key typed on a node group not reserved cluster-wide: %v", errs)
	}
}

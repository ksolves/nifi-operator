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
	"testing"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// A replica-count change alters max.candidates in the ConfigMap but must not change the hash that
// rolls every pod; any other property change must.
func TestConfigMapHashIgnoresRestartExemptProperties(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "ng1", Replicas: 3}}

	base := configMapHash(buildConfigMap(cluster, &cluster.Spec.NodeGroups[0]))

	cluster.Spec.NodeGroups[0].Replicas = 5
	scaled := buildConfigMap(cluster, &cluster.Spec.NodeGroups[0])
	if got := configMapHash(scaled); got != base {
		t.Errorf("configMapHash changed on a replica-only change (%s -> %s)", base, got)
	}

	cluster.Spec.NodeGroups[0].ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
		NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "45 sec"},
	}
	if got := configMapHash(buildConfigMap(cluster, &cluster.Spec.NodeGroups[0])); got == base {
		t.Error("configMapHash did not change on a real configuration change")
	}
}

func TestWithoutRestartExemptProperties(t *testing.T) {
	in := "a=1\nnifi.cluster.flow.election.max.candidates=3\nnifi.cluster.flow.election.max.candidatesX=keep\nb=2\n"
	want := "a=1\nnifi.cluster.flow.election.max.candidatesX=keep\nb=2\n"
	if got := withoutRestartExemptProperties(in); got != want {
		t.Errorf("withoutRestartExemptProperties() = %q, want %q", got, want)
	}
}

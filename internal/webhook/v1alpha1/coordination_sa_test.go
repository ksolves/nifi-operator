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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func saCluster(bind bool, backend nifiv1alpha1.CoordinationBackend, sas ...string) *nifiv1alpha1.NiFiCluster {
	c := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	c.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{Backend: backend, BindPodServiceAccounts: bind}
	if backend == nifiv1alpha1.CoordinationBackendZooKeeper {
		c.Spec.Coordination.ZooKeeper = &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk:2181"}
	}
	for i, sa := range sas {
		c.Spec.NodeGroups = append(c.Spec.NodeGroups, nifiv1alpha1.NodeGroup{
			Name: string(rune('a' + i)), PodOverrides: &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: sa}})
	}
	return c
}

// a node group on its own ServiceAccount without the binding opt-in is warned about,
// with the exact RoleBinding to create.
func TestCoordinationServiceAccountWarnings(t *testing.T) {
	w := coordinationServiceAccountWarnings(saCluster(false, nifiv1alpha1.CoordinationBackendKubernetes, "team-sa"))
	if len(w) != 1 || !strings.Contains(w[0], "kubectl -n ns create rolebinding c-nifi-team-sa --role=c-nifi --serviceaccount=ns:team-sa") {
		t.Errorf("warnings = %v", w)
	}
	for name, c := range map[string]*nifiv1alpha1.NiFiCluster{
		"opted in":      saCluster(true, nifiv1alpha1.CoordinationBackendKubernetes, "team-sa"),
		"ZooKeeper":     saCluster(false, nifiv1alpha1.CoordinationBackendZooKeeper, "team-sa"),
		"no override":   saCluster(false, nifiv1alpha1.CoordinationBackendKubernetes),
		"cluster's own": saCluster(false, nifiv1alpha1.CoordinationBackendKubernetes, "c-nifi"),
	} {
		if w := coordinationServiceAccountWarnings(c); len(w) != 0 {
			t.Errorf("%s: unexpected warnings %v", name, w)
		}
	}
}

// Escalation guard: asking the operator to bind the coordination Role to node groups'
// ServiceAccounts requires the requester to hold update on Leases and ConfigMaps themselves -
// on opt-in, and whenever the set of ServiceAccounts changes, but not on unrelated edits.
func TestCoordinationBindingGrants(t *testing.T) {
	k8s := nifiv1alpha1.CoordinationBackendKubernetes
	resources := func(g []exposureGrant) []string {
		out := make([]string, 0, len(g))
		for _, x := range g {
			out = append(out, x.verb+" "+x.resource)
		}
		return out
	}
	if got := resources(coordinationBindingGrants(nil, saCluster(true, k8s, "team-sa"))); strings.Join(got, ",") != "update leases,update configmaps" {
		t.Errorf("opt-in on create: grants %v", got)
	}
	if got := coordinationBindingGrants(saCluster(false, k8s, "team-sa"), saCluster(true, k8s, "team-sa")); len(got) != 2 {
		t.Errorf("opt-in on update: grants %v", resources(got))
	}
	if got := coordinationBindingGrants(saCluster(true, k8s, "team-sa"), saCluster(true, k8s, "team-sa", "other-sa")); len(got) != 2 {
		t.Errorf("new ServiceAccount: grants %v", resources(got))
	}
	for name, pair := range map[string][2]*nifiv1alpha1.NiFiCluster{
		"unchanged":         {saCluster(true, k8s, "team-sa"), saCluster(true, k8s, "team-sa")},
		"not opted in":      {nil, saCluster(false, k8s, "team-sa")},
		"no override":       {nil, saCluster(true, k8s)},
		"ZooKeeper backend": {nil, saCluster(true, nifiv1alpha1.CoordinationBackendZooKeeper, "team-sa")},
	} {
		if got := coordinationBindingGrants(pair[0], pair[1]); len(got) != 0 {
			t.Errorf("%s: unexpected grants %v", name, resources(got))
		}
	}
}

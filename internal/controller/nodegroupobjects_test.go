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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func nodeGroupObject(name string, replicas int32, deleting bool) nifiv1alpha1.NiFiNodeGroup {
	o := nifiv1alpha1.NiFiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: nifiv1alpha1.NiFiNodeGroupSpec{ClusterRef: "c",
			NodeGroup: nifiv1alpha1.NodeGroup{Name: name, Replicas: replicas, Storage: "1Gi"}},
	}
	if deleting {
		now := metav1.Now()
		o.DeletionTimestamp = &now
		o.Finalizers = []string{"x"}
	}
	return o
}

// NiFiNodeGroups join the inline node groups in memory; one being deleted is left out
// (so it is pruned), and one named like an inline group is a conflict and left out.
func TestMergeNodeGroups(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
		{Name: "main", Replicas: 2},
	}}}
	conflicts := mergeNodeGroups(cluster, []nifiv1alpha1.NiFiNodeGroup{
		nodeGroupObject("workers", 3, false),
		nodeGroupObject("old", 1, true),
		nodeGroupObject("main", 5, false),
	})
	names := make([]string, 0, len(cluster.Spec.NodeGroups))
	for _, g := range cluster.Spec.NodeGroups {
		names = append(names, g.Name)
	}
	if !reflect.DeepEqual(names, []string{"main", "workers"}) {
		t.Errorf("merged groups = %v, want [main workers]", names)
	}
	if cluster.Spec.NodeGroups[0].Replicas != 2 || cluster.Spec.NodeGroups[1].Replicas != 3 {
		t.Errorf("replicas = %+v", cluster.Spec.NodeGroups)
	}
	if !reflect.DeepEqual(conflicts, map[string]bool{"main": true}) {
		t.Errorf("conflicts = %v, want main", conflicts)
	}
}

func TestNodeGroupSelector(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	want := "app.kubernetes.io/instance=prod,app.kubernetes.io/name=nifi,nifi.ksolves.com/node-group=workers"
	if got := nodeGroupSelector(cluster, "workers"); got != want {
		t.Errorf("selector = %q, want %q", got, want)
	}
}

// The status write's response replaces the object in memory; the merged node groups must survive
// it, or the rest of the reconcile would lose the NiFiNodeGroups (and prune their StatefulSets).
func TestUpdateClusterStatusKeepsMergedNodeGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	stored := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "main", Replicas: 1, Storage: "1Gi"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).WithStatusSubresource(stored).Build()
	r := &NiFiClusterReconciler{Client: c, Scheme: scheme}
	cluster := &nifiv1alpha1.NiFiCluster{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stored), cluster); err != nil {
		t.Fatal(err)
	}
	mergeNodeGroups(cluster, []nifiv1alpha1.NiFiNodeGroup{nodeGroupObject("workers", 2, false)})
	cluster.Status.Replicas = 3
	if err := r.updateClusterStatus(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	if len(cluster.Spec.NodeGroups) != 2 {
		t.Errorf("node groups after the status write = %+v, want main and workers", cluster.Spec.NodeGroups)
	}
	fresh := &nifiv1alpha1.NiFiCluster{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stored), fresh); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Spec.NodeGroups) != 1 || fresh.Status.Replicas != 3 {
		t.Errorf("stored cluster spec groups %d (want 1: merged list never written), status replicas %d",
			len(fresh.Spec.NodeGroups), fresh.Status.Replicas)
	}
}

// OpenShift's OwnerReferencesPermissionEnforcement admission plugin refuses a blocking owner
// reference on an object the client may not delete. Without delete on nifinodegroups the owner
// patch failed every reconcile on OKD, which held up node removal;
// envtest and kind do not run that plugin, so the generated role is checked here.
func TestOperatorRoleCanDeleteNiFiNodeGroups(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatal(err)
	}
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "nifi.ksolves.com") && slices.Contains(rule.Resources, "nifinodegroups") &&
			slices.Contains(rule.Verbs, "delete") {
			return
		}
	}
	t.Error("the operator role does not grant delete on nifinodegroups, which OpenShift requires to set their owner reference")
}

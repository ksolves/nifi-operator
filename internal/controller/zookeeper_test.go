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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func zooKeeperCluster() *nifiv1alpha1.NiFiCluster {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "ng1", Replicas: 2, Storage: "1Gi"}}
	cluster.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{
		Backend:   nifiv1alpha1.CoordinationBackendZooKeeper,
		ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk-0.zk:2181,zk-1.zk:2181"},
	}
	return cluster
}

func TestBuildNifiPropertiesZooKeeperCoordination(t *testing.T) {
	cluster := zooKeeperCluster()
	props := buildNifiProperties(cluster, &cluster.Spec.NodeGroups[0])
	for _, want := range []string{
		"nifi.cluster.leader.election.implementation=CuratorLeaderElectionManager\n",
		"nifi.zookeeper.connect.string=zk-0.zk:2181,zk-1.zk:2181\n",
		"nifi.zookeeper.root.node=/nifi/" + testClusterNamespace + "/" + testClusterName + "\n",
		"nifi.state.management.provider.cluster=zk-provider\n",
		"nifi.state.management.embedded.zookeeper.start=false\n",
	} {
		if !strings.Contains(props, want) {
			t.Errorf("nifi.properties missing %q:\n%s", want, props)
		}
	}
	if strings.Contains(props, "lease.prefix") {
		t.Errorf("ZooKeeper coordination must not set a Kubernetes lease prefix:\n%s", props)
	}

	cluster.Spec.Coordination.ZooKeeper.RootNode = "/teams/data/nifi"
	if props := buildNifiProperties(cluster, &cluster.Spec.NodeGroups[0]); !strings.Contains(props, "nifi.zookeeper.root.node=/teams/data/nifi\n") {
		t.Errorf("explicit rootNode not used:\n%s", props)
	}
}

func TestBuildStatefulSetServiceAccountTokenOnlyForKubernetesCoordination(t *testing.T) {
	for _, c := range []struct {
		name    string
		cluster *nifiv1alpha1.NiFiCluster
		want    bool
	}{
		{"Kubernetes", withStorage(exposureCluster(false)), true},
		{"ZooKeeper", zooKeeperCluster(), false},
	} {
		sts, err := buildStatefulSet(c.cluster, &c.cluster.Spec.NodeGroups[0], "hash", "", nifiPodServiceAccount(c.cluster))
		if err != nil {
			t.Fatal(err)
		}
		pod := sts.Spec.Template.Spec
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken != c.want {
			t.Errorf("%s: AutomountServiceAccountToken = %v, want %v", c.name, pod.AutomountServiceAccountToken, c.want)
		}
		found := false
		for _, e := range pod.Containers[0].Env {
			if e.Name == "POD_NAMESPACE" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.namespace" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: POD_NAMESPACE not set from the downward API (needed without the token mount)", c.name)
		}
		if got := sts.Annotations[coordinationBackendAnnotation]; got != c.name {
			t.Errorf("%s: StatefulSet stamped %q", c.name, got)
		}
	}
}

func TestReconcileNiFiRBACZooKeeperGetsNoRole(t *testing.T) {
	r, cluster := rbacTestReconciler(t, "anyuid")
	ctx := context.Background()
	if err := r.reconcileNiFiRBAC(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Coordination = zooKeeperCluster().Spec.Coordination
	if err := r.reconcileNiFiRBAC(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "ns", Name: nifiRoleName(cluster)}
	if err := r.Get(ctx, key, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Errorf("coordination Role kept for a ZooKeeper cluster (err=%v)", err)
	}
	if err := r.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("coordination RoleBinding kept for a ZooKeeper cluster (err=%v)", err)
	}
	// The ServiceAccount and SCC binding stay: the pods still need an identity to be admitted.
	if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: nifiServiceAccountName(cluster)}, &corev1.ServiceAccount{}); err != nil {
		t.Errorf("ServiceAccount missing: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: nifiSCCRoleBindingName(cluster)}, &rbacv1.RoleBinding{}); err != nil {
		t.Errorf("SCC RoleBinding missing: %v", err)
	}
}

func withStorage(cluster *nifiv1alpha1.NiFiCluster) *nifiv1alpha1.NiFiCluster {
	for i := range cluster.Spec.NodeGroups {
		cluster.Spec.NodeGroups[i].Storage = "1Gi"
	}
	return cluster
}

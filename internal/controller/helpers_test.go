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
	"reflect"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Unit tests for the small pure helpers in names.go, labels.go, secret.go and coordination.go.
// Several of these names are contracts with other components (the CSI driver, the
// entrypoint, NiFi itself), so a silent change breaks a running cluster.

func TestObjectNames(t *testing.T) {
	cluster := minimalCluster() // test-cluster / default
	ng := &nifiv1alpha1.NodeGroup{Name: "ng"}
	for _, tc := range []struct{ got, want string }{
		{statefulSetName(cluster, ng), "test-cluster-ng"},
		{headlessServiceName(cluster, ng), "test-cluster-ng-headless"},
		{configMapName(cluster, ng), "test-cluster-ng-config"},
		{configSecretName(cluster, ng), "test-cluster-ng-config-secret"},
		{sensitivePropsSecretName(cluster), "test-cluster-sensitive-props"},
		{tlsCASecretName(cluster), "test-cluster-ca-certificate"},
		{authSecretName(cluster), "test-cluster-auth-credentials"},
		{csiCAReaderName(cluster), "test-cluster-csi-ca-reader"},
		{nifiRoleName(cluster), "test-cluster-nifi"},
		{PodFQDN("p-0", "c", "ng", "ns"), "p-0.c-ng-headless.ns.svc." + ClusterDomain},
		{TLSCASecretName(cluster.Name), tlsCASecretName(cluster)},
		{AuthSecretName(cluster.Name), authSecretName(cluster)},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestEffectiveAuthSecretName(t *testing.T) {
	cluster := minimalCluster()
	if got := effectiveAuthSecretName(cluster); got != "test-cluster-auth-credentials" {
		t.Errorf("default mode: %q", got)
	}
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "mine"}}
	if got := effectiveAuthSecretName(cluster); got != "mine" {
		t.Errorf("secretName mode: %q", got)
	}
}

func TestMergeStringMapsAndLabels(t *testing.T) {
	if got := mergeStringMaps(nil, map[string]string{}); got != nil {
		t.Errorf("mergeStringMaps of empty maps = %v, want nil", got)
	}
	if got := mergeStringMaps(map[string]string{"a": "1", "b": "1"}, map[string]string{"b": "2"}); !reflect.DeepEqual(got, map[string]string{"a": "1", "b": "2"}) {
		t.Errorf("later map must win: %v", got)
	}

	cluster := minimalCluster()
	cluster.Spec.Labels = map[string]string{labelAppName: "user-tries-to-override", "team": "x"}
	ng := &nifiv1alpha1.NodeGroup{Name: "ng", Labels: map[string]string{"tier": "ingest"}}
	labels := commonLabels(cluster, ng)
	if labels[labelAppName] != appNameValue || labels["team"] != "x" || labels["tier"] != "ingest" || labels[labelNodeGroup] != "ng" {
		t.Errorf("commonLabels = %v: operator labels must win, user labels must be kept", labels)
	}
	if _, ok := clusterLabels(cluster)[labelNodeGroup]; ok {
		t.Error("clusterLabels must not carry a node-group label (cluster-scoped objects would be pruned)")
	}
}

func TestGeneratedSecrets(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, errA := generateSensitivePropsKey()
	b, errB := generateSensitivePropsKey()
	if errA != nil || errB != nil || !hex32.MatchString(a) || a == b {
		t.Errorf("generateSensitivePropsKey = %q, %q (%v, %v), want two distinct 32-hex keys", a, b, errA, errB)
	}
	p, err := generateRandomPassword()
	if err != nil || !hex32.MatchString(p) || len(p) < 12 {
		t.Errorf("generateRandomPassword = %q, %v, want 32 hex chars (NiFi needs >= 12)", p, err)
	}

	cluster := minimalCluster()
	def, err := buildDefaultAuthSecret(cluster)
	if err != nil || def.StringData["username"]+string(def.Data["username"]) == "" {
		t.Errorf("buildDefaultAuthSecret = %+v, %v, want a username", def, err)
	}
}

func TestCoordinationProperties(t *testing.T) {
	cluster := minimalCluster()
	if needsKubernetesAPI(cluster) != true || coordinationBackend(cluster) != nifiv1alpha1.CoordinationBackendKubernetes {
		t.Error("an unset backend must default to Kubernetes")
	}
	k8s := coordinationProperties(cluster)
	if k8s[nifiPropLeaderElectionLeasePrefix] != cluster.Name || k8s[nifiPropEmbeddedZookeeperStart] != "false" {
		t.Errorf("Kubernetes properties = %v", k8s)
	}

	cluster.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{
		Backend:   nifiv1alpha1.CoordinationBackendZooKeeper,
		ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk:2181"},
	}
	zk := coordinationProperties(cluster)
	if needsKubernetesAPI(cluster) || zk[nifiPropZookeeperConnectString] != "zk:2181" ||
		zk[nifiPropZookeeperRootNode] != "/nifi/default/test-cluster" {
		t.Errorf("ZooKeeper properties = %v", zk)
	}
	cluster.Spec.Coordination.ZooKeeper.RootNode = "/custom"
	if got := zooKeeperRootNode(cluster); got != "/custom" {
		t.Errorf("explicit root node = %q", got)
	}

	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Replicas: 2}, {Replicas: 3}}
	if got := clusterReplicas(cluster); got != 5 {
		t.Errorf("clusterReplicas = %d, want 5", got)
	}
}

// foreignControlledConfigMap returns a ConfigMap controlled by some other object, which the
// operator refuses to adopt.
func foreignControlledConfigMap(name, namespace string) *corev1.ConfigMap {
	controller := true
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "ConfigMap", Name: "someone-else", UID: "foreign-owner", Controller: &controller,
		}},
	}}
}

// exposureCluster is a cluster with node groups a (2 replicas) and b (1), optionally with TLS.
func exposureCluster(tls bool) *nifiv1alpha1.NiFiCluster {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	var t *nifiv1alpha1.TLSSpec
	if tls {
		t = &nifiv1alpha1.TLSSpec{Enabled: true, ExtraSANs: []string{"extra.example.com"}}
	}
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 2, TLS: t}, {Name: "b", Replicas: 1, TLS: t}}
	return cluster
}

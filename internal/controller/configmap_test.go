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

	corev1 "k8s.io/api/core/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

const (
	testClusterName      = "c1"
	testClusterNamespace = "ns1"
)

func TestBuildNifiPropertiesTLSDisabledMatchesPhase1(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace

	wantLines := []string{
		"nifi.web.http.host=0.0.0.0",
		"nifi.web.http.port=8080",
		"nifi.web.https.port=",
		"nifi.remote.input.secure=false",
		"nifi.security.keystore=",
		"nifi.security.truststore=",
	}

	for _, nodeGroup := range []*nifiv1alpha1.NodeGroup{
		{Name: "ng1", Replicas: 1}, // TLS unset
		{Name: "ng1", Replicas: 1, TLS: &nifiv1alpha1.TLSSpec{Enabled: false}},
	} {
		props := buildNifiProperties(cluster, nodeGroup)
		for _, want := range wantLines {
			if !strings.Contains(props, want) {
				t.Errorf("TLS=%+v: nifi.properties missing unsecured line %q:\n%s", nodeGroup.TLS, want, props)
			}
		}
		if strings.Contains(props, "PKCS12") {
			t.Errorf("TLS=%+v: nifi.properties should not reference PKCS12 when TLS is disabled:\n%s", nodeGroup.TLS, props)
		}
	}
}

// nifi.cluster.protocol.is.secure was removed in NiFi 2.7.0 (NIFI-15140): it is set, to match
// TLS, only for older NiFi, which defaults it to false, and for a tag without a version only on
// ZooKeeper, where it may be such a NiFi.
func TestBuildNifiPropertiesProtocolIsSecure(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		zk   bool
		want map[bool]string // TLS enabled -> expected line, "" for none
	}{
		{tag: "", want: map[bool]string{false: "", true: ""}}, // the default image, 2.12.0
		{tag: "nifi-node-2.12.0-0.13.1", want: map[bool]string{false: "", true: ""}},
		{tag: "2.7.0", want: map[bool]string{false: "", true: ""}},
		{tag: "latest", want: map[bool]string{false: "", true: ""}},
		{tag: "latest", zk: true, want: map[bool]string{
			false: "nifi.cluster.protocol.is.secure=false\n", true: "nifi.cluster.protocol.is.secure=true\n"}},
		{tag: "nifi-node-2.6.0-0.13.1", zk: true, want: map[bool]string{
			false: "nifi.cluster.protocol.is.secure=false\n", true: "nifi.cluster.protocol.is.secure=true\n"}},
		{tag: "1.14.0", zk: true, want: map[bool]string{
			false: "nifi.cluster.protocol.is.secure=false\n", true: "nifi.cluster.protocol.is.secure=true\n"}},
	} {
		cluster := protocolTestCluster(tc.tag, tc.zk)
		for _, enabled := range []bool{false, true} {
			nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1, TLS: &nifiv1alpha1.TLSSpec{Enabled: enabled}}
			props := buildNifiProperties(cluster, nodeGroup)
			if want := tc.want[enabled]; want == "" {
				if strings.Contains(props, "nifi.cluster.protocol.is.secure") {
					t.Errorf("tag %q TLS %v: nifi.cluster.protocol.is.secure set:\n%s", tc.tag, enabled, props)
				}
			} else if !strings.Contains(props, want) {
				t.Errorf("tag %q TLS %v: want %q in:\n%s", tc.tag, enabled, want, props)
			}
		}
	}
	if !IsReservedNifiPropertyKey(&nifiv1alpha1.NiFiCluster{}, "nifi.cluster.protocol.is.secure") {
		t.Error("nifi.cluster.protocol.is.secure is not reserved from configOverrides")
	}
}

// NiFi 1.x loads its flow from flow.xml.gz; 1.16 on also keeps flow.json.gz. Both must live on the
// volume. NiFi 2 keeps only flow.json.gz.
func TestBuildNifiPropertiesFlowFileByVersion(t *testing.T) {
	for _, tc := range []struct {
		tag, wantXML, wantJSON string
		split                  bool
	}{
		{tag: "1.28.1", wantXML: "./data/flow.xml.gz", wantJSON: "./data/flow.json.gz"},
		{tag: "1.14.0", split: true, wantXML: "./database-repository/flow.xml.gz", wantJSON: "./database-repository/flow.json.gz"},
		{tag: "2.0.0", wantJSON: "./data/flow.json.gz"},
		{tag: "latest", wantJSON: "./data/flow.json.gz"},
	} {
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}
		if tc.split {
			nodeGroup.SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}
		}
		props := buildNifiProperties(protocolTestCluster(tc.tag, true), nodeGroup)
		if tc.wantXML == "" {
			if !strings.Contains(props, "nifi.flow.configuration.file="+tc.wantJSON+"\n") ||
				strings.Contains(props, "nifi.flow.configuration.json.file") {
				t.Errorf("tag %q: want only the flow.json.gz file at %s:\n%s", tc.tag, tc.wantJSON, props)
			}
			continue
		}
		for _, want := range []string{"nifi.flow.configuration.file=" + tc.wantXML + "\n", "nifi.flow.configuration.json.file=" + tc.wantJSON + "\n"} {
			if !strings.Contains(props, want) {
				t.Errorf("tag %q: want %q in:\n%s", tc.tag, want, props)
			}
		}
	}
	if !IsReservedNifiPropertyKey(&nifiv1alpha1.NiFiCluster{}, "nifi.flow.configuration.json.file") {
		t.Error("nifi.flow.configuration.json.file is not reserved from configOverrides")
	}
}

func protocolTestCluster(tag string, zk bool) *nifiv1alpha1.NiFiCluster {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	if tag != "" {
		cluster.Spec.Image = nifiv1alpha1.ImageSpec{Repository: "repo/nifi", Tag: tag}
	}
	if zk {
		cluster.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{
			Backend:   nifiv1alpha1.CoordinationBackendZooKeeper,
			ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk:2181"},
		}
	}
	return cluster
}

func TestBuildNifiPropertiesTLSEnabled(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1, TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}

	props := buildNifiProperties(cluster, nodeGroup)

	wantLines := []string{
		"nifi.web.http.port=",
		"nifi.web.https.port=8443",
		"nifi.remote.input.http.enabled=false",
		"nifi.security.keystore=/opt/nifi/tls/keystore.p12",
		"nifi.security.keystoreType=PKCS12",
		"nifi.security.truststore=/opt/nifi/tls/truststore.p12",
		"nifi.security.truststoreType=PKCS12",
	}
	for _, want := range wantLines {
		if !strings.Contains(props, want) {
			t.Errorf("nifi.properties (TLS enabled) missing line %q:\n%s", want, props)
		}
	}
	if strings.Contains(props, "nifi.web.http.port=8080") {
		t.Errorf("nifi.properties (TLS enabled) should not still expose the unsecured HTTP port:\n%s", props)
	}
	// The passwords come from NIFI_KEYSTORE_PASSWORD, never the ConfigMap.
	if strings.Contains(props, "keystorePasswd") || strings.Contains(props, "truststorePasswd") {
		t.Errorf("nifi.properties (TLS enabled) carries a keystore password:\n%s", props)
	}
}

func TestBuildNifiPropertiesKubernetesCoordination(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "ng1", Replicas: 3}, {Name: "ng2", Replicas: 2}}

	for i := range cluster.Spec.NodeGroups {
		props := buildNifiProperties(cluster, &cluster.Spec.NodeGroups[i])
		for _, want := range []string{
			"nifi.cluster.leader.election.implementation=KubernetesLeaderElectionManager",
			"nifi.cluster.leader.election.kubernetes.lease.prefix=" + testClusterName,
			"nifi.state.management.provider.cluster=kubernetes-provider",
			"nifi.state.management.embedded.zookeeper.start=false",
			// Every node of every group votes in one flow election.
			"nifi.cluster.flow.election.max.candidates=5",
		} {
			if !strings.Contains(props, want+"\n") {
				t.Errorf("node group %q: nifi.properties missing %q:\n%s", cluster.Spec.NodeGroups[i].Name, want, props)
			}
		}
		if strings.Contains(props, "nifi.zookeeper.connect.string=") {
			t.Errorf("node group %q: Kubernetes coordination must not set a ZooKeeper connect string:\n%s",
				cluster.Spec.NodeGroups[i].Name, props)
		}
	}
}

func TestBuildNifiPropertiesProxyHost(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}

	t.Run("omitted when nothing configured", func(t *testing.T) {
		cluster := &nifiv1alpha1.NiFiCluster{}
		cluster.Name = testClusterName
		cluster.Namespace = testClusterNamespace

		props := buildNifiProperties(cluster, nodeGroup)
		if strings.Contains(props, "nifi.web.proxy.host") {
			t.Errorf("nifi.web.proxy.host should be omitted when unset:\n%s", props)
		}
	})

	t.Run("set from spec.proxyHosts", func(t *testing.T) {
		cluster := &nifiv1alpha1.NiFiCluster{}
		cluster.Name = testClusterName
		cluster.Namespace = testClusterNamespace
		cluster.Spec.ProxyHosts = []string{"nifi.example.com", "10.0.0.5:30080"}

		props := buildNifiProperties(cluster, nodeGroup)
		want := "nifi.web.proxy.host=nifi.example.com,10.0.0.5:30080"
		if !strings.Contains(props, want) {
			t.Errorf("nifi.properties missing %q:\n%s", want, props)
		}
	})
}

func TestBuildNifiPropertiesConfigOverrides(t *testing.T) {
	t.Run("cluster-level override applied", func(t *testing.T) {
		cluster := &nifiv1alpha1.NiFiCluster{}
		cluster.Name = testClusterName
		cluster.Namespace = testClusterNamespace
		cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
			NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "60 secs"},
		}
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}

		props := buildNifiProperties(cluster, nodeGroup)
		if want := "nifi.ui.autorefresh.interval=60 secs"; !strings.Contains(props, want) {
			t.Errorf("nifi.properties missing cluster-level override %q:\n%s", want, props)
		}
	})

	t.Run("node-group override takes precedence over cluster-level for the same key", func(t *testing.T) {
		cluster := &nifiv1alpha1.NiFiCluster{}
		cluster.Name = testClusterName
		cluster.Namespace = testClusterNamespace
		cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
			NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "60 secs"},
		}
		nodeGroup := &nifiv1alpha1.NodeGroup{
			Name: "ng1", Replicas: 1,
			ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{
				NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "30 secs"},
			},
		}

		props := buildNifiProperties(cluster, nodeGroup)
		if want := "nifi.ui.autorefresh.interval=30 secs"; !strings.Contains(props, want) {
			t.Errorf("node-group override should win, want %q:\n%s", want, props)
		}
		if strings.Contains(props, "60 secs") {
			t.Errorf("cluster-level value should be overridden, not both present:\n%s", props)
		}
	})

	t.Run("reserved key cannot be overridden even if it somehow reaches buildNifiProperties", func(t *testing.T) {
		cluster := &nifiv1alpha1.NiFiCluster{}
		cluster.Name = testClusterName
		cluster.Namespace = testClusterNamespace
		nodeGroup := &nifiv1alpha1.NodeGroup{
			Name: "ng1", Replicas: 1,
			ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{
				NifiProperties: map[string]string{"nifi.zookeeper.connect.string": "evil:2181"},
			},
		}

		props := buildNifiProperties(cluster, nodeGroup)
		if strings.Contains(props, "evil:2181") {
			t.Errorf("reserved key nifi.zookeeper.connect.string should never be overridable:\n%s", props)
		}
	})
}

func TestBuildNifiPropertiesSplitRepositoryStorage(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace

	t.Run("disabled by default: repository paths under the combined data PVC", func(t *testing.T) {
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}
		props := buildNifiProperties(cluster, nodeGroup)
		wantLines := []string{
			"nifi.flow.configuration.file=./data/flow.json.gz",
			"nifi.database.directory=./data/database_repository",
			"nifi.flowfile.repository.directory=./data/flowfile_repository",
			"nifi.content.repository.directory.default=./data/content_repository",
			"nifi.provenance.repository.directory.default=./data/provenance_repository",
		}
		for _, want := range wantLines {
			if !strings.Contains(props, want) {
				t.Errorf("missing unsecured-default line %q:\n%s", want, props)
			}
		}
	})

	t.Run("enabled: repository paths point at the dedicated per-repository PVCs", func(t *testing.T) {
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1, SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}}
		props := buildNifiProperties(cluster, nodeGroup)
		wantLines := []string{
			"nifi.flow.configuration.file=./database-repository/flow.json.gz",
			"nifi.database.directory=./database-repository/database_repository",
			"nifi.flowfile.repository.directory=./flowfile-repository",
			"nifi.content.repository.directory.default=./content-repository",
			"nifi.provenance.repository.directory.default=./provenance-repository",
		}
		for _, want := range wantLines {
			if !strings.Contains(props, want) {
				t.Errorf("missing split-storage line %q:\n%s", want, props)
			}
		}
		if strings.Contains(props, "./data/flow.json.gz") || strings.Contains(props, "./data/database_repository") {
			t.Errorf("should not still reference the combined data PVC's repository paths:\n%s", props)
		}
	})
}

func TestBuildBootstrapConfOverrides(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
		BootstrapConf: map[string]string{"java.arg.3": "-Xmx2g"},
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "ng1", Replicas: 1,
		ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{
			BootstrapConf: map[string]string{"java.arg.4": "-Xms1g"},
		},
	}

	overrides := buildBootstrapConfOverrides(cluster, nodeGroup)
	for _, want := range []string{"java.arg.3=-Xmx2g", "java.arg.4=-Xms1g"} {
		if !strings.Contains(overrides, want) {
			t.Errorf("bootstrap.conf.overrides missing %q:\n%s", want, overrides)
		}
	}
}

// TestBuildConfigMapNeverCarriesEntrypointSecretFiles is a regression test for NIFI-248 B2
// (PR #210 review): bootstrap.conf.overrides, logback.xml.override and authorizers.xml.override
// must never be in the ConfigMap - a compromised pod from another NiFiCluster in the same
// namespace could otherwise rewrite them via the broad ConfigMap Role every NiFi pod holds. They
// belong in the entrypoint Secret instead (see TestBuildEntrypointSecretLogbackAndAuthorizersOverride).
func TestBuildConfigMapNeverCarriesEntrypointSecretFiles(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	logback := "<configuration><!-- x --></configuration>"
	authorizers := "<authorizers><!-- x --></authorizers>"
	cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
		BootstrapConf: map[string]string{"java.arg.3": "-Xmx2g"},
		Logback:       &logback,
		Authorizers:   &authorizers,
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}

	cm := buildConfigMap(cluster, nodeGroup)
	for _, key := range []string{"bootstrap.conf.overrides", "logback.xml.override", "authorizers.xml.override"} {
		if _, ok := cm.Data[key]; ok {
			t.Errorf("ConfigMap carries %q, want it only in the entrypoint Secret", key)
		}
	}
}

func TestBuildEntrypointSecretLogbackAndAuthorizersOverride(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}

	t.Run("absent when not set", func(t *testing.T) {
		secret := buildEntrypointSecret(cluster, nodeGroup)
		if _, ok := secret.Data["logback.xml.override"]; ok {
			t.Error("logback.xml.override should not be present when unset")
		}
		if _, ok := secret.Data["authorizers.xml.override"]; ok {
			t.Error("authorizers.xml.override should not be present when unset")
		}
		if _, ok := secret.Data["bootstrap.conf.overrides"]; !ok {
			t.Error("bootstrap.conf.overrides should always be present, even if empty")
		}
	})

	t.Run("present with content when set, node-group wins over cluster", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		clusterLogback := "<configuration><!-- cluster --></configuration>"
		cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{Logback: &clusterLogback}
		nodeGroup := nodeGroup.DeepCopy()
		nodeGroupLogback := "<configuration><!-- node-group --></configuration>"
		nodeGroup.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{Logback: &nodeGroupLogback}

		secret := buildEntrypointSecret(cluster, nodeGroup)
		if got := string(secret.Data["logback.xml.override"]); got != nodeGroupLogback {
			t.Errorf("logback.xml.override = %q, want node-group value %q", got, nodeGroupLogback)
		}
	})
}

func TestConfigMapHashDeterministicAndSensitive(t *testing.T) {
	cm1 := &corev1.ConfigMap{Data: map[string]string{"a": "1", "b": "2"}}
	cm2 := &corev1.ConfigMap{Data: map[string]string{"b": "2", "a": "1"}}
	if configMapHash(cm1) != configMapHash(cm2) {
		t.Error("configMapHash should be independent of map iteration order")
	}

	cm3 := &corev1.ConfigMap{Data: map[string]string{"a": "1", "b": "3"}}
	if configMapHash(cm1) == configMapHash(cm3) {
		t.Error("configMapHash should change when Data changes")
	}
}

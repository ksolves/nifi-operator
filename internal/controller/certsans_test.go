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
	"slices"
	"testing"
)

func TestCertificateSANs(t *testing.T) {
	cluster := exposureCluster(true)
	cluster.Spec.ProxyHosts = []string{"proxy.example.com:9443", "proxy.example.com"}

	sans := CertificateSANs(cluster, &cluster.Spec.NodeGroups[0], testClusterName+"-a-0")
	want := []string{
		PodFQDN(testClusterName+"-a-0", testClusterName, "a", testClusterNamespace),
		"extra.example.com",
		testClusterName + "." + testClusterNamespace + ".svc",
		testClusterName + "." + testClusterNamespace + ".svc.cluster.local",
		"proxy.example.com",
	}
	if sans[0] != want[0] {
		t.Errorf("first SAN = %q, want the pod FQDN", sans[0])
	}
	rest := slices.Clone(sans[1:])
	slices.Sort(rest)
	wantRest := slices.Clone(want[1:])
	slices.Sort(wantRest)
	if !reflect.DeepEqual(rest, wantRest) {
		t.Errorf("SANs = %v, want %v (deduplicated, port stripped)", sans, want)
	}
}

func TestSharedCertificateSANsChangeWithProxyHosts(t *testing.T) {
	cluster := exposureCluster(true)
	before := stringsHash(sharedCertificateSANs(cluster, &cluster.Spec.NodeGroups[0]))
	cluster.Spec.ProxyHosts = []string{"nifi.example.com"}
	if after := stringsHash(sharedCertificateSANs(cluster, &cluster.Spec.NodeGroups[0])); after == before {
		t.Error("adding a proxy host must change the SAN hash, so pods roll onto certificates that include it")
	}
}

func TestEffectiveProxyHosts(t *testing.T) {
	cluster := exposureCluster(false)
	cluster.Spec.ProxyHosts = []string{"p.example.com"}
	if got, want := effectiveProxyHosts(cluster), []string{"p.example.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveProxyHosts = %v, want %v", got, want)
	}
}

func TestHostWithoutPort(t *testing.T) {
	for in, want := range map[string]string{"a.example.com": "a.example.com", "a.example.com:8443": "a.example.com", "[::1]": "[::1]"} {
		if got := hostWithoutPort(in); got != want {
			t.Errorf("hostWithoutPort(%q) = %q, want %q", in, got, want)
		}
	}
}

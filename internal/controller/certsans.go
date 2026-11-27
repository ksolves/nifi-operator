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
	"sort"
	"strings"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// effectiveProxyHosts returns the Host headers NiFi must accept besides its own: spec.proxyHosts.
func effectiveProxyHosts(cluster *nifiv1alpha1.NiFiCluster) []string {
	return append([]string{}, cluster.Spec.ProxyHosts...)
}

// CertificateSANs returns the DNS names on a NiFi node's leaf certificate: the pod's own FQDN
// (used by its peers), the cluster Service DNS names (verified by reencrypting proxies), every
// proxy host clients use to reach the cluster (NiFi 2 rejects a TLS request whose SNI does not
// match its certificate), and the node group's extraSANs. Shared with the CSI driver, which mints the certificates.
func CertificateSANs(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup, podName string) []string {
	return append([]string{PodFQDN(podName, cluster.Name, nodeGroup.Name, cluster.Namespace)}, sharedCertificateSANs(cluster, nodeGroup)...)
}

// sharedCertificateSANs is CertificateSANs without the pod FQDN: the part every pod of a node
// group shares, sorted and de-duplicated. Its hash is part of the pod template so a change to
// any of these names rolls the pods onto certificates that include it.
func sharedCertificateSANs(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) []string {
	set := map[string]struct{}{}
	add := func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	for _, n := range clusterServiceDNSNames(cluster) {
		add(n)
	}
	for _, h := range effectiveProxyHosts(cluster) {
		add(hostWithoutPort(h))
	}
	if nodeGroup.TLS != nil {
		for _, n := range nodeGroup.TLS.ExtraSANs {
			add(n)
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// hostWithoutPort strips an optional ":port" from a proxy host entry.
func hostWithoutPort(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i+1:], "]") {
		return host[:i]
	}
	return host
}

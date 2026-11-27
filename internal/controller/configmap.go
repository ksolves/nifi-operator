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
	_ "embed"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

const (
	nifiHTTPPort            = 8080
	nifiHTTPSPort           = 8443
	nifiClusterProtocolPort = 11443

	// webPortNameHTTP/webPortNameHTTPS name whichever of nifiHTTPPort/nifiHTTPSPort a Service,
	// container port, or probe/preStop script is currently pointing at, depending on TLS state.
	webPortNameHTTP  = "http"
	webPortNameHTTPS = "https"

	// nifi.properties keys the operator sets itself, named once here (rather than as repeated
	// string literals in buildNifiProperties/tlsProperties/ReservedNifiPropertyKeys) so
	// golangci-lint's goconst check doesn't flag them, and so a typo in one place can't silently
	// diverge from the same key used elsewhere.
	nifiPropFlowConfigurationFile      = "nifi.flow.configuration.file"
	nifiPropDatabaseDirectory          = "nifi.database.directory"
	nifiPropFlowFileRepoDirectory      = "nifi.flowfile.repository.directory"
	nifiPropContentRepoDirectory       = "nifi.content.repository.directory.default"
	nifiPropProvenanceRepoDirectory    = "nifi.provenance.repository.directory.default"
	nifiPropClusterIsNode              = "nifi.cluster.is.node"
	nifiPropClusterNodeProtocolPort    = "nifi.cluster.node.protocol.port"
	nifiPropClusterFlowElectionMaxWait = "nifi.cluster.flow.election.max.wait.time"
	nifiPropClusterFlowElectionMaxCand = "nifi.cluster.flow.election.max.candidates"
	nifiPropZookeeperConnectString     = "nifi.zookeeper.connect.string"
	nifiPropZookeeperRootNode          = "nifi.zookeeper.root.node"
	nifiPropLeaderElectionImpl         = "nifi.cluster.leader.election.implementation"
	nifiPropLeaderElectionLeasePrefix  = "nifi.cluster.leader.election.kubernetes.lease.prefix"
	nifiPropStateProviderCluster       = "nifi.state.management.provider.cluster"
	nifiPropEmbeddedZookeeperStart     = "nifi.state.management.embedded.zookeeper.start"
	nifiPropWebProxyHost               = "nifi.web.proxy.host"
	nifiPropWebHTTPHost                = "nifi.web.http.host"
	nifiPropWebHTTPPort                = "nifi.web.http.port"
	nifiPropWebHTTPSHost               = "nifi.web.https.host"
	nifiPropWebHTTPSPort               = "nifi.web.https.port"
	nifiPropRemoteInputSecure          = "nifi.remote.input.secure"
	nifiPropRemoteInputHTTPEnabled     = "nifi.remote.input.http.enabled"
	nifiPropSecurityKeystore           = "nifi.security.keystore"
	nifiPropSecurityKeystoreType       = "nifi.security.keystoreType"
	nifiPropSecurityKeystorePasswd     = "nifi.security.keystorePasswd"
	nifiPropSecurityTruststore         = "nifi.security.truststore"
	nifiPropSecurityTruststoreType     = "nifi.security.truststoreType"
	nifiPropSecurityTruststorePasswd   = "nifi.security.truststorePasswd"

	// nifiPropNarLibraryAutoloadDir/nifiPropPythonExtensionsSourceDir are left at the image's own
	// default values (never set by buildNifiProperties) - reserved purely so a ConfigOverrides
	// user can't point them away from the "extensions" PVC's mount paths (statefulset.go's
	// extensionsVolumeName), which would silently break persistence for custom NARs/Python
	// extensions dropped there when NodeGroup.SplitRepositoryStorage is enabled.
	nifiPropNarLibraryAutoloadDir     = "nifi.nar.library.autoload.directory"
	nifiPropPythonExtensionsSourceDir = "nifi.python.extensions.source.directory.default"

	// nifiPropClusterProtocolIsSecure exists only before NiFi 2.7.0, and
	// nifiPropFlowConfigurationJSONFile only in NiFi 1.16 to 1.28 (see buildNifiProperties).
	nifiPropClusterProtocolIsSecure   = "nifi.cluster.protocol.is.secure"
	nifiPropFlowConfigurationJSONFile = "nifi.flow.configuration.json.file"
)

// IsReservedNifiPropertyKey reports whether key is set by the operator for cluster, so a
// ConfigOverrides value for it is rejected (webhook) or ignored (rendering): the keys in
// ReservedNifiPropertyKeys, and a repository key set through a typed Repositories field.
func IsReservedNifiPropertyKey(cluster *nifiv1alpha1.NiFiCluster, key string) bool {
	if _, reserved := ReservedNifiPropertyKeys[key]; reserved {
		return true
	}
	return TypedRepositoryKey(cluster, key)
}

// ReservedNifiPropertyKeys are the nifi.properties keys the operator sets itself (directly in
// buildNifiProperties, or via tlsProperties) - every key that appears in either function's output
// map, listed once here so both the validating webhook and buildNifiProperties' defensive merge
// share a single source of truth instead of two hand-maintained lists drifting apart. A
// user-supplied ConfigOverrides.NifiProperties key matching one of these is rejected by the
// webhook; if it somehow still reaches buildNifiProperties (e.g. the webhook is disabled), the
// operator's own value wins rather than being silently clobbered.
var ReservedNifiPropertyKeys = map[string]struct{}{
	nifiPropFlowConfigurationFile:      {},
	nifiPropDatabaseDirectory:          {},
	nifiPropFlowFileRepoDirectory:      {},
	nifiPropContentRepoDirectory:       {},
	nifiPropProvenanceRepoDirectory:    {},
	nifiPropClusterIsNode:              {},
	nifiPropClusterNodeProtocolPort:    {},
	nifiPropClusterFlowElectionMaxWait: {},
	nifiPropClusterFlowElectionMaxCand: {},
	nifiPropZookeeperConnectString:     {},
	nifiPropZookeeperRootNode:          {},
	nifiPropLeaderElectionImpl:         {},
	nifiPropLeaderElectionLeasePrefix:  {},
	nifiPropStateProviderCluster:       {},
	nifiPropEmbeddedZookeeperStart:     {},
	nifiPropWebProxyHost:               {},
	nifiPropWebHTTPHost:                {},
	nifiPropWebHTTPPort:                {},
	nifiPropWebHTTPSHost:               {},
	nifiPropWebHTTPSPort:               {},
	nifiPropRemoteInputSecure:          {},
	nifiPropRemoteInputHTTPEnabled:     {},
	nifiPropSecurityKeystore:           {},
	nifiPropSecurityKeystoreType:       {},
	nifiPropSecurityKeystorePasswd:     {},
	nifiPropSecurityTruststore:         {},
	nifiPropSecurityTruststoreType:     {},
	nifiPropSecurityTruststorePasswd:   {},
	nifiPropNarLibraryAutoloadDir:      {},
	nifiPropPythonExtensionsSourceDir:  {},
	nifiPropClusterProtocolIsSecure:    {},
	nifiPropFlowConfigurationJSONFile:  {},
}

// nifiEntrypoint is the "nifi" container's command. Shipped in the node group's own Secret
// (buildEntrypointSecret) rather than baked into an image, so stock apache/nifi images run:
// their own start.sh rewrites nifi.properties (cluster.is.node, the ZooKeeper connect string)
// from environment variables the operator does not set. images/nifi/scripts/entrypoint.sh is the
// same file, for custom node images.
//
//go:embed nifi-entrypoint.sh
var nifiEntrypoint string

// entrypointKey is the entrypoint's file name, in the Secret buildEntrypointSecret builds.
const entrypointKey = "entrypoint.sh"

// nifiPropertiesOverridesKey is the file name, in both the ConfigMap and the config Secret, that
// the entrypoint applies onto conf/nifi.properties.
const nifiPropertiesOverridesKey = "nifi.properties.overrides"

// buildConfigMap renders the ConfigMap holding a node group's NiFi configuration overrides.
// These are *overrides*, not full files: NiFi's own nifi.properties (already
// baked into the image with every property NiFi needs to even bootstrap - NAR paths, work
// directories, etc.) are patched in place by the entrypoint, not replaced outright. An earlier
// version of this ConfigMap held complete replacement files with only the ~15 properties this
// operator cares about, which dropped everything else NiFi needs and made it fail to start.
//
// bootstrap.conf.overrides (JVM args - code execution) and logback.xml.override/
// authorizers.xml.override live in the entrypoint Secret instead (buildEntrypointSecret), not
// here: a NiFiCluster's pod Role grants broad access to every ConfigMap in the namespace
// (NIFI-248 B2), so anything that could let a compromised pod from one cluster run code in, or
// change the access policy of, another cluster's pods can't live in the ConfigMap either.
func buildConfigMap(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *corev1.ConfigMap {
	data := map[string]string{
		nifiPropertiesOverridesKey: buildNifiProperties(cluster, nodeGroup),
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        configMapName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      commonLabels(cluster, nodeGroup),
			Annotations: mergedAnnotations(cluster, nodeGroup),
		},
		Data: data,
	}
}

// buildNifiProperties renders the nifi.properties overrides for a node group - only the
// properties that need to differ from the image's own shipped defaults. Everything except the
// node-group-level ConfigOverrides, repository paths, and TLS settings is identical across node
// groups, since all of them join the same NiFi cluster.
//
// repositoryPathProperties returns the five repository-location properties - pointing at the
// combined "data" PVC by default (byte-for-byte unchanged from before NodeGroup.SplitRepositoryStorage
// existed), or at the dedicated per-repository PVCs' mount paths (see statefulset.go's
// flowfileVolumeName/contentVolumeName/provenanceVolumeName/databaseVolumeName) when it's enabled.
func repositoryPathProperties(nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	if !SplitRepositoryStorageEnabled(nodeGroup) {
		// Left at their image defaults (bare "./database_repository" etc., resolving under
		// $NIFI_HOME itself rather than the PVC) would silently lose the deployed flow and all
		// repository data on a restart, so these are always set to live under the "data" PVC.
		return map[string]string{
			nifiPropFlowConfigurationFile:   "./data/flow.json.gz",
			nifiPropDatabaseDirectory:       "./data/database_repository",
			nifiPropFlowFileRepoDirectory:   "./data/flowfile_repository",
			nifiPropContentRepoDirectory:    "./data/content_repository",
			nifiPropProvenanceRepoDirectory: "./data/provenance_repository",
		}
	}
	return map[string]string{
		nifiPropFlowConfigurationFile:   "./database-repository/flow.json.gz",
		nifiPropDatabaseDirectory:       "./database-repository/database_repository",
		nifiPropFlowFileRepoDirectory:   "./flowfile-repository",
		nifiPropContentRepoDirectory:    "./content-repository",
		nifiPropProvenanceRepoDirectory: "./provenance-repository",
	}
}

func buildNifiProperties(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	props := map[string]string{
		// Every node of every node group joins one NiFi cluster. Cluster coordination
		// (ZooKeeper vs Kubernetes Lease) is handled by coordinationProperties below; repository
		// paths by repositoryPathProperties; cluster-protocol TLS follows the
		// keystore/truststore properties - see tlsProperties. The flow-election wait stays at
		// NiFi's own 5 minute default, and max.candidates counts every node across all node
		// groups so a full cold start finishes election as soon as all of them have voted.
		nifiPropClusterIsNode:              "true",
		nifiPropClusterNodeProtocolPort:    fmt.Sprintf("%d", nifiClusterProtocolPort),
		nifiPropClusterFlowElectionMaxWait: "5 mins",
		nifiPropClusterFlowElectionMaxCand: fmt.Sprintf("%d", clusterReplicas(cluster)),
	}

	for k, v := range coordinationProperties(cluster) {
		props[k] = v
	}

	for k, v := range repositoryPathProperties(nodeGroup) {
		props[k] = v
	}

	for k, v := range effectiveRepositoryProperties(cluster, nodeGroup) {
		props[k] = v
	}

	for k, v := range tlsProperties(nodeGroup) {
		props[k] = v
	}
	// NiFi before 2.7.0 still reads nifi.cluster.protocol.is.secure and ships it as false: set it
	// to match TLS so the cluster protocol is not left in plain text. Newer NiFi removed it. A tag
	// without a version on ZooKeeper may be such a NiFi, so it gets the property; newer NiFi
	// ignores it.
	version, versioned := NiFiVersion(cluster)
	if (versioned && VersionLess(version, clusterProtocolIsSecureRemoved)) ||
		(!versioned && coordinationBackend(cluster) == nifiv1alpha1.CoordinationBackendZooKeeper) {
		props[nifiPropClusterProtocolIsSecure] = fmt.Sprintf("%t", nodeGroup.TLS != nil && nodeGroup.TLS.Enabled)
	}
	// NiFi 1.x loads its flow from flow.xml.gz (1.16 on also writes flow.json.gz, and loads it
	// first when present): both live on the volume, or the flow is lost on a restart.
	if versioned && VersionLess(version, nifi2) {
		flow := props[nifiPropFlowConfigurationFile]
		props[nifiPropFlowConfigurationJSONFile] = flow
		props[nifiPropFlowConfigurationFile] = strings.TrimSuffix(flow, ".json.gz") + ".xml.gz"
	}

	// NiFi validates the incoming Host header strictly and rejects anything not on this
	// comma-separated allow-list - required for LoadBalancer, Ingress or Gateway API access (or any other
	// proxy/exposure sitting in front of the ClusterIP Service) to work at all. Left unset
	// (property omitted) when nothing's configured, matching every existing cluster's behavior
	// before this field existed.
	if hosts := effectiveProxyHosts(cluster); len(hosts) > 0 {
		props[nifiPropWebProxyHost] = strings.Join(hosts, ",")
	}

	for k, v := range effectiveConfigOverrides(cluster, nodeGroup) {
		if IsReservedNifiPropertyKey(cluster, k) {
			// Should already be rejected by the validating webhook; skipped rather than
			// applied here purely as defense in depth (e.g. the webhook is disabled), so the
			// operator's own value always wins over a user override for a key it manages.
			continue
		}
		props[k] = v
	}

	return renderProperties(props)
}

// OverrideKeyError returns why key can't be used as a nifi.properties/bootstrap.conf override
// key, or "" when it can. Java Properties ends a key at the first unescaped whitespace, '=' or
// ':', starts a comment at a leading '#' or '!', and decodes '\' escapes (including \uXXXX) in
// keys - so any of those would let a key that doesn't textually match a reserved one still land
// on it once NiFi loads the file (e.g. "nifi\u002ecluster.is.node"). A line break would start a
// second, unvalidated property line.
func OverrideKeyError(key string) string {
	switch {
	case key == "":
		return "must not be empty"
	case strings.ContainsAny(key, " \t\f\r\n=:\\"):
		return `must not contain whitespace, line breaks, '=', ':' or '\'`
	case key[0] == '#' || key[0] == '!':
		return "must not start with '#' or '!'"
	}
	return ""
}

// OverrideValueError returns why value can't be used as an override value, or "" when it can. A
// line break would end the value and inject a second property line (e.g. a java.arg or a
// reserved nifi.properties key) past every reserved-key check.
func OverrideValueError(value string) string {
	if strings.ContainsAny(value, "\r\n") {
		return "must not contain line breaks"
	}
	return ""
}

// renderProperties writes props as sorted key=value lines in Java Properties syntax. Values are
// escaped so they reach NiFi verbatim: Java Properties treats '\' as an escape character, so a
// Windows-style path like C:\data would otherwise load as C:data. Entries that fail
// OverrideKeyError/OverrideValueError are dropped - the webhook rejects them for values in the
// CR, but Secret-sourced overrides never pass through the webhook.
func renderProperties(props map[string]string) string {
	keys := make([]string, 0, len(props))
	for k, v := range props {
		if OverrideKeyError(k) != "" || OverrideValueError(v) != "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, strings.ReplaceAll(props[k], `\`, `\\`))
	}
	return b.String()
}

// effectiveConfigOverrides merges cluster-level and node-group-level NifiProperties overrides,
// with the node group's own entries taking precedence over the cluster's for the same key - the
// same cluster-then-node-group precedence used by Annotations/Labels elsewhere on this type.
func effectiveConfigOverrides(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	merged := map[string]string{}
	if co := cluster.Spec.ConfigOverrides; co != nil {
		for k, v := range co.NifiProperties {
			merged[k] = v
		}
	}
	if co := nodeGroup.ConfigOverrides; co != nil {
		for k, v := range co.NifiProperties {
			merged[k] = v
		}
	}
	return merged
}

// effectiveBootstrapConfOverrides merges cluster-level and node-group-level BootstrapConf
// overrides, node-group winning for the same key - same precedence as effectiveConfigOverrides.
// Unlike NifiProperties, nothing here is operator-reserved: the operator does not set anything in
// bootstrap.conf itself today.
func effectiveBootstrapConfOverrides(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	merged := map[string]string{}
	if co := cluster.Spec.ConfigOverrides; co != nil {
		for k, v := range co.BootstrapConf {
			merged[k] = v
		}
	}
	if co := nodeGroup.ConfigOverrides; co != nil {
		for k, v := range co.BootstrapConf {
			merged[k] = v
		}
	}
	return merged
}

// buildBootstrapConfOverrides renders the bootstrap.conf.overrides ConfigMap entry - possibly
// empty (never nil), always included in buildConfigMap's Data so the entrypoint's require_file
// check always finds it (applying an empty overrides file onto bootstrap.conf is a harmless
// no-op), the same approach as nifi.properties.overrides.
func buildBootstrapConfOverrides(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return renderProperties(effectiveBootstrapConfOverrides(cluster, nodeGroup))
}

// effectiveLogback/effectiveAuthorizers return the raw XML content to fully replace
// logback.xml/authorizers.xml with - the node group's own value taking precedence over the
// cluster-level one when both are set, nil (no replacement) when neither is set.
func effectiveLogback(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *string {
	if co := nodeGroup.ConfigOverrides; co != nil && co.Logback != nil {
		return co.Logback
	}
	if co := cluster.Spec.ConfigOverrides; co != nil && co.Logback != nil {
		return co.Logback
	}
	return nil
}

func effectiveAuthorizers(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *string {
	if co := nodeGroup.ConfigOverrides; co != nil && co.Authorizers != nil {
		return co.Authorizers
	}
	if co := cluster.Spec.ConfigOverrides; co != nil && co.Authorizers != nil {
		return co.Authorizers
	}
	return nil
}

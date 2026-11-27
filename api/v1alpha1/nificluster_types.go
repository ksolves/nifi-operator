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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CEL rules on NiFiClusterSpec below are enforced by the API server even with the
// admission webhook disabled.

// MaxClusterNodes is the most NiFi nodes one NiFiCluster may have, across all of its node groups
// (inline and NiFiNodeGroup objects). The CRD markers below repeat the number.
const MaxClusterNodes = 3

// NiFiClusterSpec defines the desired state of NiFiCluster.
// +kubebuilder:validation:XValidation:rule="!has(self.nodeGroups) || self.nodeGroups.map(g, has(g.replicas) ? g.replicas : 3).sum() <= 3 || (oldSelf.hasValue() && has(oldSelf.value().nodeGroups) && self.nodeGroups.map(g, has(g.replicas) ? g.replicas : 3).sum() <= oldSelf.value().nodeGroups.map(g, has(g.replicas) ? g.replicas : 3).sum())",message="a NiFiCluster has at most 3 NiFi nodes in total, across all node groups, unless this update does not raise that total above what it already was (a cluster already over the limit may stay there or step down, but not up)",optionalOldSelf=true
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.coordination) || (has(self.coordination) && self.coordination.backend == oldSelf.coordination.backend)",message="spec.coordination.backend is immutable once set and cannot be removed",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.nodeGroups) || self.nodeGroups.all(g, (has(g.tls) && g.tls.enabled) == (has(self.nodeGroups[0].tls) && self.nodeGroups[0].tls.enabled))",message="tls.enabled must be the same in every node group: they form one NiFi cluster"
type NiFiClusterSpec struct {
	// Image is the container image to use for NiFi nodes: the stock apache/nifi image or one built
	// on it. Leave it unset to run the operator's default (its --default-nifi-image, by default
	// apache/nifi:2.12.0); set both repository and tag, or neither. NiFi 2.11.0 or later is required with
	// Kubernetes coordination, 1.14.0 or later with ZooKeeper (docs/compatibility.md).
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// NodeGroups defines the cluster's node group inline. The Community edition allows one node
	// group (and no new NiFiNodeGroup objects), of at most 3 NiFi nodes. NiFiNodeGroups created by
	// earlier versions still count towards the 3-node total.
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:XValidation:rule="oldSelf.size() != 1 || self.size() != 1 || self[0].name == oldSelf[0].name || !isQuantity(self[0].storage) || !isQuantity(oldSelf[0].storage) || quantity(self[0].storage).compareTo(quantity(oldSelf[0].storage)) == 0",message="storage: a renamed node group keeps the storage size of the one it replaces: size changes are not supported in the Community edition",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="oldSelf.size() != 1 || self.size() != 1 || self[0].name == oldSelf[0].name || !has(self[0].logStorage) || !has(oldSelf[0].logStorage) || !isQuantity(self[0].logStorage) || !isQuantity(oldSelf[0].logStorage) || quantity(self[0].logStorage).compareTo(quantity(oldSelf[0].logStorage)) == 0",message="logStorage: a renamed node group keeps the log storage size of the one it replaces: size changes are not supported in the Community edition",reason="FieldValueForbidden"
	// +listType=map
	// +listMapKey=name
	// +optional
	NodeGroups []NodeGroup `json:"nodeGroups,omitempty"`

	// ServiceType is the Kubernetes Service type used for client access to the cluster.
	// ClusterIP (the default) keeps the cluster in-cluster only; LoadBalancer provisions an
	// external load balancer (tune it via service) and needs TLS on the node groups: the webhook
	// rejects it otherwise. For HTTP
	// exposure, point your own Ingress, Gateway API HTTPRoute or OpenShift Route at the cluster
	// Service (named after the cluster).
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`

	// Service tunes the cluster Service (the one spanning every node group, which serviceType
	// applies to): its annotations and load balancer settings.
	// +optional
	Service *ClusterServiceSpec `json:"service,omitempty"`

	// ProxyHosts is the explicit list of hostnames (and optionally ports) clients will use to
	// reach this cluster - via a LoadBalancer, an Ingress, a Gateway API route, or anything else
	// in front of the ClusterIP Service. NiFi validates the incoming Host header strictly
	// (nifi.web.proxy.host) and rejects anything not on this list, so it must be kept in sync
	// with whatever's actually routing traffic here. Not required when only accessing the
	// cluster via its in-cluster ClusterIP Service/DNS name, which is always implicitly
	// allowed.
	// +optional
	ProxyHosts []string `json:"proxyHosts,omitempty"`

	// Annotations are propagated to every object generated for this cluster, in addition to
	// any NodeGroup-level annotations.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Labels are propagated to every object generated for this cluster, in addition to any
	// NodeGroup-level labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// ServiceAccountName is the ServiceAccount every NiFi node pod runs as. When unset, the
	// operator creates a dedicated one (<name>-nifi) and, on OpenShift, binds it to the configured
	// SecurityContextConstraints. A ServiceAccount named here gets the Kubernetes coordination
	// Role (Leases and ConfigMaps in this namespace) bound to it, but never an SCC: grant it one
	// yourself, since the operator must not hand out SCC access to arbitrary ServiceAccounts.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// PodSecurityContext is merged, field by field, over the NiFi pods' own security context
	// (seccompProfile RuntimeDefault, fsGroup 0 with fsGroupChangePolicy OnRootMismatch; on
	// OpenShift without a configured SCC no fsGroup, so restricted-v2 assigns one). A node
	// group's podOverrides.podSecurityContext is merged over this in turn. Setting it requires
	// permission to create pods in the namespace.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// ContainerSecurityContext is merged, field by field, over the "nifi" container's own
	// security context (runAsNonRoot, no privilege escalation, all capabilities dropped,
	// readOnlyRootFilesystem, seccompProfile RuntimeDefault); a node group's
	// podOverrides.containerSecurityContext is merged over this in turn. Setting it requires
	// permission to create pods in the namespace.
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// ImagePullSecrets are referenced by every NiFi node pod to pull Image.Repository from a
	// private registry. Without this, a private-registry image requires manually patching the
	// namespace's default ServiceAccount after the fact.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Authentication configures how users log into the NiFi UI/API. Cluster-scoped (not
	// per-node-group like TLS): NiFi's login identity is one logical thing shared by every node
	// in the cluster, since they all serve the same flow. If unset, the operator generates one
	// shared login (user "admin", random password) in the Secret <name>-auth-credentials.
	// singleUser sets the login yourself (literal username/password or secretName; the password
	// is stored as a bcrypt hash and must be at least 12 characters and at most 72 bytes
	// (bcrypt's limit)).
	// +optional
	Authentication *AuthenticationSpec `json:"authentication,omitempty"`

	// ConfigOverrides sets additional NiFi configuration properties, merged with (and
	// overridden by) any NodeGroup-level ConfigOverrides. Lets a user tune NiFi behavior the
	// operator doesn't otherwise expose (e.g. UI session timeouts, provenance query limits)
	// without building a custom image. Keys the operator manages itself (ZooKeeper
	// coordination, cluster protocol, repository paths, TLS keystore/truststore, proxy host)
	// are rejected by the validating webhook.
	// +optional
	ConfigOverrides *ConfigOverridesSpec `json:"configOverrides,omitempty"`

	// Coordination selects how the NiFi nodes of this cluster elect their Cluster Coordinator and
	// Primary Node and where they keep cluster-wide component state. Every node group in the
	// NiFiCluster joins one single NiFi cluster through this backend. Unset means Kubernetes.
	// Deliberately not defaulted by the CRD: a CRD default is also applied to stored objects on
	// read, which would make the immutability rule above lock clusters that never chose a backend.
	// +optional
	Coordination *CoordinationSpec `json:"coordination,omitempty"`

	// PersistentVolumeClaimRetentionPolicy decides what happens to the PVCs of every node
	// group's repositories and logs when a node group's StatefulSet is deleted (the NiFiCluster
	// is deleted, or the node group is removed) or scaled down. A node group's own
	// PersistentVolumeClaimRetentionPolicy overrides it field by field. Unset fields mean Retain,
	// the behavior of every earlier operator version.
	// +optional
	PersistentVolumeClaimRetentionPolicy *PVCRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy,omitempty"`

	// Repositories tunes how long and how much the content archive, the provenance repository and
	// the FlowFile repository keep. A node group's own Repositories overrides it field by
	// field; an unset field keeps NiFi's default. A key set here can't also be set in
	// configOverrides. Changing it restarts the nodes, like any nifi.properties change.
	// +optional
	Repositories *RepositoriesSpec `json:"repositories,omitempty"`
}

// CoordinationBackend names a NiFi cluster coordination backend.
type CoordinationBackend string

const (
	// CoordinationBackendKubernetes uses NiFi 2.x's native Kubernetes support: Lease objects for
	// leader election (KubernetesLeaderElectionManager) and ConfigMaps for cluster state
	// (KubernetesConfigMapStateProvider). No ZooKeeper is deployed or required.
	CoordinationBackendKubernetes CoordinationBackend = "Kubernetes"

	// CoordinationBackendZooKeeper uses an existing, externally managed ZooKeeper ensemble for
	// leader election (CuratorLeaderElectionManager) and cluster state (ZooKeeperStateProvider).
	// The operator never deploys ZooKeeper itself. Not accepted in spec by the Community edition;
	// clusters created with it by earlier versions keep running on it.
	CoordinationBackendZooKeeper CoordinationBackend = "ZooKeeper"
)

// CoordinationSpec configures cluster coordination.
// +kubebuilder:validation:XValidation:rule="self.backend == 'ZooKeeper' ? has(self.zookeeper) : !has(self.zookeeper)",message="zookeeper must be set when backend is ZooKeeper, and only then"
type CoordinationSpec struct {
	// Backend is the coordination backend; defaults to Kubernetes when coordination is present (the
	// coordination field itself has no CRD default, see above). Immutable once set (see the rule
	// on NiFiClusterSpec):
	// switching the leader-election and state backend of a running NiFi cluster risks split-brain
	// and loses component state. Whenever the backend the running pods were built for differs from
	// this one, the operator only switches through an explicitly approved full-stop migration.
	// The Community edition accepts Kubernetes only: external ZooKeeper is not available.
	// +kubebuilder:validation:Enum=Kubernetes
	// +kubebuilder:default=Kubernetes
	// +optional
	Backend CoordinationBackend `json:"backend,omitempty"`

	// ZooKeeper configures the external ensemble of a cluster on the ZooKeeper backend, which only
	// clusters created by earlier versions can have.
	// +optional
	ZooKeeper *ZooKeeperSpec `json:"zookeeper,omitempty"`

	// BindPodServiceAccounts asks the operator to also bind the Kubernetes coordination Role
	// (Leases and ConfigMaps in this namespace) to every node group's
	// podOverrides.serviceAccountName, not only to the cluster's own ServiceAccount.
	// It takes effect only when the operator admin has enabled it too
	// (--allow-custom-service-account-binding, chart value
	// coordination.allowCustomServiceAccountBinding), and you may only set it if you yourself may
	// update Leases and ConfigMaps in this namespace. Without it, bind the Role yourself; the
	// CoordinationRBACReady condition reports any ServiceAccount still missing it.
	// +optional
	BindPodServiceAccounts bool `json:"bindPodServiceAccounts,omitempty"`
}

// ZooKeeperSpec points the cluster at an existing ZooKeeper ensemble.
// +kubebuilder:validation:XValidation:rule="has(self.rootNode) == has(oldSelf.rootNode) && (!has(self.rootNode) || self.rootNode == oldSelf.rootNode)",message="coordination.zookeeper.rootNode is immutable: it cannot be added, changed or removed after creation"
type ZooKeeperSpec struct {
	// ConnectString lists the ensemble's servers as comma-separated host:port pairs (e.g.
	// "zk-0.zk:2181,zk-1.zk:2181,zk-2.zk:2181"), without a chroot suffix - use RootNode for that.
	// Changing it is only safe while it still reaches the same ensemble (e.g. adding a server):
	// nodes restart one at a time, and nodes on two different ensembles would form two clusters.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ConnectString string `json:"connectString"`

	// RootNode is the znode under which this cluster keeps its leader election and state.
	// Defaults to /nifi/<namespace>/<name>, unique per NiFiCluster: two clusters sharing a root
	// node would merge into one. Immutable once set, for the same reason as the backend.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^(/[A-Za-z0-9_.\-]+)+$`
	// +optional
	RootNode string `json:"rootNode,omitempty"`
}

// ConfigOverridesSpec sets raw configuration key/value pairs the operator merges into its own
// generated configuration: key-level merges into nifi.properties and bootstrap.conf, and
// full-file replacement of logback.xml and authorizers.xml.
type ConfigOverridesSpec struct {
	// NifiProperties are additional "key: value" pairs merged into nifi.properties. A key
	// already set by the operator itself is rejected by the validating webhook rather than
	// silently ignored or silently overwritten - see the webhook's reserved-key check.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	NifiProperties map[string]string `json:"nifiProperties,omitempty"`

	// BootstrapConf are additional "key: value" pairs merged into bootstrap.conf - most commonly
	// JVM heap settings (e.g. "java.arg.3": "-Xmx4g"). The operator's entrypoint applies these the
	// same way it applies NifiProperties.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	BootstrapConf map[string]string `json:"bootstrapConf,omitempty"`

	// Logback, when set, fully replaces NiFi's logback.xml with this raw XML content. Unlike
	// NifiProperties/BootstrapConf above, logback.xml is structured XML, not a flat key=value
	// file, so a key-level merge doesn't apply here - the whole file is replaced instead. The
	// operator does not validate the XML's correctness beyond rejecting an empty value; a
	// malformed file will fail at NiFi startup.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Logback *string `json:"logback,omitempty"`

	// Authorizers, when set, fully replaces NiFi's authorizers.xml with this raw XML content -
	// same full-file-replacement behavior and validation caveat as Logback above.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Authorizers *string `json:"authorizers,omitempty"`

	// SecretName optionally names a Secret (in the NiFiCluster's own namespace) whose data is
	// applied as additional nifi.properties overrides, for values too sensitive to store as
	// plaintext in the CR (NifiProperties is visible via `kubectl get nificluster -o yaml`).
	// Precedence, lowest first: cluster NifiProperties < cluster Secret < node-group
	// NifiProperties < node-group Secret. Secret-sourced values are delivered through an
	// operator-owned Secret (<cluster>-<nodeGroup>-config-secret), never the ConfigMap, and
	// rotating the Secret rolls the pods. Setting or changing this field requires permission to
	// get that Secret. Reserved keys are skipped even when sourced from a Secret.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// AuthenticationSpec configures how users authenticate to the NiFi UI/API. When left unset
// entirely, the operator still generates a Secret-backed login by default (username "admin",
// a random password), shared by every node in the cluster - rather than leaving each node to
// fall back to NiFi's own per-node random single-user credentials, which differ from node to
// node and make a multi-node cluster's UI effectively impossible to log into consistently.
// Retrieve the generated password with:
// `kubectl get secret <cluster>-auth-credentials -o jsonpath='{.data.password}' | base64 -d`.
type AuthenticationSpec struct {
	// SingleUser configures NiFi's built-in single-user login provider with a specific,
	// known-ahead-of-time username/password, instead of the operator's own generated default
	// credentials, given literally or through a Secret (which can be externally managed, e.g.
	// synced from Vault or AWS Secrets Manager).
	// +optional
	SingleUser *SingleUserAuthSpec `json:"singleUser,omitempty"`
}

// SingleUserAuthSpec sets NiFi's single-user login credentials, from exactly one of two
// mutually exclusive sources - enforced by the validating webhook, since the CRD schema alone
// can't express "exactly one of A or B".
// +kubebuilder:validation:XValidation:rule="(has(self.secretName) && size(self.secretName) > 0) != ((has(self.username) && size(self.username) > 0) || (has(self.password) && size(self.password) > 0))",message="exactly one of username/password or secretName must be set"
// +kubebuilder:validation:XValidation:rule="(has(self.username) && size(self.username) > 0) == (has(self.password) && size(self.password) > 0)",message="username and password must be set together"
// +kubebuilder:validation:XValidation:rule="!has(self.password) || size(self.password) == 0 || size(self.password) >= 12",message="password must be at least 12 characters (NiFi single-user login minimum)"
type SingleUserAuthSpec struct {
	// Username, together with Password, sets the login credentials directly as plain values in
	// this CR. Simple, but stored unencrypted in this object (visible via `kubectl get
	// nificluster -o yaml`, and in etcd) - acceptable for early-stage/internal use, not
	// recommended once this matters for a real production credential. Mutually exclusive with
	// SecretName.
	// +optional
	Username string `json:"username,omitempty"`

	// Password, see Username above. Must be at least 12 characters (NiFi's single-user login
	// minimum). Enforced by the validating webhook rather than a CRD MinLength marker: the API
	// server's schema-validation error echoes the rejected value, which would leak the password.
	// +optional
	Password string `json:"password,omitempty"`

	// SecretName is the name of an existing Secret (in the NiFiCluster's own namespace)
	// containing "username" and "password" keys - keeps the actual credential values out of
	// this CR entirely. Mutually exclusive with Username/Password. Changing the Secret's
	// contents rolls the cluster's pods so the new credentials take effect.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// ClusterServiceSpec configures the cluster Service.
type ClusterServiceSpec struct {
	// Annotations set on the cluster Service only (e.g. a cloud provider's load balancer
	// settings), in addition to spec.annotations.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// LoadBalancerClass selects the load balancer implementation when serviceType is
	// LoadBalancer. Immutable on the Service once set, as Kubernetes defines it.
	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`

	// LoadBalancerSourceRanges restricts which client CIDRs may reach a LoadBalancer Service.
	// Strongly recommended for anything internet-facing.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

	// ExternalTrafficPolicy for a LoadBalancer Service. Local keeps the client's
	// source IP, which the Service's ClientIP session affinity (NiFi's UI needs one node per
	// session) relies on; Cluster (the Kubernetes default) hides it behind the node.
	// +kubebuilder:validation:Enum=Cluster;Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
}

// ImageSpec defines the container image to run for NiFi nodes.
type ImageSpec struct {
	// Repository is the image repository (without tag)
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tag is the image tag to use
	// +optional
	Tag string `json:"tag,omitempty"`
}

// NodeGroup defines a group of NiFi nodes sharing the same configuration.
// +kubebuilder:validation:XValidation:rule="!has(self.podDisruptionBudget) || !has(self.podDisruptionBudget.minAvailable) || self.podDisruptionBudget.minAvailable < self.replicas",message="podDisruptionBudget.minAvailable must be lower than replicas, or no node could ever be drained"
// +kubebuilder:validation:XValidation:rule="!isQuantity(self.storage) || !isQuantity(oldSelf.storage) || quantity(self.storage).compareTo(quantity(oldSelf.storage)) == 0",message="storage: storage size changes are not supported in the Community edition; expand the PVCs with kubectl patch pvc",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.logStorage) || !has(oldSelf.logStorage) || !isQuantity(self.logStorage) || !isQuantity(oldSelf.logStorage) || quantity(self.logStorage).compareTo(quantity(oldSelf.logStorage)) == 0",message="logStorage: storage size changes are not supported in the Community edition; expand the PVCs with kubectl patch pvc",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="has(self.storageClassName) == has(oldSelf.storageClassName) && (!has(self.storageClassName) || self.storageClassName == oldSelf.storageClassName)",message="storageClassName cannot be changed on an existing node group",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="(has(self.splitRepositoryStorage) && self.splitRepositoryStorage.enabled) == (has(oldSelf.splitRepositoryStorage) && oldSelf.splitRepositoryStorage.enabled)",message="splitRepositoryStorage.enabled cannot be changed on an existing node group",reason="FieldValueForbidden"
type NodeGroup struct {
	// Name is the identifier for this node group. Combined with the NiFiCluster's own name and
	// suffixes (e.g. "-headless") to build every generated object's name, which for Services
	// and StatefulSet-derived pod hostnames must be a valid DNS label of at most 63 characters
	// (RFC 1035) - so this is kept well short of that ceiling to leave room for the cluster
	// name and suffixes on either side.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Replicas is the number of nodes in this group. A cluster has at most 3 nodes in total,
	// across all of its node groups.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self <= 3 || (oldSelf.hasValue() && self <= oldSelf.value())",message="at most 3 replicas in one node group, unless this update does not raise an already over-limit value",optionalOldSelf=true
	Replicas int32 `json:"replicas,omitempty"`

	// Storage is the persistent volume size for the NiFi repositories on each node (e.g. "10Gi").
	// Can only grow (online expansion needs an expandable StorageClass).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity, e.g. 10Gi"
	Storage string `json:"storage"`

	// LogStorage is the persistent volume size for NiFi's logs on each node (e.g. "5Gi").
	// Logs are always stored on a dedicated PVC, never on emptyDir. Can only grow (online
	// expansion needs an expandable StorageClass).
	// +kubebuilder:default="5Gi"
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity, e.g. 5Gi"
	LogStorage string `json:"logStorage,omitempty"`

	// StorageClassName is the StorageClass used for this node group's PVCs. NFS- and
	// CIFS-backed StorageClasses are rejected by the validating webhook. Immutable.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// SplitRepositoryStorage, when enabled, provisions separate PVCs for NiFi's flowfile,
	// content, provenance, and database repositories (instead of nesting them all under the
	// single Storage PVC), plus a dedicated, persistent "extensions" PVC for custom NARs and
	// custom Python extensions (NiFi auto-loads these from
	// nifi.nar.library.autoload.directory / nifi.python.extensions.source.directory.default,
	// which otherwise resolve to the image's own ephemeral filesystem - anything dropped there
	// today is silently lost on a pod restart). Off by default: an existing cluster's generated
	// PVCs and mount paths stay byte-for-byte unchanged unless this is explicitly enabled. Only
	// takes effect at node-group creation - the validating webhook rejects changing whether
	// this is enabled on an existing node group, since the operator has no mechanism to migrate
	// already-written data (flow definition, content, provenance history) from the combined
	// Storage PVC to the new per-repository layout.
	// +optional
	SplitRepositoryStorage *SplitRepositoryStorageSpec `json:"splitRepositoryStorage,omitempty"`

	// PersistentVolumeClaimRetentionPolicy overrides the cluster-level
	// PersistentVolumeClaimRetentionPolicy for this node group, field by field: an unset field
	// falls back to the cluster's, then to Retain. Can be changed at any time; the StatefulSet is
	// updated in place without restarting pods.
	// +optional
	PersistentVolumeClaimRetentionPolicy *PVCRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy,omitempty"`

	// Repositories overrides the cluster-level Repositories for this node group, field by field.
	// +optional
	Repositories *RepositoriesSpec `json:"repositories,omitempty"`

	// Resources defines CPU/memory requests and limits for each node
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// PodDisruptionBudget configures voluntary-disruption protection for this node group.
	// +optional
	PodDisruptionBudget *PodDisruptionBudgetSpec `json:"podDisruptionBudget,omitempty"`

	// TerminationGracePeriodSeconds is the grace period given to a NiFi node to offload
	// data and disconnect from the cluster before it is force-killed.
	// +kubebuilder:default=120
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// Annotations are propagated to every object generated for this node group, merged
	// with (and taking precedence over) the cluster-level Annotations.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Labels are propagated to every object generated for this node group, merged with
	// (and taking precedence over) the cluster-level Labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// TLS configures mutual TLS for this node group's REST API and cluster protocol traffic.
	// Off by default, preserving the unsecured HTTP behavior of existing clusters that don't
	// opt in.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// ConfigOverrides sets additional NiFi configuration properties for this node group,
	// merged with (and taking precedence over) the cluster-level ConfigOverrides.
	// +optional
	ConfigOverrides *ConfigOverridesSpec `json:"configOverrides,omitempty"`

	// PodOverrides customizes this node group's generated Pod template beyond what the fields
	// above already expose (Resources, TLS, etc.), without needing a custom image. Each field is
	// additive to, or fully replaces, one specific part of the pod spec - see PodOverridesSpec's
	// own field docs. A value colliding with something the operator manages itself (the "nifi"
	// container name, its own env vars, or its own volume names) is rejected by the validating
	// webhook.
	// +optional
	PodOverrides *PodOverridesSpec `json:"podOverrides,omitempty"`
}

// PodOverridesSpec customizes a node group's generated Pod template. Scoped per node group
// (unlike ConfigOverrides) since pod templates - scheduling, sidecars, extra volumes - are
// already inherently per-node-group, one per StatefulSet.
// +kubebuilder:validation:XValidation:rule="!has(self.env) || self.env.all(e, !(e.name in ['POD_NAME', 'POD_NAMESPACE', 'NIFI_CLUSTER_DOMAIN', 'NIFI_SENSITIVE_PROPS_KEY', 'NIFI_AUTH_USERNAME', 'NIFI_AUTH_PASSWORD', 'NIFI_AUTH_PASSWORD_HASH', 'NIFI_KEYSTORE_PASSWORD']))",message="env must not set a variable the operator sets itself"
// +kubebuilder:validation:XValidation:rule="(!has(self.initContainers) || self.initContainers.all(c, !(c.name in ['nifi', 'nifi-init']))) && (!has(self.sidecars) || self.sidecars.all(c, !(c.name in ['nifi', 'nifi-init'])))",message="initContainers and sidecars must not be named nifi or nifi-init"
// +kubebuilder:validation:XValidation:rule="!has(self.volumes) || self.volumes.all(v, !(v.name in ['data', 'logs', 'config', 'config-secret', 'entrypoint', 'tls', 'flowfile-repository', 'content-repository', 'provenance-repository', 'database-repository', 'extensions', 'nifi-scratch', 'nifi-conf', 'nifi-passwd']))",message="volumes must not use a name the operator uses itself"
type PodOverridesSpec struct {
	// Env adds extra environment variables to the "nifi" container, alongside the ones the
	// operator sets itself (POD_NAME, POD_NAMESPACE, NIFI_CLUSTER_DOMAIN,
	// NIFI_SENSITIVE_PROPS_KEY, NIFI_AUTH_USERNAME, NIFI_AUTH_PASSWORD,
	// NIFI_AUTH_PASSWORD_HASH and, on TLS node groups, NIFI_KEYSTORE_PASSWORD). A name colliding with one of those is rejected by the validating
	// webhook, since silently shadowing one could break the entrypoint or authentication.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Env []corev1.EnvVar `json:"env,omitempty"`

	// NodeSelector constrains which nodes this node group's pods can be scheduled on.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let this node group's pods be scheduled onto nodes with matching taints.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity, when set, fully replaces the operator's own default pod anti-affinity (which
	// otherwise spreads this node group's pods across hosts) - useful e.g. on a single-node dev
	// cluster. The default zone topology spread constraint stays (see TopologySpreadConstraints);
	// it is ScheduleAnyway, so it never blocks scheduling, only prefers spreading across zones.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints, when set, fully replace the operator's default one (spread this
	// node group's pods across topology.kubernetes.io/zone with maxSkew 1, ScheduleAnyway) -
	// e.g. to require zone spreading with whenUnsatisfiable: DoNotSchedule, or to spread across a
	// custom topology label. A constraint without a labelSelector gets this node
	// group's pod labels, so it counts exactly this node group's pods.
	// +optional
	// +listType=atomic
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName sets this node group's pod priority class. Unset by default (the
	// cluster's own default priority applies).
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// PodSecurityContext is merged, field by field, over the pod security context from the
	// operator's defaults and the cluster's spec.podSecurityContext.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// ContainerSecurityContext is merged, field by field, over the "nifi" container's security
	// context from the operator's defaults and the cluster's spec.containerSecurityContext.
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// InitContainers adds extra init containers to the pod, run in order (after the operator's
	// own "nifi-init") before the "nifi" container starts. A container named "nifi" or
	// "nifi-init" is rejected. One without a securityContext gets the restricted defaults of the
	// "nifi" container.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Sidecars adds extra containers to the pod, running alongside the "nifi" container. A
	// container named "nifi" or "nifi-init" is rejected. One without a securityContext gets the
	// restricted defaults of the "nifi" container.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Sidecars []corev1.Container `json:"sidecars,omitempty"`

	// Volumes adds extra volumes to the pod, for InitContainers/Sidecars to mount (e.g. a
	// shared emptyDir, a mounted Secret/ConfigMap). A name colliding with one of the operator's
	// own volumes ("data", "logs", "config", "tls", "nifi-scratch", ...) is rejected by the
	// validating webhook.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// ImagePullPolicy sets the "nifi" container's image pull policy. Unset by default, which
	// leaves Kubernetes' own standard default in effect (Always for a ":latest" tag,
	// IfNotPresent otherwise) - unchanged from the behavior before this field existed.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets overrides this node group's image pull secrets, taking precedence over
	// the cluster-level Spec.ImagePullSecrets when set. Falls back to the cluster-level value
	// when unset - so most clusters never need to set this per node group.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// ServiceAccountName overrides this node group's ServiceAccount, taking precedence over
	// NiFiClusterSpec.ServiceAccountName when set. Falls back to the cluster-level value (and
	// from there, the operator's own dedicated ServiceAccount) when unset.
	//
	// With Kubernetes-Lease cluster coordination, this ServiceAccount needs the coordination
	// Role (Leases and ConfigMaps in this namespace). The operator binds it only when both the
	// operator admin (--allow-custom-service-account-binding) and this cluster
	// (coordination.bindPodServiceAccounts) opt in - the same no-automatic-elevation policy as
	// for OpenShift SCCs, which it never binds to a ServiceAccount you name. Otherwise bind it
	// yourself: kubectl create rolebinding <cluster>-nifi-<sa> --role=<cluster>-nifi
	// --serviceaccount=<namespace>:<sa>. The CoordinationRBACReady condition reports any
	// ServiceAccount still missing it.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// HostAliases adds extra entries to the pod's /etc/hosts. None by default.
	// +optional
	HostAliases []corev1.HostAlias `json:"hostAliases,omitempty"`
}

// RepositoriesSpec holds typed nifi.properties settings for NiFi's repositories.
type RepositoriesSpec struct {
	// ContentArchive controls the archive of content no longer referenced by any FlowFile.
	// +optional
	ContentArchive *ContentArchiveSpec `json:"contentArchive,omitempty"`

	// Provenance limits how long and how much provenance history is kept.
	// +optional
	Provenance *ProvenanceRepositorySpec `json:"provenance,omitempty"`

	// FlowFile tunes the FlowFile repository.
	// +optional
	FlowFile *FlowFileRepositorySpec `json:"flowFile,omitempty"`
}

// ContentArchiveSpec sets nifi.content.repository.archive.*.
type ContentArchiveSpec struct {
	// Enabled turns the content archive on or off (nifi.content.repository.archive.enabled).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MaxRetentionPeriod is how long archived content is kept, in NiFi's time format, e.g.
	// "12 hours" or "7 days" (nifi.content.repository.archive.max.retention.period).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)? *(ms|millis|milliseconds|s|sec|secs|second|seconds|m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days|w|wk|wks|week|weeks)$`
	MaxRetentionPeriod string `json:"maxRetentionPeriod,omitempty"`

	// MaxUsagePercentage is the share of the content repository's disk above which archived
	// content is removed early (nifi.content.repository.archive.max.usage.percentage).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxUsagePercentage *int32 `json:"maxUsagePercentage,omitempty"`
}

// ProvenanceRepositorySpec sets nifi.provenance.repository.max.storage.*.
type ProvenanceRepositorySpec struct {
	// MaxStorageTime is how long provenance events are kept, in NiFi's time format, e.g.
	// "30 days" (nifi.provenance.repository.max.storage.time).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)? *(ms|millis|milliseconds|s|sec|secs|second|seconds|m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days|w|wk|wks|week|weeks)$`
	MaxStorageTime string `json:"maxStorageTime,omitempty"`

	// MaxStorageSize caps the provenance repository's size, as a Kubernetes quantity, e.g. "10Gi"
	// (nifi.provenance.repository.max.storage.size).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).isGreaterThan(quantity('0'))",message="must be a positive Kubernetes quantity"
	MaxStorageSize string `json:"maxStorageSize,omitempty"`
}

// FlowFileRepositorySpec sets nifi.flowfile.repository.*.
type FlowFileRepositorySpec struct {
	// CheckpointInterval is how often the FlowFile repository writes a checkpoint, in NiFi's time
	// format, e.g. "20 secs" (nifi.flowfile.repository.checkpoint.interval).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)? *(ms|millis|milliseconds|s|sec|secs|second|seconds|m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days|w|wk|wks|week|weeks)$`
	CheckpointInterval string `json:"checkpointInterval,omitempty"`
}

// PVCRetentionType is what happens to a node's PVCs: Retain keeps them, Delete removes them.
// +kubebuilder:validation:Enum=Retain;Delete
type PVCRetentionType string

const (
	// PVCRetain keeps the PVCs, and with them the node's flow, repositories and logs.
	PVCRetain PVCRetentionType = "Retain"
	// PVCDelete deletes the PVCs and everything on them.
	PVCDelete PVCRetentionType = "Delete"
)

// PVCRetentionPolicy mirrors the StatefulSet's persistentVolumeClaimRetentionPolicy. Fields are
// deliberately not defaulted by the CRD, so an unset node-group field can inherit the cluster's.
type PVCRetentionPolicy struct {
	// WhenDeleted applies when the node group's StatefulSet is deleted: the NiFiCluster is
	// deleted, or the node group is removed from it. With Delete the flow can't be recovered.
	// +optional
	WhenDeleted PVCRetentionType `json:"whenDeleted,omitempty"`

	// WhenScaled applies to the PVCs of the nodes removed by a scale-down, after the operator has
	// offloaded them. With Delete a later scale-up starts those nodes empty.
	// +optional
	WhenScaled PVCRetentionType `json:"whenScaled,omitempty"`
}

// SplitRepositoryStorageSpec configures the per-repository storage split for a node group - see
// NodeGroup.SplitRepositoryStorage's own doc comment for the full behavior and migration
// caveat. Every size field is optional; a default is applied when unset, so a user can override
// just the one repository they care about (e.g. a much bigger Content repository) without
// having to specify sizes for the rest.
// The repository sizes can only grow once the node group has its volumes (mirrors the webhook).
// +kubebuilder:validation:XValidation:rule="!self.enabled || !oldSelf.enabled || (quantity(self.flowFileRepositoryStorage).compareTo(quantity(oldSelf.flowFileRepositoryStorage)) == 0 && quantity(self.contentRepositoryStorage).compareTo(quantity(oldSelf.contentRepositoryStorage)) == 0 && quantity(self.provenanceRepositoryStorage).compareTo(quantity(oldSelf.provenanceRepositoryStorage)) == 0 && quantity(self.databaseRepositoryStorage).compareTo(quantity(oldSelf.databaseRepositoryStorage)) == 0 && quantity(self.extensionsStorage).compareTo(quantity(oldSelf.extensionsStorage)) == 0)",message="split repository sizes: storage size changes are not supported in the Community edition; expand the PVCs with kubectl patch pvc",reason="FieldValueForbidden"
type SplitRepositoryStorageSpec struct {
	// Enabled toggles the per-repository storage split.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// FlowFileRepositoryStorage is the persistent volume size for the FlowFile repository
	// (e.g. "5Gi").
	// +kubebuilder:default="5Gi"
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity"
	FlowFileRepositoryStorage string `json:"flowFileRepositoryStorage,omitempty"`

	// ContentRepositoryStorage is the persistent volume size for the Content repository.
	// Defaults larger than the other repositories, since this one holds the actual flowing
	// data.
	// +kubebuilder:default="10Gi"
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity"
	ContentRepositoryStorage string `json:"contentRepositoryStorage,omitempty"`

	// ProvenanceRepositoryStorage is the persistent volume size for the Provenance repository.
	// +kubebuilder:default="5Gi"
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity"
	ProvenanceRepositoryStorage string `json:"provenanceRepositoryStorage,omitempty"`

	// DatabaseRepositoryStorage is the persistent volume size for the database repository
	// (NiFi's flow definition and internal H2 database).
	// +kubebuilder:default="2Gi"
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity"
	DatabaseRepositoryStorage string `json:"databaseRepositoryStorage,omitempty"`

	// ExtensionsStorage is the persistent volume size for custom NARs/jars and custom Python
	// extensions.
	// +kubebuilder:default="5Gi"
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="isQuantity(self)",message="must be a Kubernetes quantity"
	ExtensionsStorage string `json:"extensionsStorage,omitempty"`
}

// TLSSpec configures mutual TLS for a node group. The operator owns a self-signed CA (one per
// NiFiCluster, created once and never rotated - see reconcileTLSCA); a separate CSI driver mints
// a fresh, per-pod leaf certificate at pod-mount time, signed by that CA, as a PKCS12 keystore and
// truststore (NiFi 2 has no PEM keystore support).
// +kubebuilder:validation:XValidation:rule="!self.enabled || !has(self.certificateValidityDays) || self.certificateValidityDays <= 3650",message="certificateValidityDays must be at most 3650"
type TLSSpec struct {
	// Enabled toggles TLS/mTLS for this node group. On by default for new node groups: the
	// defaulting webhook adds tls.enabled: true to a node group created without a tls block. A tls
	// block without enabled means TLS off.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled"`

	// ExtraSANs are additional DNS names included on every pod's certificate in this node
	// group, beyond that pod's own internal FQDN - e.g. an external hostname clients use
	// through a proxy or load balancer that passes TLS through.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	ExtraSANs []string `json:"extraSANs,omitempty"`

	// CertificateValidityDays is the leaf certificate's validity period. A pod is restarted
	// (and issued a fresh certificate) once it has run for roughly half of this period,
	// mirroring the proactive-renewal pattern used by other Kubernetes CSI-based secret
	// provisioners.
	// +kubebuilder:default=365
	// +kubebuilder:validation:Minimum=1
	// +optional
	CertificateValidityDays *int32 `json:"certificateValidityDays,omitempty"`

	// KeystorePasswordSecretName names a Secret (in this NiFiCluster's own namespace, "password"
	// key) whose value is used as every pod's PKCS12 keystore/truststore password, instead of the
	// operator generating one itself. This password only protects the on-disk PKCS12 encoding,
	// not the key material inside it - the private key is exactly as sensitive with or without a
	// password on the container around it - so this field exists for organizations whose own
	// compliance/rotation tooling needs to own every password in the system, not because the
	// operator-generated default is otherwise insecure. All node groups of one cluster share one
	// CA (and so one keystore password); set this the same way on every TLS-enabled node group,
	// or leave it unset on all of them. Read once, when the cluster's CA Secret is first created,
	// and never re-read afterward - the same create-once rule as the CA itself, since changing
	// the password after pods already have keystores encrypted with the old one would lock them
	// out until every pod's certificate is reissued.
	// +optional
	KeystorePasswordSecretName string `json:"keystorePasswordSecretName,omitempty"`
}

// PodDisruptionBudgetSpec configures a PodDisruptionBudget for a node group.
type PodDisruptionBudgetSpec struct {
	// Enabled toggles generation of a PodDisruptionBudget for this node group. A *bool (rather
	// than bool) so an explicit "false" can actually be represented on the wire: with a plain
	// bool and omitempty, JSON marshaling would drop the zero value entirely, and the
	// default below would silently coerce it back to true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MinAvailable is the minimum number of pods that must remain available during a
	// voluntary disruption. Defaults to Replicas-1 when unset, and is validated by the
	// webhook to never equal or exceed Replicas.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinAvailable *int32 `json:"minAvailable,omitempty"`
}

// NiFiClusterStatus defines the observed state of NiFiCluster.
type NiFiClusterStatus struct {
	// Conditions represent the latest available observations of the cluster's state
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Replicas is the total number of desired NiFi node replicas across all node groups.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the total number of ready NiFi node replicas across all node groups.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// NodeGroupStatuses reports the observed state of each node group.
	// +optional
	NodeGroupStatuses []NodeGroupStatus `json:"nodeGroupStatuses,omitempty"`

	// NodeRemovalPending is set when the cluster shrank (fewer replicas or a removed node group)
	// and NiFi may still list the departed nodes; the operator then deletes them from the NiFi
	// cluster once the remaining nodes are ready, and clears it.
	// +optional
	NodeRemovalPending bool `json:"nodeRemovalPending,omitempty"`

	// CoordinationBackend is the coordination backend the running NiFi nodes were last
	// configured with by the operator.
	// +kubebuilder:validation:Enum=Kubernetes;ZooKeeper
	// +optional
	CoordinationBackend CoordinationBackend `json:"coordinationBackend,omitempty"`

	// AuthSecretName is the name of the Secret (in this NiFiCluster's own namespace) currently
	// backing the NiFi UI/API login - whichever one is actually in effect: the operator's
	// default-generated login when spec.authentication is unset, the operator's Secret synced
	// from literal Username/Password, or the user's own Secret named by
	// spec.authentication.singleUser.secretName. Never the credentials themselves - only a
	// pointer to where to find them (`kubectl get secret <name> -o jsonpath='{.data.password}'
	// | base64 -d`), since this field is as readable as the NiFiCluster itself, which is
	// typically broader access than who can read Secrets.
	// +optional
	AuthSecretName string `json:"authSecretName,omitempty"`

	// NiFiVersion is the NiFi version the cluster reports (GET /nifi-api/flow/about).
	// +optional
	NiFiVersion string `json:"nifiVersion,omitempty"`

	// ClusterCoordinator is the address of the node NiFi currently runs as Cluster Coordinator.
	// +optional
	ClusterCoordinator string `json:"clusterCoordinator,omitempty"`

	// PrimaryNode is the address of the node NiFi currently runs as Primary Node.
	// +optional
	PrimaryNode string `json:"primaryNode,omitempty"`

	// Nodes is NiFi's own view of its cluster members, polled from the NiFi REST API every
	// --nifi-status-interval (default 20s). Heartbeat, thread and queue figures are refreshed in
	// status at most every 5 minutes unless a node's state or roles change.
	// Keyed by nodeId, not address: a restarted node rejoins under a new ID, and NiFi can list its
	// old, disconnected identity at the same address until the operator removes it.
	// +listType=map
	// +listMapKey=nodeId
	// +optional
	Nodes []NiFiNodeStatus `json:"nodes,omitempty"`

	// LastNiFiPoll is when Nodes and the ClusterHealthy condition were last written from NiFi.
	// +optional
	LastNiFiPoll *metav1.Time `json:"lastNiFiPoll,omitempty"`
}

// NiFiNodeStatus is one NiFi cluster member as NiFi reports it.
type NiFiNodeStatus struct {
	// NodeID is NiFi's identifier for the node; a restarted node usually gets a new one.
	NodeID string `json:"nodeId"`
	// Address is the node's address as NiFi knows it (the pod's FQDN).
	Address string `json:"address"`
	// NodeGroup is the node group the node's pod belongs to; empty for a node NiFi still lists
	// but that no current pod backs (a departed node awaiting removal).
	// +optional
	NodeGroup string `json:"nodeGroup,omitempty"`
	// State is NiFi's connection state: CONNECTED, CONNECTING, DISCONNECTED, DISCONNECTING,
	// OFFLOADING or OFFLOADED.
	State string `json:"state"`
	// Roles held by the node: "Cluster Coordinator", "Primary Node".
	// +optional
	Roles []string `json:"roles,omitempty"`
	// LastHeartbeat is NiFi's time of the node's last heartbeat, as NiFi formats it.
	// +optional
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	// ActiveThreads is the node's active thread count.
	// +optional
	ActiveThreads int32 `json:"activeThreads,omitempty"`
	// Queued is the node's queued FlowFiles, as NiFi formats it (e.g. "120 / 4.5 MB").
	// +optional
	Queued string `json:"queued,omitempty"`
}

// NodeGroupStatus reports the observed state of a single node group.
type NodeGroupStatus struct {
	// Name is the node group this status applies to.
	Name string `json:"name"`

	// Replicas is the desired number of replicas for this node group.
	Replicas int32 `json:"replicas"`

	// ReadyReplicas is the number of ready replicas for this node group.
	ReadyReplicas int32 `json:"readyReplicas"`

	// UpdatedReplicas is the number of replicas for this node group running the StatefulSet's
	// current pod template revision - distinct from ReadyReplicas, which a pod still on the
	// *previous* revision also counts toward. Needed to tell a genuinely finished rollout (every
	// pod on the new template, all ready) apart from the merely coincidental moment, part-way
	// through a rolling update, where the old and new pods' ready counts happen to add up to the
	// desired total (the UpgradeCompleted event and the Upgrading condition both require
	// this, not just ReadyReplicas).
	// Optional so that an operator one release older, which does not set it yet, can still
	// write status while ArgoCD has already applied this CRD.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// StatefulSetObserved is false for the one reconcile right after this node group's
	// StatefulSet spec changes, until the StatefulSet controller itself has caught up with it -
	// during that brief window ReadyReplicas/UpdatedReplicas above still reflect the *previous*
	// template, not the one just applied, so the UpgradeCompleted event and the Upgrading
	// condition both require this to be true before trusting those counts mean a rollout actually
	// finished. Caught live: without this, a pod still on the old image being Ready could make
	// ReadyReplicas alone equal the desired count before the rollout had even started.
	// Optional for the same reason; absent reads as false (not yet observed).
	// +optional
	StatefulSetObserved bool `json:"statefulSetObserved,omitempty"`

	// Conditions represent the latest available observations of this node group's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas"
// +kubebuilder:printcolumn:name="Coordination",type="string",JSONPath=".status.coordinationBackend",priority=1
// +kubebuilder:printcolumn:name="Auth Secret",type="string",JSONPath=".status.authSecretName",priority=1
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.nifiVersion",priority=1
// +kubebuilder:printcolumn:name="Healthy",type="string",JSONPath=".status.conditions[?(@.type==\"ClusterHealthy\")].status",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NiFiCluster runs one Apache NiFi cluster, made of its inline node groups plus any
// NiFiNodeGroup objects that reference it.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.authentication) || !has(self.spec.authentication.singleUser) || !has(self.spec.authentication.singleUser.secretName) || self.spec.authentication.singleUser.secretName != self.metadata.name + '-auth-credentials'",message="spec.authentication.singleUser.secretName is the operator-managed login Secret for this cluster; name a Secret you manage yourself"
type NiFiCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NiFiClusterSpec   `json:"spec,omitempty"`
	Status NiFiClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NiFiClusterList contains a list of NiFiCluster.
type NiFiClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NiFiCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NiFiCluster{}, &NiFiClusterList{})
}

# API reference

Every field of the operator's custom resources, generated from the Go API types with
[crd-ref-docs](https://github.com/elastic/crd-ref-docs) (`make api-docs`). For an overview of
each resource (printer columns, subresources, examples) start with the
[CRD reference](crd-reference.md).

In the field tables, **Required** marks fields the API server rejects a resource without;
**Default** is the value set when the field is omitted; **Validation** lists the OpenAPI rules
(enums, patterns, limits). Rules that span several fields (CEL) are listed under the type.

## Packages
- [nifi.ksolves.com/v1alpha1](#nifiksolvescomv1alpha1)


## nifi.ksolves.com/v1alpha1

Package v1alpha1 contains API Schema definitions for the nifi v1alpha1 API group.

### Resource types
- [NiFiCluster](#nificluster)
- [NiFiNodeGroup](#nifinodegroup)

#### NiFiCluster



NiFiCluster runs one Apache NiFi cluster, made of its inline node groups plus any
NiFiNodeGroup objects that reference it.





| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| `apiVersion` | _string_ | ✓ | | `nifi.ksolves.com/v1alpha1` | |
| `kind` | _string_ | ✓ | | `NiFiCluster` | |
| <a id="nificluster-metadata"></a>`metadata` | _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ |  |  | Refer to Kubernetes API documentation for fields of `metadata`. |  |
| <a id="nificluster-spec"></a>`spec` | _[NiFiClusterSpec](#nificlusterspec)_ |  |  |  |  |
| <a id="nificluster-status"></a>`status` | _[NiFiClusterStatus](#nificlusterstatus)_ |  |  |  |  |

#### NiFiNodeGroup



NiFiNodeGroup is a node group of a NiFiCluster as an object of its own.





| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| `apiVersion` | _string_ | ✓ | | `nifi.ksolves.com/v1alpha1` | |
| `kind` | _string_ | ✓ | | `NiFiNodeGroup` | |
| <a id="nifinodegroup-metadata"></a>`metadata` | _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ |  |  | Refer to Kubernetes API documentation for fields of `metadata`. |  |
| <a id="nifinodegroup-spec"></a>`spec` | _[NiFiNodeGroupSpec](#nifinodegroupspec)_ |  |  |  |  |
| <a id="nifinodegroup-status"></a>`status` | _[NiFiNodeGroupStatus](#nifinodegroupstatus)_ |  |  |  |  |


### Other types

#### AuthenticationSpec



AuthenticationSpec configures how users authenticate to the NiFi UI/API. When left unset
entirely, the operator still generates a Secret-backed login by default (username "admin",
a random password), shared by every node in the cluster - rather than leaving each node to
fall back to NiFi's own per-node random single-user credentials, which differ from node to
node and make a multi-node cluster's UI effectively impossible to log into consistently.
Retrieve the generated password with:
`kubectl get secret <cluster>-auth-credentials -o jsonpath='{.data.password}' | base64 -d`.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="authenticationspec-singleuser"></a>`singleUser` | _[SingleUserAuthSpec](#singleuserauthspec)_ |  |  | SingleUser configures NiFi's built-in single-user login provider with a specific,<br />known-ahead-of-time username/password, instead of the operator's own generated default<br />credentials, given literally or through a Secret (which can be externally managed, e.g.<br />synced from Vault or AWS Secrets Manager). |  |

#### ClusterServiceSpec



ClusterServiceSpec configures the cluster Service.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="clusterservicespec-annotations"></a>`annotations` | _object (keys:string, values:string)_ |  |  | Annotations set on the cluster Service only (e.g. a cloud provider's load balancer<br />settings), in addition to spec.annotations. |  |
| <a id="clusterservicespec-loadbalancerclass"></a>`loadBalancerClass` | _string_ |  |  | LoadBalancerClass selects the load balancer implementation when serviceType is<br />LoadBalancer. Immutable on the Service once set, as Kubernetes defines it. |  |
| <a id="clusterservicespec-loadbalancersourceranges"></a>`loadBalancerSourceRanges` | _string array_ |  |  | LoadBalancerSourceRanges restricts which client CIDRs may reach a LoadBalancer Service.<br />Strongly recommended for anything internet-facing. | MaxItems: 64 <br /> |
| <a id="clusterservicespec-externaltrafficpolicy"></a>`externalTrafficPolicy` | _[ServiceExternalTrafficPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#serviceexternaltrafficpolicy-v1-core)_ |  |  | ExternalTrafficPolicy for a LoadBalancer Service. Local keeps the client's<br />source IP, which the Service's ClientIP session affinity (NiFi's UI needs one node per<br />session) relies on; Cluster (the Kubernetes default) hides it behind the node. | Enum: [Cluster Local] <br /> |

#### ConfigOverridesSpec



ConfigOverridesSpec sets raw configuration key/value pairs the operator merges into its own
generated configuration: key-level merges into nifi.properties and bootstrap.conf, and
full-file replacement of logback.xml and authorizers.xml.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="configoverridesspec-nifiproperties"></a>`nifiProperties` | _object (keys:string, values:string)_ |  |  | NifiProperties are additional "key: value" pairs merged into nifi.properties. A key<br />already set by the operator itself is rejected by the validating webhook rather than<br />silently ignored or silently overwritten - see the webhook's reserved-key check. | MaxProperties: 256 <br /> |
| <a id="configoverridesspec-bootstrapconf"></a>`bootstrapConf` | _object (keys:string, values:string)_ |  |  | BootstrapConf are additional "key: value" pairs merged into bootstrap.conf - most commonly<br />JVM heap settings (e.g. "java.arg.3": "-Xmx4g"). The operator's entrypoint applies these the<br />same way it applies NifiProperties. | MaxProperties: 256 <br /> |
| <a id="configoverridesspec-logback"></a>`logback` | _string_ |  |  | Logback, when set, fully replaces NiFi's logback.xml with this raw XML content. Unlike<br />NifiProperties/BootstrapConf above, logback.xml is structured XML, not a flat key=value<br />file, so a key-level merge doesn't apply here - the whole file is replaced instead. The<br />operator does not validate the XML's correctness beyond rejecting an empty value; a<br />malformed file will fail at NiFi startup. | MinLength: 1 <br /> |
| <a id="configoverridesspec-authorizers"></a>`authorizers` | _string_ |  |  | Authorizers, when set, fully replaces NiFi's authorizers.xml with this raw XML content -<br />same full-file-replacement behavior and validation caveat as Logback above. | MinLength: 1 <br /> |
| <a id="configoverridesspec-secretname"></a>`secretName` | _string_ |  |  | SecretName optionally names a Secret (in the NiFiCluster's own namespace) whose data is<br />applied as additional nifi.properties overrides, for values too sensitive to store as<br />plaintext in the CR (NifiProperties is visible via `kubectl get nificluster -o yaml`).<br />Precedence, lowest first: cluster NifiProperties < cluster Secret < node-group<br />NifiProperties < node-group Secret. Secret-sourced values are delivered through an<br />operator-owned Secret (<cluster>-<nodeGroup>-config-secret), never the ConfigMap, and<br />rotating the Secret rolls the pods. Setting or changing this field requires permission to<br />get that Secret. Reserved keys are skipped even when sourced from a Secret. |  |

#### ContentArchiveSpec



ContentArchiveSpec sets nifi.content.repository.archive.*.



_Appears in:_
- [RepositoriesSpec](#repositoriesspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="contentarchivespec-enabled"></a>`enabled` | _boolean_ |  |  | Enabled turns the content archive on or off (nifi.content.repository.archive.enabled). |  |
| <a id="contentarchivespec-maxretentionperiod"></a>`maxRetentionPeriod` | _string_ |  |  | MaxRetentionPeriod is how long archived content is kept, in NiFi's time format, e.g.<br />"12 hours" or "7 days" (nifi.content.repository.archive.max.retention.period). | MaxLength: 64 <br />Pattern: `^[0-9]+(\.[0-9]+)? *(ms\|millis\|milliseconds\|s\|sec\|secs\|second\|seconds\|m\|min\|mins\|minute\|minutes\|h\|hr\|hrs\|hour\|hours\|d\|day\|days\|w\|wk\|wks\|week\|weeks)$` <br /> |
| <a id="contentarchivespec-maxusagepercentage"></a>`maxUsagePercentage` | _integer_ |  |  | MaxUsagePercentage is the share of the content repository's disk above which archived<br />content is removed early (nifi.content.repository.archive.max.usage.percentage). | Maximum: 100 <br />Minimum: 1 <br /> |

#### CoordinationBackend

_Underlying type:_ _string_

CoordinationBackend names a NiFi cluster coordination backend.



_Appears in:_
- [CoordinationSpec](#coordinationspec)
- [NiFiClusterStatus](#nificlusterstatus)

| Value | Description |
| --- | --- |
| `Kubernetes` | CoordinationBackendKubernetes uses NiFi 2.x's native Kubernetes support: Lease objects for<br />leader election (KubernetesLeaderElectionManager) and ConfigMaps for cluster state<br />(KubernetesConfigMapStateProvider). No ZooKeeper is deployed or required.<br /> |
| `ZooKeeper` | CoordinationBackendZooKeeper uses an existing, externally managed ZooKeeper ensemble for<br />leader election (CuratorLeaderElectionManager) and cluster state (ZooKeeperStateProvider).<br />The operator never deploys ZooKeeper itself. Not accepted in spec by the Community edition;<br />clusters created with it by earlier versions keep running on it.<br /> |

#### CoordinationSpec



CoordinationSpec configures cluster coordination.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="coordinationspec-backend"></a>`backend` | _[CoordinationBackend](#coordinationbackend)_ |  | Kubernetes | Backend is the coordination backend; defaults to Kubernetes when coordination is present (the<br />coordination field itself has no CRD default, see above). Immutable once set (see the rule<br />on NiFiClusterSpec):<br />switching the leader-election and state backend of a running NiFi cluster risks split-brain<br />and loses component state. Whenever the backend the running pods were built for differs from<br />this one, the operator only switches through an explicitly approved full-stop migration.<br />The Community edition accepts Kubernetes only: external ZooKeeper is not available. | Enum: [Kubernetes] <br /> |
| <a id="coordinationspec-zookeeper"></a>`zookeeper` | _[ZooKeeperSpec](#zookeeperspec)_ |  |  | ZooKeeper configures the external ensemble of a cluster on the ZooKeeper backend, which only<br />clusters created by earlier versions can have. |  |
| <a id="coordinationspec-bindpodserviceaccounts"></a>`bindPodServiceAccounts` | _boolean_ |  |  | BindPodServiceAccounts asks the operator to also bind the Kubernetes coordination Role<br />(Leases and ConfigMaps in this namespace) to every node group's<br />podOverrides.serviceAccountName, not only to the cluster's own ServiceAccount.<br />It takes effect only when the operator admin has enabled it too<br />(--allow-custom-service-account-binding, chart value<br />coordination.allowCustomServiceAccountBinding), and you may only set it if you yourself may<br />update Leases and ConfigMaps in this namespace. Without it, bind the Role yourself; the<br />CoordinationRBACReady condition reports any ServiceAccount still missing it. |  |

#### FlowFileRepositorySpec



FlowFileRepositorySpec sets nifi.flowfile.repository.*.



_Appears in:_
- [RepositoriesSpec](#repositoriesspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="flowfilerepositoryspec-checkpointinterval"></a>`checkpointInterval` | _string_ |  |  | CheckpointInterval is how often the FlowFile repository writes a checkpoint, in NiFi's time<br />format, e.g. "20 secs" (nifi.flowfile.repository.checkpoint.interval). | MaxLength: 64 <br />Pattern: `^[0-9]+(\.[0-9]+)? *(ms\|millis\|milliseconds\|s\|sec\|secs\|second\|seconds\|m\|min\|mins\|minute\|minutes\|h\|hr\|hrs\|hour\|hours\|d\|day\|days\|w\|wk\|wks\|week\|weeks)$` <br /> |

#### ImageSpec



ImageSpec defines the container image to run for NiFi nodes.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="imagespec-repository"></a>`repository` | _string_ |  |  | Repository is the image repository (without tag) |  |
| <a id="imagespec-tag"></a>`tag` | _string_ |  |  | Tag is the image tag to use |  |

#### NiFiClusterSpec



NiFiClusterSpec defines the desired state of NiFiCluster.



_Appears in:_
- [NiFiCluster](#nificluster)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nificlusterspec-image"></a>`image` | _[ImageSpec](#imagespec)_ |  |  | Image is the container image to use for NiFi nodes: the stock apache/nifi image or one built<br />on it. Leave it unset to run the operator's default (its --default-nifi-image, by default<br />apache/nifi:2.12.0); set both repository and tag, or neither. NiFi 2.11.0 or later is required with<br />Kubernetes coordination, 1.14.0 or later with ZooKeeper (docs/compatibility.md). |  |
| <a id="nificlusterspec-nodegroups"></a>`nodeGroups` | _[NodeGroup](#nodegroup) array_ |  |  | NodeGroups defines the cluster's node group inline. The Community edition allows one node<br />group (and no new NiFiNodeGroup objects), of at most 3 NiFi nodes. NiFiNodeGroups created by<br />earlier versions still count towards the 3-node total. | MaxItems: 1 <br /> |
| <a id="nificlusterspec-servicetype"></a>`serviceType` | _[ServiceType](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#servicetype-v1-core)_ |  | ClusterIP | ServiceType is the Kubernetes Service type used for client access to the cluster.<br />ClusterIP (the default) keeps the cluster in-cluster only; LoadBalancer provisions an<br />external load balancer (tune it via service) and needs TLS on the node groups: the webhook<br />rejects it otherwise. For HTTP<br />exposure, point your own Ingress, Gateway API HTTPRoute or OpenShift Route at the cluster<br />Service (named after the cluster). | Enum: [ClusterIP LoadBalancer] <br /> |
| <a id="nificlusterspec-service"></a>`service` | _[ClusterServiceSpec](#clusterservicespec)_ |  |  | Service tunes the cluster Service (the one spanning every node group, which serviceType<br />applies to): its annotations and load balancer settings. |  |
| <a id="nificlusterspec-proxyhosts"></a>`proxyHosts` | _string array_ |  |  | ProxyHosts is the explicit list of hostnames (and optionally ports) clients will use to<br />reach this cluster - via a LoadBalancer, an Ingress, a Gateway API route, or anything else<br />in front of the ClusterIP Service. NiFi validates the incoming Host header strictly<br />(nifi.web.proxy.host) and rejects anything not on this list, so it must be kept in sync<br />with whatever's actually routing traffic here. Not required when only accessing the<br />cluster via its in-cluster ClusterIP Service/DNS name, which is always implicitly<br />allowed. |  |
| <a id="nificlusterspec-annotations"></a>`annotations` | _object (keys:string, values:string)_ |  |  | Annotations are propagated to every object generated for this cluster, in addition to<br />any NodeGroup-level annotations. |  |
| <a id="nificlusterspec-labels"></a>`labels` | _object (keys:string, values:string)_ |  |  | Labels are propagated to every object generated for this cluster, in addition to any<br />NodeGroup-level labels. |  |
| <a id="nificlusterspec-serviceaccountname"></a>`serviceAccountName` | _string_ |  |  | ServiceAccountName is the ServiceAccount every NiFi node pod runs as. When unset, the<br />operator creates a dedicated one (<name>-nifi) and, on OpenShift, binds it to the configured<br />SecurityContextConstraints. A ServiceAccount named here gets the Kubernetes coordination<br />Role (Leases and ConfigMaps in this namespace) bound to it, but never an SCC: grant it one<br />yourself, since the operator must not hand out SCC access to arbitrary ServiceAccounts. |  |
| <a id="nificlusterspec-podsecuritycontext"></a>`podSecurityContext` | _[PodSecurityContext](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#podsecuritycontext-v1-core)_ |  |  | PodSecurityContext is merged, field by field, over the NiFi pods' own security context<br />(seccompProfile RuntimeDefault, fsGroup 0 with fsGroupChangePolicy OnRootMismatch; on<br />OpenShift without a configured SCC no fsGroup, so restricted-v2 assigns one). A node<br />group's podOverrides.podSecurityContext is merged over this in turn. Setting it requires<br />permission to create pods in the namespace. |  |
| <a id="nificlusterspec-containersecuritycontext"></a>`containerSecurityContext` | _[SecurityContext](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#securitycontext-v1-core)_ |  |  | ContainerSecurityContext is merged, field by field, over the "nifi" container's own<br />security context (runAsNonRoot, no privilege escalation, all capabilities dropped,<br />readOnlyRootFilesystem, seccompProfile RuntimeDefault); a node group's<br />podOverrides.containerSecurityContext is merged over this in turn. Setting it requires<br />permission to create pods in the namespace. |  |
| <a id="nificlusterspec-imagepullsecrets"></a>`imagePullSecrets` | _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#localobjectreference-v1-core) array_ |  |  | ImagePullSecrets are referenced by every NiFi node pod to pull Image.Repository from a<br />private registry. Without this, a private-registry image requires manually patching the<br />namespace's default ServiceAccount after the fact. |  |
| <a id="nificlusterspec-authentication"></a>`authentication` | _[AuthenticationSpec](#authenticationspec)_ |  |  | Authentication configures how users log into the NiFi UI/API. Cluster-scoped (not<br />per-node-group like TLS): NiFi's login identity is one logical thing shared by every node<br />in the cluster, since they all serve the same flow. If unset, the operator generates one<br />shared login (user "admin", random password) in the Secret <name>-auth-credentials.<br />singleUser sets the login yourself (literal username/password or secretName; the password<br />is stored as a bcrypt hash and must be at least 12 characters and at most 72 bytes<br />(bcrypt's limit)). |  |
| <a id="nificlusterspec-configoverrides"></a>`configOverrides` | _[ConfigOverridesSpec](#configoverridesspec)_ |  |  | ConfigOverrides sets additional NiFi configuration properties, merged with (and<br />overridden by) any NodeGroup-level ConfigOverrides. Lets a user tune NiFi behavior the<br />operator doesn't otherwise expose (e.g. UI session timeouts, provenance query limits)<br />without building a custom image. Keys the operator manages itself (ZooKeeper<br />coordination, cluster protocol, repository paths, TLS keystore/truststore, proxy host)<br />are rejected by the validating webhook. |  |
| <a id="nificlusterspec-coordination"></a>`coordination` | _[CoordinationSpec](#coordinationspec)_ |  |  | Coordination selects how the NiFi nodes of this cluster elect their Cluster Coordinator and<br />Primary Node and where they keep cluster-wide component state. Every node group in the<br />NiFiCluster joins one single NiFi cluster through this backend. Unset means Kubernetes.<br />Deliberately not defaulted by the CRD: a CRD default is also applied to stored objects on<br />read, which would make the immutability rule above lock clusters that never chose a backend. |  |
| <a id="nificlusterspec-persistentvolumeclaimretentionpolicy"></a>`persistentVolumeClaimRetentionPolicy` | _[PVCRetentionPolicy](#pvcretentionpolicy)_ |  |  | PersistentVolumeClaimRetentionPolicy decides what happens to the PVCs of every node<br />group's repositories and logs when a node group's StatefulSet is deleted (the NiFiCluster<br />is deleted, or the node group is removed) or scaled down. A node group's own<br />PersistentVolumeClaimRetentionPolicy overrides it field by field. Unset fields mean Retain,<br />the behavior of every earlier operator version. |  |
| <a id="nificlusterspec-repositories"></a>`repositories` | _[RepositoriesSpec](#repositoriesspec)_ |  |  | Repositories tunes how long and how much the content archive, the provenance repository and<br />the FlowFile repository keep. A node group's own Repositories overrides it field by<br />field; an unset field keeps NiFi's default. A key set here can't also be set in<br />configOverrides. Changing it restarts the nodes, like any nifi.properties change. |  |

#### NiFiClusterStatus



NiFiClusterStatus defines the observed state of NiFiCluster.



_Appears in:_
- [NiFiCluster](#nificluster)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nificlusterstatus-conditions"></a>`conditions` | _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Conditions represent the latest available observations of the cluster's state |  |
| <a id="nificlusterstatus-replicas"></a>`replicas` | _integer_ |  |  | Replicas is the total number of desired NiFi node replicas across all node groups. |  |
| <a id="nificlusterstatus-readyreplicas"></a>`readyReplicas` | _integer_ |  |  | ReadyReplicas is the total number of ready NiFi node replicas across all node groups. |  |
| <a id="nificlusterstatus-nodegroupstatuses"></a>`nodeGroupStatuses` | _[NodeGroupStatus](#nodegroupstatus) array_ |  |  | NodeGroupStatuses reports the observed state of each node group. |  |
| <a id="nificlusterstatus-noderemovalpending"></a>`nodeRemovalPending` | _boolean_ |  |  | NodeRemovalPending is set when the cluster shrank (fewer replicas or a removed node group)<br />and NiFi may still list the departed nodes; the operator then deletes them from the NiFi<br />cluster once the remaining nodes are ready, and clears it. |  |
| <a id="nificlusterstatus-coordinationbackend"></a>`coordinationBackend` | _[CoordinationBackend](#coordinationbackend)_ |  |  | CoordinationBackend is the coordination backend the running NiFi nodes were last<br />configured with by the operator. | Enum: [Kubernetes ZooKeeper] <br /> |
| <a id="nificlusterstatus-authsecretname"></a>`authSecretName` | _string_ |  |  | AuthSecretName is the name of the Secret (in this NiFiCluster's own namespace) currently<br />backing the NiFi UI/API login - whichever one is actually in effect: the operator's<br />default-generated login when spec.authentication is unset, the operator's Secret synced<br />from literal Username/Password, or the user's own Secret named by<br />spec.authentication.singleUser.secretName. Never the credentials themselves - only a<br />pointer to where to find them (`kubectl get secret <name> -o jsonpath='\{.data.password\}'<br />\| base64 -d`), since this field is as readable as the NiFiCluster itself, which is<br />typically broader access than who can read Secrets. |  |
| <a id="nificlusterstatus-nifiversion"></a>`nifiVersion` | _string_ |  |  | NiFiVersion is the NiFi version the cluster reports (GET /nifi-api/flow/about). |  |
| <a id="nificlusterstatus-clustercoordinator"></a>`clusterCoordinator` | _string_ |  |  | ClusterCoordinator is the address of the node NiFi currently runs as Cluster Coordinator. |  |
| <a id="nificlusterstatus-primarynode"></a>`primaryNode` | _string_ |  |  | PrimaryNode is the address of the node NiFi currently runs as Primary Node. |  |
| <a id="nificlusterstatus-nodes"></a>`nodes` | _[NiFiNodeStatus](#nifinodestatus) array_ |  |  | Nodes is NiFi's own view of its cluster members, polled from the NiFi REST API every<br />--nifi-status-interval (default 20s). Heartbeat, thread and queue figures are refreshed in<br />status at most every 5 minutes unless a node's state or roles change.<br />Keyed by nodeId, not address: a restarted node rejoins under a new ID, and NiFi can list its<br />old, disconnected identity at the same address until the operator removes it. |  |
| <a id="nificlusterstatus-lastnifipoll"></a>`lastNiFiPoll` | _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ |  |  | LastNiFiPoll is when Nodes and the ClusterHealthy condition were last written from NiFi. |  |

#### NiFiNodeGroupSpec



NiFiNodeGroupSpec is one node group of a NiFiCluster, defined as an object of its own so it can
be scaled on its own: kubectl scale, HorizontalPodAutoscaler and KEDA all work
through its /scale subresource. Every field of an inline nodeGroups entry applies, with the same
validation rules; its name is the object's own name.



_Appears in:_
- [NiFiNodeGroup](#nifinodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nifinodegroupspec-clusterref"></a>`clusterRef` | _string_ | ✓ |  | ClusterRef is the name of the NiFiCluster, in this namespace, this node group belongs to.<br />Immutable. | MinLength: 1 <br /> |
| <a id="nifinodegroupspec-name"></a>`name` | _string_ | ✓ |  | Name is the identifier for this node group. Combined with the NiFiCluster's own name and<br />suffixes (e.g. "-headless") to build every generated object's name, which for Services<br />and StatefulSet-derived pod hostnames must be a valid DNS label of at most 63 characters<br />(RFC 1035) - so this is kept well short of that ceiling to leave room for the cluster<br />name and suffixes on either side. | MaxLength: 30 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| <a id="nifinodegroupspec-replicas"></a>`replicas` | _integer_ |  | 3 | Replicas is the number of nodes in this group. A cluster has at most 3 nodes in total,<br />across all of its node groups. | Minimum: 1 <br /> |
| <a id="nifinodegroupspec-storage"></a>`storage` | _string_ | ✓ |  | Storage is the persistent volume size for the NiFi repositories on each node (e.g. "10Gi").<br />Can only grow (online expansion needs an expandable StorageClass). | MaxLength: 64 <br /> |
| <a id="nifinodegroupspec-logstorage"></a>`logStorage` | _string_ |  | 5Gi | LogStorage is the persistent volume size for NiFi's logs on each node (e.g. "5Gi").<br />Logs are always stored on a dedicated PVC, never on emptyDir. Can only grow (online<br />expansion needs an expandable StorageClass). | MaxLength: 64 <br /> |
| <a id="nifinodegroupspec-storageclassname"></a>`storageClassName` | _string_ |  |  | StorageClassName is the StorageClass used for this node group's PVCs. NFS- and<br />CIFS-backed StorageClasses are rejected by the validating webhook. Immutable. |  |
| <a id="nifinodegroupspec-splitrepositorystorage"></a>`splitRepositoryStorage` | _[SplitRepositoryStorageSpec](#splitrepositorystoragespec)_ |  |  | SplitRepositoryStorage, when enabled, provisions separate PVCs for NiFi's flowfile,<br />content, provenance, and database repositories (instead of nesting them all under the<br />single Storage PVC), plus a dedicated, persistent "extensions" PVC for custom NARs and<br />custom Python extensions (NiFi auto-loads these from<br />nifi.nar.library.autoload.directory / nifi.python.extensions.source.directory.default,<br />which otherwise resolve to the image's own ephemeral filesystem - anything dropped there<br />today is silently lost on a pod restart). Off by default: an existing cluster's generated<br />PVCs and mount paths stay byte-for-byte unchanged unless this is explicitly enabled. Only<br />takes effect at node-group creation - the validating webhook rejects changing whether<br />this is enabled on an existing node group, since the operator has no mechanism to migrate<br />already-written data (flow definition, content, provenance history) from the combined<br />Storage PVC to the new per-repository layout. |  |
| <a id="nifinodegroupspec-persistentvolumeclaimretentionpolicy"></a>`persistentVolumeClaimRetentionPolicy` | _[PVCRetentionPolicy](#pvcretentionpolicy)_ |  |  | PersistentVolumeClaimRetentionPolicy overrides the cluster-level<br />PersistentVolumeClaimRetentionPolicy for this node group, field by field: an unset field<br />falls back to the cluster's, then to Retain. Can be changed at any time; the StatefulSet is<br />updated in place without restarting pods. |  |
| <a id="nifinodegroupspec-repositories"></a>`repositories` | _[RepositoriesSpec](#repositoriesspec)_ |  |  | Repositories overrides the cluster-level Repositories for this node group, field by field. |  |
| <a id="nifinodegroupspec-resources"></a>`resources` | _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ |  |  | Resources defines CPU/memory requests and limits for each node |  |
| <a id="nifinodegroupspec-poddisruptionbudget"></a>`podDisruptionBudget` | _[PodDisruptionBudgetSpec](#poddisruptionbudgetspec)_ |  |  | PodDisruptionBudget configures voluntary-disruption protection for this node group. |  |
| <a id="nifinodegroupspec-terminationgraceperiodseconds"></a>`terminationGracePeriodSeconds` | _integer_ |  | 120 | TerminationGracePeriodSeconds is the grace period given to a NiFi node to offload<br />data and disconnect from the cluster before it is force-killed. |  |
| <a id="nifinodegroupspec-annotations"></a>`annotations` | _object (keys:string, values:string)_ |  |  | Annotations are propagated to every object generated for this node group, merged<br />with (and taking precedence over) the cluster-level Annotations. |  |
| <a id="nifinodegroupspec-labels"></a>`labels` | _object (keys:string, values:string)_ |  |  | Labels are propagated to every object generated for this node group, merged with<br />(and taking precedence over) the cluster-level Labels. |  |
| <a id="nifinodegroupspec-tls"></a>`tls` | _[TLSSpec](#tlsspec)_ |  |  | TLS configures mutual TLS for this node group's REST API and cluster protocol traffic.<br />Off by default, preserving the unsecured HTTP behavior of existing clusters that don't<br />opt in. |  |
| <a id="nifinodegroupspec-configoverrides"></a>`configOverrides` | _[ConfigOverridesSpec](#configoverridesspec)_ |  |  | ConfigOverrides sets additional NiFi configuration properties for this node group,<br />merged with (and taking precedence over) the cluster-level ConfigOverrides. |  |
| <a id="nifinodegroupspec-podoverrides"></a>`podOverrides` | _[PodOverridesSpec](#podoverridesspec)_ |  |  | PodOverrides customizes this node group's generated Pod template beyond what the fields<br />above already expose (Resources, TLS, etc.), without needing a custom image. Each field is<br />additive to, or fully replaces, one specific part of the pod spec - see PodOverridesSpec's<br />own field docs. A value colliding with something the operator manages itself (the "nifi"<br />container name, its own env vars, or its own volume names) is rejected by the validating<br />webhook. |  |

#### NiFiNodeGroupStatus



NiFiNodeGroupStatus is the observed state of a NiFiNodeGroup, written by the NiFiCluster
controller.



_Appears in:_
- [NiFiNodeGroup](#nifinodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nifinodegroupstatus-replicas"></a>`replicas` | _integer_ |  |  | Replicas is the number of NiFi pods this group's StatefulSet currently has (observed, not<br />desired; 0 while the group is not reconciled). The /scale subresource reports it as the<br />current replica count. |  |
| <a id="nifinodegroupstatus-readyreplicas"></a>`readyReplicas` | _integer_ |  |  | ReadyReplicas is the number of this group's NiFi pods that are Ready. |  |
| <a id="nifinodegroupstatus-selector"></a>`selector` | _string_ |  |  | Selector is the label selector of this group's pods, in string form, for the /scale<br />subresource: a HorizontalPodAutoscaler reads it to find the pods whose metrics it averages. |  |
| <a id="nifinodegroupstatus-conditions"></a>`conditions` | _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Conditions: Ready (every node of the group Ready) and Conflict (the group cannot be<br />reconciled: its cluster does not exist, or the cluster already has an inline node group of<br />the same name). |  |

#### NiFiNodeStatus



NiFiNodeStatus is one NiFi cluster member as NiFi reports it.



_Appears in:_
- [NiFiClusterStatus](#nificlusterstatus)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nifinodestatus-nodeid"></a>`nodeId` | _string_ |  |  | NodeID is NiFi's identifier for the node; a restarted node usually gets a new one. |  |
| <a id="nifinodestatus-address"></a>`address` | _string_ |  |  | Address is the node's address as NiFi knows it (the pod's FQDN). |  |
| <a id="nifinodestatus-nodegroup"></a>`nodeGroup` | _string_ |  |  | NodeGroup is the node group the node's pod belongs to; empty for a node NiFi still lists<br />but that no current pod backs (a departed node awaiting removal). |  |
| <a id="nifinodestatus-state"></a>`state` | _string_ |  |  | State is NiFi's connection state: CONNECTED, CONNECTING, DISCONNECTED, DISCONNECTING,<br />OFFLOADING or OFFLOADED. |  |
| <a id="nifinodestatus-roles"></a>`roles` | _string array_ |  |  | Roles held by the node: "Cluster Coordinator", "Primary Node". |  |
| <a id="nifinodestatus-lastheartbeat"></a>`lastHeartbeat` | _string_ |  |  | LastHeartbeat is NiFi's time of the node's last heartbeat, as NiFi formats it. |  |
| <a id="nifinodestatus-activethreads"></a>`activeThreads` | _integer_ |  |  | ActiveThreads is the node's active thread count. |  |
| <a id="nifinodestatus-queued"></a>`queued` | _string_ |  |  | Queued is the node's queued FlowFiles, as NiFi formats it (e.g. "120 / 4.5 MB"). |  |

#### NodeGroup



NodeGroup defines a group of NiFi nodes sharing the same configuration.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)
- [NiFiNodeGroupSpec](#nifinodegroupspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nodegroup-name"></a>`name` | _string_ | ✓ |  | Name is the identifier for this node group. Combined with the NiFiCluster's own name and<br />suffixes (e.g. "-headless") to build every generated object's name, which for Services<br />and StatefulSet-derived pod hostnames must be a valid DNS label of at most 63 characters<br />(RFC 1035) - so this is kept well short of that ceiling to leave room for the cluster<br />name and suffixes on either side. | MaxLength: 30 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| <a id="nodegroup-replicas"></a>`replicas` | _integer_ |  | 3 | Replicas is the number of nodes in this group. A cluster has at most 3 nodes in total,<br />across all of its node groups. | Minimum: 1 <br /> |
| <a id="nodegroup-storage"></a>`storage` | _string_ | ✓ |  | Storage is the persistent volume size for the NiFi repositories on each node (e.g. "10Gi").<br />Can only grow (online expansion needs an expandable StorageClass). | MaxLength: 64 <br /> |
| <a id="nodegroup-logstorage"></a>`logStorage` | _string_ |  | 5Gi | LogStorage is the persistent volume size for NiFi's logs on each node (e.g. "5Gi").<br />Logs are always stored on a dedicated PVC, never on emptyDir. Can only grow (online<br />expansion needs an expandable StorageClass). | MaxLength: 64 <br /> |
| <a id="nodegroup-storageclassname"></a>`storageClassName` | _string_ |  |  | StorageClassName is the StorageClass used for this node group's PVCs. NFS- and<br />CIFS-backed StorageClasses are rejected by the validating webhook. Immutable. |  |
| <a id="nodegroup-splitrepositorystorage"></a>`splitRepositoryStorage` | _[SplitRepositoryStorageSpec](#splitrepositorystoragespec)_ |  |  | SplitRepositoryStorage, when enabled, provisions separate PVCs for NiFi's flowfile,<br />content, provenance, and database repositories (instead of nesting them all under the<br />single Storage PVC), plus a dedicated, persistent "extensions" PVC for custom NARs and<br />custom Python extensions (NiFi auto-loads these from<br />nifi.nar.library.autoload.directory / nifi.python.extensions.source.directory.default,<br />which otherwise resolve to the image's own ephemeral filesystem - anything dropped there<br />today is silently lost on a pod restart). Off by default: an existing cluster's generated<br />PVCs and mount paths stay byte-for-byte unchanged unless this is explicitly enabled. Only<br />takes effect at node-group creation - the validating webhook rejects changing whether<br />this is enabled on an existing node group, since the operator has no mechanism to migrate<br />already-written data (flow definition, content, provenance history) from the combined<br />Storage PVC to the new per-repository layout. |  |
| <a id="nodegroup-persistentvolumeclaimretentionpolicy"></a>`persistentVolumeClaimRetentionPolicy` | _[PVCRetentionPolicy](#pvcretentionpolicy)_ |  |  | PersistentVolumeClaimRetentionPolicy overrides the cluster-level<br />PersistentVolumeClaimRetentionPolicy for this node group, field by field: an unset field<br />falls back to the cluster's, then to Retain. Can be changed at any time; the StatefulSet is<br />updated in place without restarting pods. |  |
| <a id="nodegroup-repositories"></a>`repositories` | _[RepositoriesSpec](#repositoriesspec)_ |  |  | Repositories overrides the cluster-level Repositories for this node group, field by field. |  |
| <a id="nodegroup-resources"></a>`resources` | _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ |  |  | Resources defines CPU/memory requests and limits for each node |  |
| <a id="nodegroup-poddisruptionbudget"></a>`podDisruptionBudget` | _[PodDisruptionBudgetSpec](#poddisruptionbudgetspec)_ |  |  | PodDisruptionBudget configures voluntary-disruption protection for this node group. |  |
| <a id="nodegroup-terminationgraceperiodseconds"></a>`terminationGracePeriodSeconds` | _integer_ |  | 120 | TerminationGracePeriodSeconds is the grace period given to a NiFi node to offload<br />data and disconnect from the cluster before it is force-killed. |  |
| <a id="nodegroup-annotations"></a>`annotations` | _object (keys:string, values:string)_ |  |  | Annotations are propagated to every object generated for this node group, merged<br />with (and taking precedence over) the cluster-level Annotations. |  |
| <a id="nodegroup-labels"></a>`labels` | _object (keys:string, values:string)_ |  |  | Labels are propagated to every object generated for this node group, merged with<br />(and taking precedence over) the cluster-level Labels. |  |
| <a id="nodegroup-tls"></a>`tls` | _[TLSSpec](#tlsspec)_ |  |  | TLS configures mutual TLS for this node group's REST API and cluster protocol traffic.<br />Off by default, preserving the unsecured HTTP behavior of existing clusters that don't<br />opt in. |  |
| <a id="nodegroup-configoverrides"></a>`configOverrides` | _[ConfigOverridesSpec](#configoverridesspec)_ |  |  | ConfigOverrides sets additional NiFi configuration properties for this node group,<br />merged with (and taking precedence over) the cluster-level ConfigOverrides. |  |
| <a id="nodegroup-podoverrides"></a>`podOverrides` | _[PodOverridesSpec](#podoverridesspec)_ |  |  | PodOverrides customizes this node group's generated Pod template beyond what the fields<br />above already expose (Resources, TLS, etc.), without needing a custom image. Each field is<br />additive to, or fully replaces, one specific part of the pod spec - see PodOverridesSpec's<br />own field docs. A value colliding with something the operator manages itself (the "nifi"<br />container name, its own env vars, or its own volume names) is rejected by the validating<br />webhook. |  |

#### NodeGroupStatus



NodeGroupStatus reports the observed state of a single node group.



_Appears in:_
- [NiFiClusterStatus](#nificlusterstatus)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="nodegroupstatus-name"></a>`name` | _string_ |  |  | Name is the node group this status applies to. |  |
| <a id="nodegroupstatus-replicas"></a>`replicas` | _integer_ |  |  | Replicas is the desired number of replicas for this node group. |  |
| <a id="nodegroupstatus-readyreplicas"></a>`readyReplicas` | _integer_ |  |  | ReadyReplicas is the number of ready replicas for this node group. |  |
| <a id="nodegroupstatus-updatedreplicas"></a>`updatedReplicas` | _integer_ |  |  | UpdatedReplicas is the number of replicas for this node group running the StatefulSet's<br />current pod template revision - distinct from ReadyReplicas, which a pod still on the<br />*previous* revision also counts toward. Needed to tell a genuinely finished rollout (every<br />pod on the new template, all ready) apart from the merely coincidental moment, part-way<br />through a rolling update, where the old and new pods' ready counts happen to add up to the<br />desired total (the UpgradeCompleted event and the Upgrading condition both require<br />this, not just ReadyReplicas).<br />Optional so that an operator one release older, which does not set it yet, can still<br />write status while ArgoCD has already applied this CRD. |  |
| <a id="nodegroupstatus-statefulsetobserved"></a>`statefulSetObserved` | _boolean_ |  |  | StatefulSetObserved is false for the one reconcile right after this node group's<br />StatefulSet spec changes, until the StatefulSet controller itself has caught up with it -<br />during that brief window ReadyReplicas/UpdatedReplicas above still reflect the *previous*<br />template, not the one just applied, so the UpgradeCompleted event and the Upgrading<br />condition both require this to be true before trusting those counts mean a rollout actually<br />finished. Caught live: without this, a pod still on the old image being Ready could make<br />ReadyReplicas alone equal the desired count before the rollout had even started.<br />Optional for the same reason; absent reads as false (not yet observed). |  |
| <a id="nodegroupstatus-conditions"></a>`conditions` | _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Conditions represent the latest available observations of this node group's state. |  |

#### PVCRetentionPolicy



PVCRetentionPolicy mirrors the StatefulSet's persistentVolumeClaimRetentionPolicy. Fields are
deliberately not defaulted by the CRD, so an unset node-group field can inherit the cluster's.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="pvcretentionpolicy-whendeleted"></a>`whenDeleted` | _[PVCRetentionType](#pvcretentiontype)_ |  |  | WhenDeleted applies when the node group's StatefulSet is deleted: the NiFiCluster is<br />deleted, or the node group is removed from it. With Delete the flow can't be recovered. | Enum: [Retain Delete] <br /> |
| <a id="pvcretentionpolicy-whenscaled"></a>`whenScaled` | _[PVCRetentionType](#pvcretentiontype)_ |  |  | WhenScaled applies to the PVCs of the nodes removed by a scale-down, after the operator has<br />offloaded them. With Delete a later scale-up starts those nodes empty. | Enum: [Retain Delete] <br /> |

#### PVCRetentionType

_Underlying type:_ _string_

PVCRetentionType is what happens to a node's PVCs: Retain keeps them, Delete removes them.

_Rules:_
- Enum: [Retain Delete]

_Appears in:_
- [PVCRetentionPolicy](#pvcretentionpolicy)

| Value | Description |
| --- | --- |
| `Retain` | PVCRetain keeps the PVCs, and with them the node's flow, repositories and logs.<br /> |
| `Delete` | PVCDelete deletes the PVCs and everything on them.<br /> |

#### PodDisruptionBudgetSpec



PodDisruptionBudgetSpec configures a PodDisruptionBudget for a node group.



_Appears in:_
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="poddisruptionbudgetspec-enabled"></a>`enabled` | _boolean_ |  | true | Enabled toggles generation of a PodDisruptionBudget for this node group. A *bool (rather<br />than bool) so an explicit "false" can actually be represented on the wire: with a plain<br />bool and omitempty, JSON marshaling would drop the zero value entirely, and the<br />default below would silently coerce it back to true. |  |
| <a id="poddisruptionbudgetspec-minavailable"></a>`minAvailable` | _integer_ |  |  | MinAvailable is the minimum number of pods that must remain available during a<br />voluntary disruption. Defaults to Replicas-1 when unset, and is validated by the<br />webhook to never equal or exceed Replicas. | Minimum: 0 <br /> |

#### PodOverridesSpec



PodOverridesSpec customizes a node group's generated Pod template. Scoped per node group
(unlike ConfigOverrides) since pod templates - scheduling, sidecars, extra volumes - are
already inherently per-node-group, one per StatefulSet.



_Appears in:_
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="podoverridesspec-env"></a>`env` | _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#envvar-v1-core) array_ |  |  | Env adds extra environment variables to the "nifi" container, alongside the ones the<br />operator sets itself (POD_NAME, POD_NAMESPACE, NIFI_CLUSTER_DOMAIN,<br />NIFI_SENSITIVE_PROPS_KEY, NIFI_AUTH_USERNAME, NIFI_AUTH_PASSWORD,<br />NIFI_AUTH_PASSWORD_HASH and, on TLS node groups, NIFI_KEYSTORE_PASSWORD). A name colliding with one of those is rejected by the validating<br />webhook, since silently shadowing one could break the entrypoint or authentication. | MaxItems: 64 <br /> |
| <a id="podoverridesspec-nodeselector"></a>`nodeSelector` | _object (keys:string, values:string)_ |  |  | NodeSelector constrains which nodes this node group's pods can be scheduled on. |  |
| <a id="podoverridesspec-tolerations"></a>`tolerations` | _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#toleration-v1-core) array_ |  |  | Tolerations let this node group's pods be scheduled onto nodes with matching taints. |  |
| <a id="podoverridesspec-affinity"></a>`affinity` | _[Affinity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#affinity-v1-core)_ |  |  | Affinity, when set, fully replaces the operator's own default pod anti-affinity (which<br />otherwise spreads this node group's pods across hosts) - useful e.g. on a single-node dev<br />cluster. The default zone topology spread constraint stays (see TopologySpreadConstraints);<br />it is ScheduleAnyway, so it never blocks scheduling, only prefers spreading across zones. |  |
| <a id="podoverridesspec-topologyspreadconstraints"></a>`topologySpreadConstraints` | _[TopologySpreadConstraint](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#topologyspreadconstraint-v1-core) array_ |  |  | TopologySpreadConstraints, when set, fully replace the operator's default one (spread this<br />node group's pods across topology.kubernetes.io/zone with maxSkew 1, ScheduleAnyway) -<br />e.g. to require zone spreading with whenUnsatisfiable: DoNotSchedule, or to spread across a<br />custom topology label. A constraint without a labelSelector gets this node<br />group's pod labels, so it counts exactly this node group's pods. |  |
| <a id="podoverridesspec-priorityclassname"></a>`priorityClassName` | _string_ |  |  | PriorityClassName sets this node group's pod priority class. Unset by default (the<br />cluster's own default priority applies). |  |
| <a id="podoverridesspec-podsecuritycontext"></a>`podSecurityContext` | _[PodSecurityContext](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#podsecuritycontext-v1-core)_ |  |  | PodSecurityContext is merged, field by field, over the pod security context from the<br />operator's defaults and the cluster's spec.podSecurityContext. |  |
| <a id="podoverridesspec-containersecuritycontext"></a>`containerSecurityContext` | _[SecurityContext](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#securitycontext-v1-core)_ |  |  | ContainerSecurityContext is merged, field by field, over the "nifi" container's security<br />context from the operator's defaults and the cluster's spec.containerSecurityContext. |  |
| <a id="podoverridesspec-initcontainers"></a>`initContainers` | _[Container](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#container-v1-core) array_ |  |  | InitContainers adds extra init containers to the pod, run in order (after the operator's<br />own "nifi-init") before the "nifi" container starts. A container named "nifi" or<br />"nifi-init" is rejected. One without a securityContext gets the restricted defaults of the<br />"nifi" container. | MaxItems: 64 <br /> |
| <a id="podoverridesspec-sidecars"></a>`sidecars` | _[Container](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#container-v1-core) array_ |  |  | Sidecars adds extra containers to the pod, running alongside the "nifi" container. A<br />container named "nifi" or "nifi-init" is rejected. One without a securityContext gets the<br />restricted defaults of the "nifi" container. | MaxItems: 64 <br /> |
| <a id="podoverridesspec-volumes"></a>`volumes` | _[Volume](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#volume-v1-core) array_ |  |  | Volumes adds extra volumes to the pod, for InitContainers/Sidecars to mount (e.g. a<br />shared emptyDir, a mounted Secret/ConfigMap). A name colliding with one of the operator's<br />own volumes ("data", "logs", "config", "tls", "nifi-scratch", ...) is rejected by the<br />validating webhook. | MaxItems: 64 <br /> |
| <a id="podoverridesspec-imagepullpolicy"></a>`imagePullPolicy` | _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#pullpolicy-v1-core)_ |  |  | ImagePullPolicy sets the "nifi" container's image pull policy. Unset by default, which<br />leaves Kubernetes' own standard default in effect (Always for a ":latest" tag,<br />IfNotPresent otherwise) - unchanged from the behavior before this field existed. | Enum: [Always Never IfNotPresent] <br /> |
| <a id="podoverridesspec-imagepullsecrets"></a>`imagePullSecrets` | _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#localobjectreference-v1-core) array_ |  |  | ImagePullSecrets overrides this node group's image pull secrets, taking precedence over<br />the cluster-level Spec.ImagePullSecrets when set. Falls back to the cluster-level value<br />when unset - so most clusters never need to set this per node group. |  |
| <a id="podoverridesspec-serviceaccountname"></a>`serviceAccountName` | _string_ |  |  | ServiceAccountName overrides this node group's ServiceAccount, taking precedence over<br />NiFiClusterSpec.ServiceAccountName when set. Falls back to the cluster-level value (and<br />from there, the operator's own dedicated ServiceAccount) when unset.<br />With Kubernetes-Lease cluster coordination, this ServiceAccount needs the coordination<br />Role (Leases and ConfigMaps in this namespace). The operator binds it only when both the<br />operator admin (--allow-custom-service-account-binding) and this cluster<br />(coordination.bindPodServiceAccounts) opt in - the same no-automatic-elevation policy as<br />for OpenShift SCCs, which it never binds to a ServiceAccount you name. Otherwise bind it<br />yourself: kubectl create rolebinding <cluster>-nifi-<sa> --role=<cluster>-nifi<br />--serviceaccount=<namespace>:<sa>. The CoordinationRBACReady condition reports any<br />ServiceAccount still missing it. |  |
| <a id="podoverridesspec-hostaliases"></a>`hostAliases` | _[HostAlias](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#hostalias-v1-core) array_ |  |  | HostAliases adds extra entries to the pod's /etc/hosts. None by default. |  |

#### ProvenanceRepositorySpec



ProvenanceRepositorySpec sets nifi.provenance.repository.max.storage.*.



_Appears in:_
- [RepositoriesSpec](#repositoriesspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="provenancerepositoryspec-maxstoragetime"></a>`maxStorageTime` | _string_ |  |  | MaxStorageTime is how long provenance events are kept, in NiFi's time format, e.g.<br />"30 days" (nifi.provenance.repository.max.storage.time). | MaxLength: 64 <br />Pattern: `^[0-9]+(\.[0-9]+)? *(ms\|millis\|milliseconds\|s\|sec\|secs\|second\|seconds\|m\|min\|mins\|minute\|minutes\|h\|hr\|hrs\|hour\|hours\|d\|day\|days\|w\|wk\|wks\|week\|weeks)$` <br /> |
| <a id="provenancerepositoryspec-maxstoragesize"></a>`maxStorageSize` | _string_ |  |  | MaxStorageSize caps the provenance repository's size, as a Kubernetes quantity, e.g. "10Gi"<br />(nifi.provenance.repository.max.storage.size). | MaxLength: 64 <br /> |

#### RepositoriesSpec



RepositoriesSpec holds typed nifi.properties settings for NiFi's repositories.



_Appears in:_
- [NiFiClusterSpec](#nificlusterspec)
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="repositoriesspec-contentarchive"></a>`contentArchive` | _[ContentArchiveSpec](#contentarchivespec)_ |  |  | ContentArchive controls the archive of content no longer referenced by any FlowFile. |  |
| <a id="repositoriesspec-provenance"></a>`provenance` | _[ProvenanceRepositorySpec](#provenancerepositoryspec)_ |  |  | Provenance limits how long and how much provenance history is kept. |  |
| <a id="repositoriesspec-flowfile"></a>`flowFile` | _[FlowFileRepositorySpec](#flowfilerepositoryspec)_ |  |  | FlowFile tunes the FlowFile repository. |  |

#### SingleUserAuthSpec



SingleUserAuthSpec sets NiFi's single-user login credentials, from exactly one of two
mutually exclusive sources - enforced by the validating webhook, since the CRD schema alone
can't express "exactly one of A or B".



_Appears in:_
- [AuthenticationSpec](#authenticationspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="singleuserauthspec-username"></a>`username` | _string_ |  |  | Username, together with Password, sets the login credentials directly as plain values in<br />this CR. Simple, but stored unencrypted in this object (visible via `kubectl get<br />nificluster -o yaml`, and in etcd) - acceptable for early-stage/internal use, not<br />recommended once this matters for a real production credential. Mutually exclusive with<br />SecretName. |  |
| <a id="singleuserauthspec-password"></a>`password` | _string_ |  |  | Password, see Username above. Must be at least 12 characters (NiFi's single-user login<br />minimum). Enforced by the validating webhook rather than a CRD MinLength marker: the API<br />server's schema-validation error echoes the rejected value, which would leak the password. |  |
| <a id="singleuserauthspec-secretname"></a>`secretName` | _string_ |  |  | SecretName is the name of an existing Secret (in the NiFiCluster's own namespace)<br />containing "username" and "password" keys - keeps the actual credential values out of<br />this CR entirely. Mutually exclusive with Username/Password. Changing the Secret's<br />contents rolls the cluster's pods so the new credentials take effect. |  |

#### SplitRepositoryStorageSpec



SplitRepositoryStorageSpec configures the per-repository storage split for a node group - see
NodeGroup.SplitRepositoryStorage's own doc comment for the full behavior and migration
caveat. Every size field is optional; a default is applied when unset, so a user can override
just the one repository they care about (e.g. a much bigger Content repository) without
having to specify sizes for the rest.
The repository sizes can only grow once the node group has its volumes (mirrors the webhook).



_Appears in:_
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="splitrepositorystoragespec-enabled"></a>`enabled` | _boolean_ |  | false | Enabled toggles the per-repository storage split. |  |
| <a id="splitrepositorystoragespec-flowfilerepositorystorage"></a>`flowFileRepositoryStorage` | _string_ |  | 5Gi | FlowFileRepositoryStorage is the persistent volume size for the FlowFile repository<br />(e.g. "5Gi"). | MaxLength: 64 <br /> |
| <a id="splitrepositorystoragespec-contentrepositorystorage"></a>`contentRepositoryStorage` | _string_ |  | 10Gi | ContentRepositoryStorage is the persistent volume size for the Content repository.<br />Defaults larger than the other repositories, since this one holds the actual flowing<br />data. | MaxLength: 64 <br /> |
| <a id="splitrepositorystoragespec-provenancerepositorystorage"></a>`provenanceRepositoryStorage` | _string_ |  | 5Gi | ProvenanceRepositoryStorage is the persistent volume size for the Provenance repository. | MaxLength: 64 <br /> |
| <a id="splitrepositorystoragespec-databaserepositorystorage"></a>`databaseRepositoryStorage` | _string_ |  | 2Gi | DatabaseRepositoryStorage is the persistent volume size for the database repository<br />(NiFi's flow definition and internal H2 database). | MaxLength: 64 <br /> |
| <a id="splitrepositorystoragespec-extensionsstorage"></a>`extensionsStorage` | _string_ |  | 5Gi | ExtensionsStorage is the persistent volume size for custom NARs/jars and custom Python<br />extensions. | MaxLength: 64 <br /> |

#### TLSSpec



TLSSpec configures mutual TLS for a node group. The operator owns a self-signed CA (one per
NiFiCluster, created once and never rotated - see reconcileTLSCA); a separate CSI driver mints
a fresh, per-pod leaf certificate at pod-mount time, signed by that CA, as a PKCS12 keystore and
truststore (NiFi 2 has no PEM keystore support).



_Appears in:_
- [NiFiNodeGroupSpec](#nifinodegroupspec)
- [NodeGroup](#nodegroup)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="tlsspec-enabled"></a>`enabled` | _boolean_ |  | false | Enabled toggles TLS/mTLS for this node group. On by default for new node groups: the<br />defaulting webhook adds tls.enabled: true to a node group created without a tls block. A tls<br />block without enabled means TLS off. |  |
| <a id="tlsspec-extrasans"></a>`extraSANs` | _string array_ |  |  | ExtraSANs are additional DNS names included on every pod's certificate in this node<br />group, beyond that pod's own internal FQDN - e.g. an external hostname clients use<br />through a proxy or load balancer that passes TLS through. | MaxItems: 64 <br />items:MaxLength: 253 <br /> |
| <a id="tlsspec-certificatevaliditydays"></a>`certificateValidityDays` | _integer_ |  | 365 | CertificateValidityDays is the leaf certificate's validity period. A pod is restarted<br />(and issued a fresh certificate) once it has run for roughly half of this period,<br />mirroring the proactive-renewal pattern used by other Kubernetes CSI-based secret<br />provisioners. | Minimum: 1 <br /> |
| <a id="tlsspec-keystorepasswordsecretname"></a>`keystorePasswordSecretName` | _string_ |  |  | KeystorePasswordSecretName names a Secret (in this NiFiCluster's own namespace, "password"<br />key) whose value is used as every pod's PKCS12 keystore/truststore password, instead of the<br />operator generating one itself. This password only protects the on-disk PKCS12 encoding,<br />not the key material inside it - the private key is exactly as sensitive with or without a<br />password on the container around it - so this field exists for organizations whose own<br />compliance/rotation tooling needs to own every password in the system, not because the<br />operator-generated default is otherwise insecure. All node groups of one cluster share one<br />CA (and so one keystore password); set this the same way on every TLS-enabled node group,<br />or leave it unset on all of them. Read once, when the cluster's CA Secret is first created,<br />and never re-read afterward - the same create-once rule as the CA itself, since changing<br />the password after pods already have keystores encrypted with the old one would lock them<br />out until every pod's certificate is reissued. |  |

#### ZooKeeperSpec



ZooKeeperSpec points the cluster at an existing ZooKeeper ensemble.



_Appears in:_
- [CoordinationSpec](#coordinationspec)

| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
| <a id="zookeeperspec-connectstring"></a>`connectString` | _string_ | ✓ |  | ConnectString lists the ensemble's servers as comma-separated host:port pairs (e.g.<br />"zk-0.zk:2181,zk-1.zk:2181,zk-2.zk:2181"), without a chroot suffix - use RootNode for that.<br />Changing it is only safe while it still reaches the same ensemble (e.g. adding a server):<br />nodes restart one at a time, and nodes on two different ensembles would form two clusters. | MaxLength: 1024 <br />MinLength: 1 <br /> |
| <a id="zookeeperspec-rootnode"></a>`rootNode` | _string_ |  |  | RootNode is the znode under which this cluster keeps its leader election and state.<br />Defaults to /nifi/<namespace>/<name>, unique per NiFiCluster: two clusters sharing a root<br />node would merge into one. Immutable once set, for the same reason as the backend. | MaxLength: 255 <br />Pattern: `^(/[A-Za-z0-9_.\-]+)+$` <br /> |


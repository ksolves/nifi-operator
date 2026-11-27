# CRD reference

The operator adds two custom resources in the API group `nifi.ksolves.com`, version `v1alpha1`.
This page gives an overview of each; [API reference](api-reference.md) lists every field, with its
type, default and validation, and is generated from the API types (`make api-docs`).

The CRD manifests themselves are in the repository:
[`nificlusters`](../config/crd/bases/nifi.ksolves.com_nificlusters.yaml) and
[`nifinodegroups`](../config/crd/bases/nifi.ksolves.com_nifinodegroups.yaml). The Helm chart
installs them. `kubectl explain nificlusters.nifi.ksolves.com.spec` shows the same descriptions on a
cluster.

## NiFiCluster

| | |
|---|---|
| API version | `nifi.ksolves.com/v1alpha1` |
| Kind | `NiFiCluster` |
| Plural | `nificlusters` (use `nificlusters.nifi.ksolves.com` with kubectl) |
| Scope | Namespaced |
| Subresources | `status` |

One Apache NiFi cluster, made of one node group (`spec.nodeGroups` holds exactly one entry),
which runs as a StatefulSet. NiFiNodeGroup objects of earlier versions that reference the cluster
still join it.

`kubectl get nificlusters.nifi.ksolves.com` shows `Ready`, `Replicas` and `Age`; `-o wide` adds:

| Column | From |
|---|---|
| Coordination | `.status.coordinationBackend` |
| Auth Secret | `.status.authSecretName`, the Secret holding the login |
| Version | `.status.nifiVersion`, as NiFi reports it |
| Healthy | the `ClusterHealthy` condition |

### Minimal example

```yaml
apiVersion: nifi.ksolves.com/v1alpha1
kind: NiFiCluster
metadata:
  name: nifi
  namespace: nifi
spec:
  nodeGroups:
    - name: default
      replicas: 2
      storage: 10Gi
      tls:
        enabled: true
```

[`config/samples/nifi-full-customization.yaml`](../config/samples/nifi-full-customization.yaml)
shows every field with comments; the other samples in [`config/samples`](../config/samples) cover
login, local storage and single features.

### Spec at a glance

| Area | Fields | Guide |
|---|---|---|
| Image | [`image`](api-reference.md#nificlusterspec-image), [`imagePullSecrets`](api-reference.md#nificlusterspec-imagepullsecrets) | [Compatibility](compatibility.md) |
| Node groups | [`nodeGroups`](api-reference.md#nificlusterspec-nodegroups) (see [NodeGroup](#node-group-fields)) | [Scaling](scaling.md), [NiFiNodeGroup objects](autoscaling.md) |
| Exposure | [`serviceType`](api-reference.md#nificlusterspec-servicetype), [`service`](api-reference.md#nificlusterspec-service), [`proxyHosts`](api-reference.md#nificlusterspec-proxyhosts) | [Exposure](exposure.md) |
| Login | [`authentication`](api-reference.md#nificlusterspec-authentication) | [Authentication](authentication.md) |
| Coordination | [`coordination`](api-reference.md#nificlusterspec-coordination) | [Cluster coordination](cluster-coordination.md) |
| NiFi configuration | [`configOverrides`](api-reference.md#nificlusterspec-configoverrides), [`repositories`](api-reference.md#nificlusterspec-repositories) | [Storage](storage.md) |
| Storage lifecycle | [`persistentVolumeClaimRetentionPolicy`](api-reference.md#nificlusterspec-persistentvolumeclaimretentionpolicy) | [Storage](storage.md) |
| Pod security | [`serviceAccountName`](api-reference.md#nificlusterspec-serviceaccountname), [`podSecurityContext`](api-reference.md#nificlusterspec-podsecuritycontext), [`containerSecurityContext`](api-reference.md#nificlusterspec-containersecuritycontext) | [Installation: pod security](installation.md#pod-security) |
| Metadata | [`labels`](api-reference.md#nificlusterspec-labels), [`annotations`](api-reference.md#nificlusterspec-annotations) | [Installation: labels](installation.md#labels-and-annotations-on-generated-objects) |

### Node group fields

Each entry of `spec.nodeGroups` (and the spec of a NiFiNodeGroup) is a
[NodeGroup](api-reference.md#nodegroup). A cluster has at most 3 NiFi nodes in total, counting
NiFiNodeGroups of earlier versions (`replicas` 1 to 3):

| Area | Fields |
|---|---|
| Size | [`name`](api-reference.md#nodegroup-name), [`replicas`](api-reference.md#nodegroup-replicas), [`resources`](api-reference.md#nodegroup-resources) |
| Storage | [`storage`](api-reference.md#nodegroup-storage), [`logStorage`](api-reference.md#nodegroup-logstorage), [`storageClassName`](api-reference.md#nodegroup-storageclassname), [`splitRepositoryStorage`](api-reference.md#nodegroup-splitrepositorystorage), [`repositories`](api-reference.md#nodegroup-repositories), [`persistentVolumeClaimRetentionPolicy`](api-reference.md#nodegroup-persistentvolumeclaimretentionpolicy) |
| TLS | [`tls`](api-reference.md#nodegroup-tls) ([TLS](tls.md)) |
| Availability | [`podDisruptionBudget`](api-reference.md#nodegroup-poddisruptionbudget), [`terminationGracePeriodSeconds`](api-reference.md#nodegroup-terminationgraceperiodseconds) |
| Configuration | [`configOverrides`](api-reference.md#nodegroup-configoverrides), [`podOverrides`](api-reference.md#nodegroup-podoverrides), [`labels`](api-reference.md#nodegroup-labels), [`annotations`](api-reference.md#nodegroup-annotations) |

Some fields cannot change once the node group exists: `storage`, `logStorage`, the split
repository sizes, `storageClassName` and `splitRepositoryStorage.enabled`. `coordination.backend` and
`coordination.zookeeper.rootNode` are fixed when the cluster is created.

### Status

| Field | Meaning |
|---|---|
| `conditions` | `Available`, `ClusterHealthy`, `Degraded`, `Upgrading`, `CoordinationUpToDate` and more ([Troubleshooting](troubleshooting.md#reading-the-clusters-status)) |
| `replicas`, `readyReplicas` | desired and ready NiFi pods over all node groups |
| `nodeGroupStatuses` | the same per node group |
| `nifiVersion`, `clusterCoordinator`, `primaryNode`, `nodes`, `lastNiFiPoll` | NiFi's own view of the cluster ([Status](status.md)) |
| `coordinationBackend` | the backend the cluster runs on |
| `authSecretName` | the Secret holding the single-user login |
| `nodeRemovalPending` | a scale-down is waiting to remove departed nodes from NiFi |

[NiFiClusterStatus](api-reference.md#nificlusterstatus) has the details.

### Names the operator reserves

The operator sets these itself, so a NiFiCluster cannot set them (the admission webhook rejects it):

- environment variables in `podOverrides.env`: `POD_NAME`, `POD_NAMESPACE`, `NIFI_CLUSTER_DOMAIN`,
  `NIFI_SENSITIVE_PROPS_KEY`, `NIFI_AUTH_USERNAME`, `NIFI_AUTH_PASSWORD`,
  `NIFI_AUTH_PASSWORD_HASH`, `NIFI_KEYSTORE_PASSWORD`;
- container names `nifi` and `nifi-init`;
- volume names `data`, `logs`, `config`, `config-secret`, `tls`, `flowfile-repository`,
  `content-repository`, `provenance-repository`, `database-repository`, `extensions`,
  `nifi-scratch`, `nifi-conf`, `nifi-passwd`;
- `nifi.properties` keys the operator manages (web, cluster, TLS, coordination and repository
  settings), in `configOverrides.nifiProperties`.

### Annotations you can set

| Annotation | Effect |
|---|---|
| `nifi.ksolves.com/approve-coordination-migration: <Kubernetes\|ZooKeeper>` | Approves stopping every NiFi node to move the cluster's StatefulSets to the coordination backend named ([Cluster coordination](cluster-coordination.md)). |

## NiFiNodeGroup

| | |
|---|---|
| API version | `nifi.ksolves.com/v1alpha1` |
| Kind | `NiFiNodeGroup` |
| Plural / short name | `nifinodegroups` / `nng` |
| Scope | Namespaced |
| Subresources | `status`, `scale` (`.spec.replicas`, `.status.replicas`, `.status.selector`) |

Not available in the Community edition: the webhook rejects creating one. NiFiNodeGroups created
by earlier versions keep working and can be updated, scaled and deleted
([NiFiNodeGroup objects](autoscaling.md)). Its spec is a
[NodeGroup](#node-group-fields) plus `clusterRef`, the NiFiCluster it joins (immutable).
`spec.name` must equal `metadata.name`; the webhook fills it in when omitted.

`kubectl get nng` shows `Cluster`, `Ready`, `Replicas` and `Age`.

```yaml
apiVersion: nifi.ksolves.com/v1alpha1
kind: NiFiNodeGroup
metadata:
  name: workers
  namespace: nifi
spec:
  clusterRef: nifi
  replicas: 1              # with the example cluster's 2 nodes: 3 in total, the limit
  storage: 10Gi
  tls:
    enabled: true          # must match the cluster's other node groups
```

Its status has `replicas`, `readyReplicas`, `selector` and the conditions `Ready` and `Conflict`
([NiFiNodeGroupStatus](api-reference.md#nifinodegroupstatus)).

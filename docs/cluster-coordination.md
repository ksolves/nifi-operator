# Cluster coordination

Every node group of a `NiFiCluster` joins **one** NiFi cluster. The nodes elect their Cluster
Coordinator and Primary Node, and keep cluster-wide component state, through the backend in
`spec.coordination.backend`.

| Backend | Leader election | Cluster state | Status |
|---|---|---|---|
| `Kubernetes` (default) | `Lease` objects (`KubernetesLeaderElectionManager`) | `ConfigMap` per component (`KubernetesConfigMapStateProvider`) | Supported |
| `ZooKeeper` | external ensemble (`CuratorLeaderElectionManager`) | znodes under the cluster's root node (`ZooKeeperStateProvider`) | Not available in the Community edition; clusters created with it by earlier versions keep running |

No ZooKeeper is deployed. Older operator versions ran an embedded ZooKeeper quorum inside every
node group, which made each node group a separate NiFi cluster; see [Migrating](#migrating-a-cluster-from-an-older-operator-version).

```yaml
spec:
  coordination:
    backend: Kubernetes   # optional; unset means Kubernetes
```

`spec.coordination.backend` is **immutable once set** (enforced by the API server). Switching the
backend of a running NiFi cluster risks split-brain and loses component state.

## Clusters on external ZooKeeper

The CRD accepts `backend: Kubernetes` only (`Unsupported value: "ZooKeeper"`), so no new cluster
can use ZooKeeper. A cluster created on it by an earlier operator version keeps running on its
ensemble, and edits that leave `spec.coordination` unchanged still validate (CRD validation
ratcheting, Kubernetes 1.30+). Its spec looks like this:

```yaml
spec:
  coordination:
    backend: ZooKeeper
    zookeeper:
      connectString: zk-0.zk:2181,zk-1.zk:2181,zk-2.zk:2181   # host:port list, no chroot
      rootNode: /nifi/team-a/prod                            # optional
```

- The operator never deploys ZooKeeper; point it at an ensemble you run.
- `rootNode` defaults to `/nifi/<namespace>/<name>`, unique per NiFiCluster (two clusters on one
  root node would merge). It is immutable: it cannot be added, changed or removed after creation.
- `connectString` may change, but only while it still reaches the same ensemble: nodes restart one
  at a time, and nodes on two different ensembles would form two clusters.
- NiFi pods coordinated through ZooKeeper get no Role and no ServiceAccount token (they never call
  the Kubernetes API); the ServiceAccount and, on OpenShift, its SCC binding remain.
- TLS and SASL to ZooKeeper are not configured by the operator yet.

## Requirements

- **NiFi 2.11 or newer** in the node image for Kubernetes coordination (see
  [Compatibility](compatibility.md)). Kubernetes coordination exists since 2.0.0, but releases
  before 2.11 read the Lease on every scheduling decision ([NIFI-16120](https://issues.apache.org/jira/browse/NIFI-16120)),
  which can put hundreds of requests per second per node on the API server. The operator's own image
  (`images/nifi`) is built on 2.12.0.
- All node groups must use the same `tls.enabled` value (the cluster protocol is TLS-secured on
  every node or on none). The validating webhook enforces this.

## What the operator creates

| Object | Name | Purpose |
|---|---|---|
| ServiceAccount | `<name>-nifi` | NiFi pods run as it (skipped when `spec.serviceAccountName` is set) |
| Role + RoleBinding | `<name>-nifi` | `leases`: create/get/update/patch; `configmaps`: create/delete/get/list/patch/update. Bound to the pods' ServiceAccount, and to node groups' own ServiceAccounts only when both opt-ins below are set |
| RoleBinding (OpenShift only) | `<name>-nifi-scc` | Binds `<name>-nifi` to `system:openshift:scc:<scc>` |

All of them are owned by the `NiFiCluster` and deleted with it. The operator never adopts a
pre-existing object of the same name; it reports an error instead.

**OpenShift:** the SCC comes from the chart value `nifiPods.openshiftSCC` (default empty, so no
binding; passed to the operator as `--nifi-scc`; see [Installation](installation.md#openshift)). It is only bound to the operator-created ServiceAccount,
never to a `spec.serviceAccountName` you supply: a ServiceAccount you name needs its SCC granted by
the namespace administrator.

## ServiceAccounts of your own

Every ServiceAccount a cluster's NiFi pods run as needs the coordination Role, or those pods cannot
elect a coordinator or keep component state.

- **`spec.serviceAccountName`** (all node groups): the operator binds the Role to it.
- **`nodeGroups[].podOverrides.serviceAccountName`** (one node group): the operator binds the Role to
  it only when **both** opt-ins are set:
  - the operator admin allows it: chart value `coordination.allowCustomServiceAccountBinding: true`
    (operator flag `--allow-custom-service-account-binding`), off by default;
  - the cluster asks for it: `spec.coordination.bindPodServiceAccounts: true`. The webhook only
    accepts this from someone who may themselves update Leases and ConfigMaps in the namespace,
    so nobody can use the operator to grant a ServiceAccount more than they hold.

  Otherwise bind it yourself (the webhook prints this command when you create or update the
  cluster):

  ```sh
  kubectl -n <ns> create rolebinding <name>-nifi-<sa> --role=<name>-nifi --serviceaccount=<ns>:<sa>
  ```

The **`CoordinationRBACReady`** condition reports whether every ServiceAccount the pods run as may
actually use Leases and ConfigMaps, asked of the API server's own authorizer (a SubjectAccessReview
per ServiceAccount and permission), so it also sees bindings you made yourself, and a binding
removed later. `False` (reason `CoordinationRBACMissing`, with a Warning event) names each
ServiceAccount, its node groups, the missing permissions and the command above. It is checked
every minute while it passes, and reported `False` only after two failed checks 10 seconds apart,
since the authorizer can lag a RoleBinding that was just written. ZooKeeper clusters do not get the
condition.

NiFi itself creates, with the `NiFiCluster` name as prefix:

- Leases `<name>-cluster-coordinator` and `<name>-primary-node`
- ConfigMaps `<name>-nifi-component-<component id>` (cluster state of each stateful component)

These carry no owner reference. On `NiFiCluster` deletion the operator stops the NiFi pods and
then deletes the two **Leases**. The component-state **ConfigMaps are kept**, like the PVCs and the
sensitive-props key, so a `NiFiCluster` recreated with the same name resumes its flow and
processor state, unless every node group's `persistentVolumeClaimRetentionPolicy.whenDeleted` is
`Delete`: then they are deleted with the PVCs (see [Storage and retention](storage.md)). To start
from scratch instead, remove them yourself:

```sh
kubectl -n <ns> get configmap -o name | grep "^configmap/<name>-nifi-component-" | xargs -r kubectl -n <ns> delete
```

With the **ZooKeeper** backend the operator never touches the ensemble. Leader-election and state
znodes stay under the cluster's root node (default `/nifi/<namespace>/<name>`) until you remove
them, for example with `zkCli.sh deleteall <rootNode>`.

## Limits

- Each component's state must fit in one ConfigMap (1 MiB). Listing processors that track very
  many entities can hit this; ZooKeeper had a similar ~1 MB limit (`jute.maxbuffer`).
- Clearing a component's state in the UI makes the nodes reconnect ([NIFI-14125](https://issues.apache.org/jira/browse/NIFI-14125), open upstream).
- Lease timing is fixed by NiFi (see below).

## Lease timing and failover

NiFi 2.12's `KubernetesLeaderElectionManager` uses fixed values, read from its bytecode
(`LeaderElectionCommand`); no `nifi.properties` key changes them:

| Setting | Value | Meaning |
|---|---|---|
| Lease duration | 15 s | how long a leader's claim is valid without renewal |
| Renew deadline | 10 s | how long the leader keeps retrying a renewal before giving up the role |
| Retry period | 2 s | how often candidates try to acquire, and the leader renews |
| Leader cache | 5 s | how long a node caches who the leader is |

So when the Cluster Coordinator's pod dies abruptly, another node takes the Lease at most about
15-17 seconds later; a pod that shuts down cleanly releases it sooner. Measure it on your own
cluster (disruptive: it deletes the coordinator's pod) with:

```sh
hack/coordination-failover.sh <ns> <name>
```

Measured: see [Sizing and measurements](#sizing-and-measurements). The nodes then reconnect to the
new coordinator; see [status.md](status.md) for the short window in which they report
`CONNECTING`.

## Sizing and measurements

`hack/coordination-load.sh <minutes>` reports Lease and ConfigMap requests per second, by verb,
plus API Priority and Fairness rejections and HTTP 429s, from the API server's `/metrics`. The
counters are cluster-wide: run it once with the NiFi cluster running and once without it, and
subtract.

Measured on kind (Kubernetes 1.31), NiFi 2.12, an idle flow, 3-minute windows, against a baseline
without the cluster:

| NiFi nodes | Lease GET/s | Lease PATCH/s | Lease PUT/s | ConfigMap/s | Total | Per node |
|---|---|---|---|---|---|---|
| 3 | +2.9 | +1.0 | 0 | 0 | ~3.9 | ~1.3 |

The pattern: every node reads both Leases about every 2 seconds (the retry period), and the two
leaders renew theirs once every 2 seconds, so roughly **0.9 × nodes + 1 requests per second** per
cluster, about 4 per second at the 3-node maximum. No request was rejected (no API Priority and Fairness
rejections, no 429s). ConfigMap traffic depends on the flow: each stateful component (ListSFTP,
QueryDatabaseTable, ...) writes its state ConfigMap when its state changes; an idle flow writes
none.

Measured on kind (Kubernetes 1.31) against a baseline taken just before.

Failover, measured with `hack/coordination-failover.sh` (the coordinator's pod deleted): a new
coordinator was elected within 15 to 19 seconds.

## Migrating a cluster from an older operator version

After upgrading the operator, an existing cluster whose StatefulSets were built for embedded
ZooKeeper is **left untouched** - no restart, no config change - and reports:

```
CoordinationUpToDate=False  reason=MigrationAwaitingApproval
```

plus a Warning Event. Spec changes to such a cluster are not applied until it is migrated.

Migrating stops **all** NiFi nodes of the cluster, then starts them as one cluster on the new
backend (a few minutes of downtime). Flows, repositories and the sensitive-props key are kept
(the key of the old per-node-group Secret is adopted, so encrypted properties still decrypt).
Cluster-scoped component state held in the old ZooKeeper is **not** carried over: listing
processors may list again.

To approve, set the annotation to the backend you are migrating to:

```sh
kubectl annotate nificluster.nifi.ksolves.com <name> nifi.ksolves.com/approve-coordination-migration=Kubernetes
```

The condition moves to `MigrationStoppingNodes` while nodes stop, then to `UpToDate`. A cluster
whose node groups were created with *different* sensitive-props keys cannot be merged
automatically (`reason=ConflictingSensitivePropsKeys`): keep one node group, or recreate the
others with empty data, before approving.

# Scaling a NiFi cluster

A NiFiCluster has one node group of at most 3 NiFi nodes. The CRD and the admission webhook
reject anything larger; NiFiNodeGroup objects of earlier versions count towards the 3 nodes (see
[NiFiNodeGroup objects](autoscaling.md)).

Change `replicas` on the node group. The other nodes keep running: the
values that change with the node count (such as `nifi.cluster.flow.election.max.candidates`) are
not part of the pod-restart hash.

## Scaling down

1. **The node leaves cleanly.** Before Kubernetes stops a NiFi pod, its preStop hook asks the node
   how many nodes are connected. With other connected nodes it runs `nifi.sh decommission`:
   disconnect, offload its queued FlowFiles to the remaining nodes, then stop. The last
   connected node just stops (there is nowhere to offload to). On TLS clusters the hook logs in
   with the cluster's login credentials to ask; if that fails it still attempts the
   decommission, with a short retry.
2. **The operator removes it from the NiFi cluster.** NiFi keeps listing a departed node as
   `DISCONNECTED`/`OFFLOADED` forever. Once every remaining pod is ready, the operator deletes
   such nodes through the NiFi REST API (only nodes that are no longer in the spec and not
   connected), and records a `NodeRemoved` event for each. If the departed node was the Cluster
   Coordinator, the new coordinator lists it as connected until its heartbeats time out (about
   40 seconds); the operator waits for that.

**One node at a time.** Lowering `replicas` by more than one (say 3 to 1) is applied in steps:
the operator lowers the StatefulSet's replicas by one, waits until that pod is gone, then takes
the next step. Only one node offloads at a time, onto every node that remains.

`terminationGracePeriodSeconds` (default 120) must cover the offload of one node. If the pod is
killed mid-offload, its remaining FlowFiles stay on its retained PVC and the operator still removes
the node from the NiFi cluster. Size it from the data a node holds: queued bytes divided by the
rate the remaining nodes can take it in, plus a minute for the disconnect itself. Check a node's
queued data under **Cluster** > **Nodes** in the NiFi UI before scaling down a busy cluster, or
in `status.nodes[].queued` (see [status.md](status.md)).

**Warning when the grace period looks too short.** At each scale-down step the operator checks
the queued data of the node it is about to remove (NiFi's own figure in `status.nodes`). If that
data would take longer than the node's `terminationGracePeriodSeconds` to offload at the assumed
throughput, it records a Warning event `OffloadMayExceedGracePeriod` naming the node, its queued
data and the estimate. The step still goes ahead. The assumed throughput is the operator flag
`--offload-throughput-bytes-per-second` (chart value `offloadThroughputBytesPerSecond`), 10 MiB/s
by default; set it to what your cluster actually achieves.

```sh
kubectl get events --field-selector reason=OffloadMayExceedGracePeriod
```

## Scheduling defaults

- Pods of a node group prefer different hosts (preferred pod anti-affinity on
  `kubernetes.io/hostname`) and spread across zones (`topology.kubernetes.io/zone`, `maxSkew: 1`,
  `whenUnsatisfiable: ScheduleAnyway`): neither ever blocks scheduling.
- A PodDisruptionBudget keeps all but one node of each group available (`minAvailable: replicas-1`)
  during voluntary disruptions: drains, cluster upgrades, evictions. Each evicted pod's preStop
  hook first offloads its FlowFiles to the other nodes.
- To require zone spreading, or spread over your own topology label, set
  `nodeGroups[].podOverrides.topologySpreadConstraints`; it replaces the default constraint, and a
  constraint without a `labelSelector` gets the node group's own pod labels:

  ```yaml
  podOverrides:
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: DoNotSchedule
  ```

- Use a StorageClass with `volumeBindingMode: WaitForFirstConsumer`, so each volume is created in
  the zone (or on the node) where its pod is first scheduled.

## Status

- `status.nodeRemovalPending` is `true` from the scale-down until the departed nodes are removed.
  It is also set when a restarted pod (rolling restart, pod kill, eviction) has rejoined NiFi under
  a new node ID while NiFi still lists its old identity as disconnected; the operator then deletes
  that old identity the same way, once every pod is ready.
- `MembershipUpToDate=False` appears only when removal is blocked:
  - `NoCredentials`: the cluster's login credentials Secret does not exist (yet).
  - `NiFiAPIError`: the NiFi API call failed; retried every 30 seconds.
- `ScaleDownBlocked=True` appears only when a scale-down step cannot confirm the node it is about
  to remove has offloaded its data:
  - `NoCredentials`: same as above, for the scale-down path.
  - `APIUnreachable`: the NiFi API call failed; retried on the next reconcile.
  - `NodeUnresponsive`: NiFi rejected the disconnect/offload request itself. If the node can never
    respond (crashed, or NiFi leaves it stuck OFFLOADING - [Apache NIFI-6189](https://issues.apache.org/jira/browse/NIFI-6189)),
    the scale-down stays held until you intervene manually: either fix the node so it reconnects
    (then the operator retries automatically), or remove it from NiFi's own cluster membership
    through the NiFi UI/API (**Cluster** > **Nodes** > disconnect/offload/remove) so the next
    reconcile finds it `OFFLOADED` already.
  - Cleared automatically the moment a step succeeds again.

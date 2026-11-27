# NiFiCluster status

Besides replica counts, the operator reports **NiFi's own view of the cluster**, read from the
NiFi REST API (`GET /nifi-api/controller/cluster` and `/flow/about`) every
`nifiStatusInterval` (chart value, default `20s`; manager flag `--nifi-status-interval`).

```console
$ kubectl get nificlusters.nifi.ksolves.com -o wide
NAME   READY   REPLICAS   AGE   AUTH SECRET               VERSION   HEALTHY
prod   3       3          4d    prod-auth-credentials     2.12.0    True
```

| Field | Meaning |
|---|---|
| `status.nifiVersion` | NiFi version the cluster runs |
| `status.clusterCoordinator` | address of the node acting as Cluster Coordinator |
| `status.primaryNode` | address of the node acting as Primary Node |
| `status.nodes[]` | every member NiFi lists, keyed by `nodeId`: `nodeId`, `address`, `nodeGroup` (empty for a departed node awaiting removal), `state` (`CONNECTED`, `CONNECTING`, `DISCONNECTED`, `DISCONNECTING`, `OFFLOADING`, `OFFLOADED`), `roles`, `lastHeartbeat`, `activeThreads`, `queued`. A restarted node rejoins under a new `nodeId`; if NiFi still lists its old identity (disconnected) at the same address, the operator deletes that entry once the new one is connected |
| `status.lastNiFiPoll` | when `nodes` was last written |

`lastHeartbeat`, `activeThreads` and `queued` change on every poll; to keep status writes down
they are refreshed every 5 minutes, or immediately whenever a node's state or roles change.

## The `ClusterHealthy` condition

| Status | Reason | Meaning |
|---|---|---|
| `True` | `AllNodesConnected` | every node the spec asks for is `CONNECTED` |
| `False` | `NodesNotConnected` | the message lists the nodes that are not, with their state, or "not in the NiFi cluster" |
| `Unknown` | `NotReady` | no NiFi pod is ready yet, so NiFi is not asked |
| `Unknown` | `AuthFailed` | NiFi refused the operator's credentials (single-user login Secret wrong or missing) |
| `Unknown` | `NiFiAPIError` | NiFi answered with an error |
| `Unknown` | `NiFiUnreachable` | no node answered |

A node that dies shows up within about a minute: NiFi marks it `DISCONNECTED` after roughly 40 s
of missed heartbeats, and the next poll reports it. A failed poll never fails the reconcile; the
last known `nodes` stay in place and only the condition changes.

The operator authenticates the way the rest of it does: no login on plain-HTTP clusters, the
single-user login on TLS clusters ([Authentication](authentication.md)).

## Other conditions

The NiFiCluster carries more conditions than `ClusterHealthy`: `Available`, `Degraded`,
`Upgrading`, `CoordinationUpToDate`, `CoordinationRBACReady`,
`MembershipUpToDate`, and for exposure `ServiceReady`. [Troubleshooting](troubleshooting.md#reading-the-clusters-status) lists every
condition with its reasons.

## Pod probes

Each NiFi container has three probes:

| Probe | Checks | Timing |
|---|---|---|
| startup | NiFi's web server answers HTTP at all (`/nifi-api/flow/about`, any status code) | every 10s, up to 10 minutes: first boot plus NiFi's 5-minute flow election |
| liveness | the same | every 20s; the container restarts after about 2 minutes with no answer |
| readiness | this node is connected to the NiFi cluster (`/flow/cluster/summary`, `connectedToCluster`); on TLS it logs in with the cluster's credentials (cached token) | every 10s, after the startup probe passes |

Liveness deliberately ignores cluster membership: a node that is disconnected or waiting out a
flow election is not Ready, so it receives no traffic and blocks a rolling restart, but restarting
it would only start that wait over. Only a JVM or web server that stops answering is restarted.

**Rolling restarts.** Readiness makes a rolling restart replace one pod only after the previous
one has rejoined, so NiFi keeps at least N-1 nodes connected - except while the Cluster
Coordinator's own pod restarts. NiFi then has the remaining nodes reconnect to the newly elected
coordinator, and for a while reports them `CONNECTING` rather than `CONNECTED`, so `ClusterHealthy`
can briefly be `False`. The pods stay Ready throughout. How long varies widely from one rolling
restart to the next: the E2E suite has measured no such window at all in most runs, and windows of
5 seconds to about 2 minutes in others (3-node TLS clusters on OKD and kind). A rolling restart can
also hand the role over more than once: pods restart in reverse ordinal order, and the newly elected
coordinator may be a pod that has not restarted yet. It comes from NiFi's own coordinator handover,
not from how the pods are rolled, so the operator does not try to work around it. The E2E
suite checks what the operator controls - pods restart one at a time, at most N-1 such windows, and
every node connected again at the end - and logs each window's length.

# NiFiNodeGroup objects and autoscaling

The Community edition runs **one node group** per NiFiCluster, defined inline in
`spec.nodeGroups`. The CRD rejects a second inline node group, and the admission webhook rejects
new NiFiNodeGroup objects ("NiFiNodeGroup is not available in the Community edition"). With them
goes scaling a node group on its own through the `/scale` subresource: `kubectl scale`, a
HorizontalPodAutoscaler or KEDA.

To scale a cluster, change `replicas` of its node group (1 to 3); see [Scaling](scaling.md):

```sh
kubectl patch nificlusters.nifi.ksolves.com <name> --type json \
  -p '[{"op":"replace","path":"/spec/nodeGroups/0/replicas","value":3}]'
```

## Clusters from earlier versions

- **Several inline node groups:** the cluster keeps running, and edits that leave
  `spec.nodeGroups` unchanged still validate (CRD validation ratcheting, Kubernetes 1.30+). Any
  change to `spec.nodeGroups` must bring it down to one entry; the removed node groups are drained
  one node at a time.
- **NiFiNodeGroup objects:** the operator still runs each one as a node group of its cluster; it
  counts towards the 3-node limit, and it can still be updated, scaled
  (`kubectl scale nifinodegroups.nifi.ksolves.com <name> --replicas=<n>`) and deleted. Deleting
  one drains its nodes one at a time. `/scale` writes go straight to the API server, past the main
  NiFiCluster/NiFiNodeGroup webhooks - but a dedicated validating webhook on the `/scale`
  subresource itself checks the cluster-wide total (every node group's replicas, not just this
  one's) and refuses a scale that would push it past 3. If that webhook is unreachable or
  disabled, or the cluster was already over the limit before an upgrade added the check, the
  operator keeps reconciling everything else (ConfigMaps, Secrets, RBAC, a scale-down's own
  pacing) - it just never scales a StatefulSet up any further - and sets `Degraded=True`
  (`NodeLimitExceeded`) until the total is back within the limit.

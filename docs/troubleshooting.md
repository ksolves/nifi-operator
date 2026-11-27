# Troubleshooting

Each entry starts with the message or symptom you see, so you can search this page for it. Start
with [Reading the cluster's status](#reading-the-clusters-status) when you don't know where to look,
and [Collecting diagnostics](#collecting-diagnostics) before opening an issue.

## Reading the cluster's status

```sh
kubectl -n <ns> get nificlusters.nifi.ksolves.com <name> -o wide
kubectl -n <ns> describe nificlusters.nifi.ksolves.com <name>   # Conditions and Events at the end
```

The operator reports on the NiFiCluster with these conditions:

| Condition | Reasons | What it tells you |
|---|---|---|
| `Available` | `NodeGroupsReconciled` | `True` when all replicas are ready; the message counts them. |
| `ClusterHealthy` | `AllNodesConnected`, `NodesNotConnected`, `NotReady`, `AuthFailed`, `NiFiAPIError`, `NiFiUnreachable` | NiFi's own view: whether every node is connected to the NiFi cluster ([Status](status.md)). |
| `Degraded` | `AllNodeGroupsReconciled`, `NodeGroupReconcileFailed` | `True` when a node group could not be reconciled; the operator log and Events say why. |
| `Upgrading` | `RolloutInProgress`, `RolloutComplete` | A node group is rolling out a new image. |
| `CoordinationUpToDate` | `UpToDate`, `MigrationAwaitingApproval`, `MigrationStoppingNodes`, `ConflictingSensitivePropsKeys` | Whether all node groups run as one NiFi cluster on the configured backend. |
| `CoordinationRBACReady` | `Granted`, `CoordinationRBACMissing` | Whether the NiFi pods' ServiceAccounts may use the Leases and ConfigMaps of Kubernetes coordination. |
| `MembershipUpToDate` | `NoCredentials`, `NiFiAPIError` | Present only while departed nodes cannot be removed from the NiFi cluster. |
| `ServiceReady` | `ServiceNameConflict` | A Service of the cluster's name exists and is not the operator's. |

A NiFiNodeGroup object reports `Ready` (`NodesReady`, `NodesNotReady`, `Conflict`) and `Conflict`
(`InlineNodeGroupExists`, `ClusterNotFound`).

## Installing and starting

### `ImagePullBackOff` on the NiFi pods

The node can't pull the NiFi image. Check `spec.image` (`kubectl describe pod` shows the exact
image): a misspelt tag, or a private registry without credentials. For a private registry, create a
pull Secret in the cluster's namespace and reference it:

```yaml
spec:
  imagePullSecrets:
    - name: my-registry-credentials
```

### NiFi pod stays `Pending`

`kubectl describe pod <pod>` shows why. The usual causes:
- **`pod has unbound immediate PersistentVolumeClaims`**: there is no default StorageClass, or
  the StorageClass can't provision. Set `storageClassName` on the node group, or install a
  provisioner.
- **`Insufficient cpu` / `Insufficient memory`**: lower the node group's `resources.requests`, or
  add nodes. Each NiFi node needs about 2 GiB of memory.

### `violates PodSecurity "restricted"` / `"baseline"` in the operator namespace

The CSI driver mounts host directories, which only a `privileged` namespace allows:

```sh
kubectl label namespace nifi-operator pod-security.kubernetes.io/enforce=privileged --overwrite
```

The NiFi pods themselves pass `restricted`; if a NiFi pod is rejected, a `podOverrides`
security context, init container or sidecar takes it out of the profile, and the webhook warned
about it when you applied the cluster.

### `unable to validate against any security context constraint` (OpenShift)

The NiFi pods ask for a fixed UID or fsGroup that the namespace's `restricted-v2` SCC does not
allow. Install the operator with `nifiPods.fsGroup: ""` and `nifiPods.runAsUser: ""`
([Installation](getting-started/installation.md#openshift)) so OpenShift assigns both. With
`nifiPods.openshiftSCC: anyuid` also set `nifiPods.seccompProfile: ""`: `anyuid` allows no seccomp
profile.

### `bin/nifi.sh: Permission denied` in the NiFi container log

The stock `apache/nifi` image opens `lib/` and `bin/nifi.sh` only to its `nifi` group (GID 1000),
and the pod does not run in it. The operator always adds the supplemental group 1000; a custom image
that changes the group, or a security policy that strips supplemental groups, causes this. Keep the
image's layout or make the files readable to the UID the pod runs as.

### `keystore password was incorrect` in the NiFi log

The keystore the CSI driver wrote does not open with the cluster's keystore password, almost always
because the operator and the CSI driver come from different releases. Install both from the same
chart (`helm upgrade` updates both), then delete the NiFi pods so they get new keystores.

### `FATAL: running as an unrecognized UID and /etc/passwd is not writable`

The `nifi-init` init container gives the pod's UID a `/etc/passwd` entry. This message means it did
not run, usually because a `podOverrides` change removed or replaced the operator's volumes. Check
`kubectl describe pod` for the `nifi-init` container and the `nifi-passwd` volume.

### `ERROR: failed to apply single-user credentials from the authentication Secret`

The login password is shorter than NiFi's minimum of 12 characters. The webhook rejects short
passwords when you apply the cluster, but a referenced Secret can be edited afterwards. Set a
password of 12 characters or more (at most 72 bytes) in the Secret.

## Applying a NiFiCluster

The admission webhook and the CRD's validation rules reject invalid changes before anything runs.
The messages name the field.

### `this environment variable is set by the operator and cannot be overridden`

`podOverrides.env` sets a variable the operator sets itself: `POD_NAME`, `POD_NAMESPACE`,
`NIFI_CLUSTER_DOMAIN`, `NIFI_SENSITIVE_PROPS_KEY`, `NIFI_AUTH_USERNAME`, `NIFI_AUTH_PASSWORD`,
`NIFI_AUTH_PASSWORD_HASH` or `NIFI_KEYSTORE_PASSWORD`. Use another name. The same rule exists for
volumes (`this volume name is managed by the operator and cannot be reused`) and for the container
names `nifi` and `nifi-init`.

### `you are not allowed to create pods (the image repository is not one the operator trusts) in namespace "<ns>", so the operator will not do it for you`

`spec.image.repository` is not in the operator's trusted list (`webhook.trustedImageRepositories`,
by default `apache/nifi`), and you may not create pods in the namespace yourself. Ask an admin to
add the repository to the list, or to apply the change.

### `NiFi <version> is too old for Kubernetes coordination`

Kubernetes coordination, the only coordination the Community edition supports, needs NiFi 2.11.0
or later. Use a 2.11 or newer image ([Compatibility](compatibility.md)).

### `storage size changes are not supported in the Community edition`

The API server rejects a changed `storage`, `logStorage` or split repository size before the
admission webhook runs (the webhook's own message is `cannot change from <old> to <new>`). To grow
the volumes, expand the PVCs with `kubectl patch pvc` ([Storage](storage.md#volume-sizes-are-fixed)).
To use smaller disks, create a new node group and remove the old one.

### `spec.coordination.backend is immutable once set and cannot be removed`

The coordination backend is fixed at creation. To change it, create a new cluster and move the flow
to it.

### `must be at least 12 characters (NiFi single-user login minimum)`

The single-user password is too short. NiFi's minimum is 12 characters; the maximum is 72 bytes
(`must be at most 72 bytes`).

## Using NiFi

### `The request contained an invalid host header` or HTTP 421

NiFi accepts requests only for host names it knows. Add the address the browser uses, with its port
if it is not 443, to `spec.proxyHosts`:

```yaml
spec:
  proxyHosts:
    - nifi.example.com
    - localhost:8443
```

### `kubectl port-forward`: `failed to connect to localhost:8443 inside namespace ... connection refused`

NiFi listens on its pod address, not on the loopback address a port-forward connects to, so
port-forwarding to NiFi does not work. Expose the cluster Service instead (`spec.serviceType:
LoadBalancer`, or your own NodePort Service, OpenShift Route, Ingress or HTTPRoute) and add the address to
`spec.proxyHosts` ([First steps](getting-started/first-steps.md#4-open-the-ui)).

### Login fails right after the cluster starts

NiFi needs a minute after the pod is `Ready` before the login works on every node. Read the current
password again: it is in the Secret named in the `AUTH SECRET` column of
`kubectl get nificlusters.nifi.ksolves.com -o wide` (key `password`), and it changes when the
NiFiCluster is deleted and recreated.

### A UI or API call returns HTTP 409

While a node is still joining the cluster (after a pod restart, a rollout or a scale-up), NiFi
refuses requests that must be replicated to every node and answers `409 Conflict` (for example
"... is not connected", or the cluster is still being formed). This is NiFi behaviour, not an
operator fault, and it clears once every node is CONNECTED: wait until
`kubectl get nificlusters.nifi.ksolves.com <name>` shows READY equal to REPLICAS (or every
`status.nodes` entry is CONNECTED and `ClusterHealthy=True`), then reload the UI or retry the call.
The operator's own calls already retry a 409 on their own.

### `ClusterHealthy` is `False` with `NodesNotConnected`

The message lists the nodes that are not connected and their state. A node that just restarted
reconnects by itself within a few minutes. If one stays disconnected, its NiFi log says why
(`kubectl logs <pod> -c nifi`).

### `ClusterHealthy` is `Unknown` with `AuthFailed`

`The NiFi REST API refused the operator's credentials`: the operator logs in with the cluster's
login Secret. Someone changed the password in NiFi, or the Secret no longer matches. Restore the
Secret's password, or set a new one in the Secret; the pods roll and pick it up.

## Scaling, upgrades and maintenance

### Scale-down does not finish (`nodeRemovalPending: true`)

Before removing a node, the operator waits until every remaining node is ready and the departing
node is offloaded and gone from the NiFi cluster. `MembershipUpToDate` says what blocks it
(`NoCredentials`, `NiFiAPIError`). A large queue takes time to offload; the
`OffloadMayExceedGracePeriod` event warns when it may not fit in `terminationGracePeriodSeconds`.

### `CoordinationUpToDate` is `False` with `MigrationAwaitingApproval`

The cluster's StatefulSets were built for another coordination backend, and moving them stops every
NiFi node. The message contains the command that approves it:

```sh
kubectl annotate nificlusters.nifi.ksolves.com <name> nifi.ksolves.com/approve-coordination-migration=<Kubernetes|ZooKeeper>
```

### `CoordinationRBACReady` is `False` with `CoordinationRBACMissing`

A node group runs as its own ServiceAccount, which may not use the Leases and ConfigMaps Kubernetes
coordination needs. The message contains the `kubectl create rolebinding` command that grants it.

### A Kubernetes node is lost

A node that stops answering is `NotReady`; after about 5 minutes Kubernetes marks its pods for
deletion, but a StatefulSet pod stays `Terminating` until the node is confirmed gone, so its
replacement is not started. When you are sure the node is down for good:

```sh
# Kubernetes 1.28+: its pods are deleted and their volumes detached
kubectl taint node <node> node.kubernetes.io/out-of-service=nodeshutdown:NoExecute
# or, for one pod
kubectl -n <ns> delete pod <pod> --grace-period=0 --force
```

Network-attached volumes then reattach on another node (in the same zone for zonal disks). A pod
whose volumes were node-local, or in a zone that is gone, stays `Pending` (volume node affinity)
until they are back. To run it elsewhere now, accepting the loss of the FlowFiles queued on it,
delete its PVCs and the pod; the StatefulSet recreates them empty, and the node rejoins and
receives the flow from the others:

```sh
kubectl -n <ns> delete pvc data-<pod> logs-<pod> --wait=false
kubectl -n <ns> delete pod <pod>
```

### Changes to the NiFiCluster have no effect

Check `Degraded` and the Events for a reconcile error. To keep the operator from reverting a manual
change for a while, scale the operator Deployment to zero and back (this stops it for every
cluster it manages); the annotation `nifi.ksolves.com/reconcile-paused` of earlier versions is
ignored.

## Collecting diagnostics

```sh
kubectl -n <ns> get nificlusters.nifi.ksolves.com <name> -o yaml
kubectl -n <ns> describe nificlusters.nifi.ksolves.com <name>
kubectl -n <ns> get pods,pvc,svc,statefulsets -l app.kubernetes.io/instance=<name>
kubectl -n <ns> get events --sort-by=.lastTimestamp
kubectl -n <ns> logs <name>-<nodegroup>-0 -c nifi --tail=500
kubectl -n <ns> logs <name>-<nodegroup>-0 -c nifi-init
kubectl -n nifi-operator logs deploy/nifi-operator-controller-manager --tail=500
kubectl -n nifi-operator logs daemonset/nifi-operator-csi-driver --all-containers --tail=200
```

Remove passwords and other secrets before you attach the output to an issue.

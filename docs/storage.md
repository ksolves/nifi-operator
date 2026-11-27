# Storage and retention

Each NiFi node keeps its repositories (FlowFiles, content, provenance, the flow definition) and
its logs on its own PersistentVolumeClaims, created from the StatefulSet's volume claim templates:
`data` and `logs`, plus one claim per repository with `splitRepositoryStorage`.

## PVC retention policy

`persistentVolumeClaimRetentionPolicy` decides what happens to those PVCs. It mirrors
the StatefulSet field of the same name:

| Field | Applies when | `Retain` (default) | `Delete` |
|---|---|---|---|
| `whenDeleted` | the NiFiCluster is deleted, or a node group is removed (inline group dropped from `spec.nodeGroups`, or its NiFiNodeGroup deleted) | PVCs stay | PVCs are deleted with the StatefulSet |
| `whenScaled` | a scale-down removes nodes (after the operator has offloaded them, see [Scaling](scaling.md)) | PVCs of the removed nodes stay; a later scale-up reuses them | they are deleted; a later scale-up starts those nodes empty |

Set it on the cluster as the default, and override it per node group, field by field. An unset
field falls back to the cluster's value, then to `Retain`:

```yaml
spec:
  persistentVolumeClaimRetentionPolicy:   # cluster default
    whenDeleted: Delete
  nodeGroups:
    - name: ingest
      replicas: 2
      storage: 100Gi
      persistentVolumeClaimRetentionPolicy:
        whenDeleted: Retain               # keep this group's data; whenScaled inherits (Retain)
    - name: scratch
      replicas: 1
      storage: 10Gi                       # inherits whenDeleted: Delete
      persistentVolumeClaimRetentionPolicy:
        whenScaled: Delete
```

A NiFiNodeGroup takes the same field in its spec, and inherits the cluster's default the same way.

The policy can be changed at any time. The StatefulSets are updated in place and no pod restarts.
The webhook warns, naming the node group, whenever a policy resolves to `Delete`: data on a
deleted PVC can't be recovered.

Deleting a cluster applies the policy the StatefulSets carry.

## What a deleted cluster leaves behind

The flow on the PVCs needs two other things to be usable again: the **sensitive-props key**
(Secret `<name>-sensitive-props`), which encrypts every sensitive property and parameter, and
NiFi's **component state** (ConfigMaps `<name>-nifi-component-<id>`, with Kubernetes
coordination). Both follow the PVCs:

| The cluster's PVCs | Sensitive-props key | Component state |
|---|---|---|
| any PVC stays (a group with `Retain`, the default) | kept, released from the cluster | kept |
| every PVC is deleted with the cluster | deleted with the cluster | deleted |

The operator decides from the PVCs themselves, not from the spec: Kubernetes marks a PVC it will
delete with its StatefulSet or Pod as owner, and any PVC of the cluster without such an owner
counts as staying. That covers a paused cluster whose StatefulSets still carry an older policy, a
NiFiNodeGroup deleted together with the cluster, and the retained PVCs of a node group removed
earlier. PVCs are recognised by their labels; claims made before operator 0.6.0 carry none and
count only while their node group is still in the spec.

A NiFiCluster recreated **with the same name** adopts the kept key, but only while that cluster's
PVCs are still there; a key with nothing left to decrypt is never adopted, so a planted Secret
can't take over a brand-new cluster. The recreated nodes mount their old PVCs, decrypt the flow and
resume where they stopped, including processor state.

## Starting from scratch after a Retain delete

To discard everything a deleted cluster `<name>` kept, delete its PVCs, key and component state:

```sh
kubectl -n <ns> delete pvc -l app.kubernetes.io/name=nifi,app.kubernetes.io/instance=<name>
kubectl -n <ns> delete secret <name>-sensitive-props
kubectl -n <ns> get configmap -o name | grep "^configmap/<name>-nifi-component-" | xargs -r kubectl -n <ns> delete
```

Deleting only the key while keeping the PVCs leaves a flow whose sensitive values nobody can
decrypt: a recreated cluster generates a new key, and NiFi can't load the old flow with it.

## Repository tuning

`repositories` sets the most common repository limits as typed fields, instead of raw
`configOverrides` keys:

| Field | nifi.properties key | Format |
|---|---|---|
| `contentArchive.enabled` | `nifi.content.repository.archive.enabled` | `true` / `false` |
| `contentArchive.maxRetentionPeriod` | `nifi.content.repository.archive.max.retention.period` | NiFi time, e.g. `12 hours` |
| `contentArchive.maxUsagePercentage` | `nifi.content.repository.archive.max.usage.percentage` | 1-100 |
| `provenance.maxStorageTime` | `nifi.provenance.repository.max.storage.time` | NiFi time, e.g. `30 days` |
| `provenance.maxStorageSize` | `nifi.provenance.repository.max.storage.size` | Kubernetes quantity, e.g. `10Gi` (rendered `10 GB`) |
| `flowFile.checkpointInterval` | `nifi.flowfile.repository.checkpoint.interval` | NiFi time, e.g. `20 secs` |

NiFi time is a number and a unit: `ms`, `s`/`sec`/`secs`, `m`/`min`/`mins`, `h`/`hr`/`hours`,
`d`/`days`, `w`/`weeks`. Anything else is rejected when applied.

Like the retention policy, set it on the cluster and override it per node group, field by field.
Unset fields keep NiFi's defaults.

```yaml
spec:
  repositories:
    provenance:
      maxStorageTime: 30 days     # compliance: keep 30 days of provenance
      maxStorageSize: 50Gi
    contentArchive:
      maxUsagePercentage: 40      # small disks: cap the archive
  nodeGroups:
    - name: edge
      repositories:
        contentArchive:
          enabled: false          # this group archives nothing
```

- A key set through `repositories`, on the cluster or on any node group, can't also be set in
  `configOverrides`: the webhook rejects it, naming the field to use.
- A change rolls the pods, like every `nifi.properties` change.
- The webhook warns when `provenance.maxStorageSize` is larger than the volume holding the
  provenance repository (`splitRepositoryStorage.provenanceRepositoryStorage`, otherwise `storage`),
  and when `contentArchive.maxUsagePercentage` is 90 or more.

## Volume sizes are fixed

`storage`, `logStorage` and the `splitRepositoryStorage` sizes of an existing node group can't
change: the API server and the webhook reject a larger or a smaller value ("storage size changes
are not supported in the Community edition"). The same size written another way (`2Gi` and
`2048Mi`) is no change. A new node group may pick any size.

To give a running node group more room, expand its PVCs yourself, one per node, on a StorageClass
with `allowVolumeExpansion: true`:

```sh
kubectl get pvc -n <ns> -l app.kubernetes.io/instance=<name>
kubectl patch pvc data-<name>-<group>-0 -n <ns> \
  -p '{"spec":{"resources":{"requests":{"storage":"300Gi"}}}}'
kubectl get pvc data-<name>-<group>-0 -n <ns> -o jsonpath='{.status.capacity.storage}'
```

- Some drivers grow the file system only when the pod restarts (the PVC shows the condition
  `FileSystemResizePending`): delete the pod, one node at a time.
- The StatefulSet's `volumeClaimTemplates` keep the original size, so a node added later by a
  scale-up gets a volume of the size in the spec. Expand its PVC too.
- `local-path` can't expand: use an LVM-based local provisioner (see
  [Local volumes](local-storage.md)) or a network volume driver that supports it.

The webhook also rejects **changing `storageClassName`**, or turning `splitRepositoryStorage` on or
off: existing PVCs can't move. Add a new node group instead.

NFS and CIFS storage classes are rejected.

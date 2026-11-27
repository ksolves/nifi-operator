# Local volumes (NVMe, local PVs)

NiFi's content repository writes every byte that flows through it, so its disk is often what limits
throughput. Local disks (NVMe or SSD on the node itself) are much faster than network-attached
storage, but a local volume is tied to one node. This page covers when that trade is worth it and
how to set a cluster up for it.

## When to use local volumes

| | Local volumes | Network-attached volumes |
|---|---|---|
| Throughput and latency | Best: the node's own disk | Limited by the network and the storage backend |
| Node drained (maintenance) | The pod **stays Pending** until the node is back | The pod moves to another node |
| Node lost for good | That node's queued FlowFiles are **lost**; the node rejoins empty | The pod and its data move to another node |
| Resize (`kubectl patch pvc`) | Only with an expandable provisioner (LVM based) | Usually supported |

Use local volumes when throughput matters more than surviving the permanent loss of a node's
queued data. A typical example is high-rate ingestion, where the source can replay or the data is
quickly passed on. Keep network storage when every queued FlowFile must survive a node failure.

## Provisioners

- **local-path** (`rancher.io/local-path`): a directory on the node's root disk. It is simple, but
  it **enforces no size**: a volume can fill the node's whole disk. It also can't resize. Fine for
  development and benchmarks.
- **LVM based** ([TopoLVM](https://github.com/topolvm/topolvm),
  [OpenEBS LVM LocalPV](https://github.com/openebs/lvm-localpv)): a logical volume of exactly the
  requested size, carved from a volume group on a dedicated disk, which **can be resized** online
  (`kubectl patch pvc`).
  This is the recommended choice for production on local NVMe.
- **Static local PVs** (`kubernetes.io/no-provisioner`): you create one PV per disk by hand.
  There's no resizing and no automation; use it only if nothing else is allowed.

Whatever the provisioner, the StorageClass must use `volumeBindingMode: WaitForFirstConsumer`, so a
volume is created on the node where its pod is first scheduled.

## Cluster setup

`config/samples/nifi-local-storage.yaml` puts it together:

- **Required pod anti-affinity**, so two nodes of a group never share a host. By default the
  operator only *prefers* spreading, which is fine for network storage. With local volumes,
  though, two NiFi nodes on one host means losing that host loses both. Set
  `podOverrides.affinity` (it replaces the default).
- **A PodDisruptionBudget** of `replicas - 1` (the default), so a drain takes one node at a time.
  The drained node's pod stays Pending until the node is uncordoned, so plan drains as short
  maintenance windows.
- **`persistentVolumeClaimRetentionPolicy.whenScaled: Delete`** (see [Storage](storage.md)): a
  scaled-down node's local volume is released instead of pinning a later scale-up to that host.
- **`splitRepositoryStorage`** to size the content repository on its own. All of a node group's
  volumes use the node group's `storageClassName`; there is no per-repository class. To put only
  the content repository on local disk, the whole node group goes on the local class.
- **Content archive limits** (`repositories.contentArchive`, see [Storage](storage.md)): on
  local-path, which enforces no size, the archive is bounded only by `maxUsagePercentage` of the
  node's disk.

## When a node is lost for good

Its pod stays Pending (volume node affinity). To bring the NiFi node back on another host with
empty repositories, accepting the loss of its queued FlowFiles, delete its PVCs and the pod, as
described in [Troubleshooting](troubleshooting.md#a-kubernetes-node-is-lost). The node rejoins the cluster and receives
the flow from the others.

## Benchmark

`hack/bench-content-repo.sh <namespace> <cluster> [disk|nifi|all] [seconds]` measures node 0 of a
test cluster:

- **disk:** sequential 1 GiB write and read with `dd` and direct I/O on the content repository's
  own directory;
- **nifi:** NiFi itself, with GenerateFlowFile (1 MiB unique FlowFiles) feeding an auto-terminating
  UpdateAttribute, reported as content MiB/s. HTTP clusters only. The script creates a process
  group `bench-content-repo` and always removes it again.

Run it on a test cluster: it writes gigabytes. On local-path that space comes from the node's own
disk, so set `repositories.contentArchive.enabled: false` (or a low `maxUsagePercentage`) for the
benchmark cluster.

Measured results:

| Environment | Volume | dd write | dd read | NiFi content write |
|---|---|---|---|---|
| kind on a laptop (16 cores), Samsung PM9A1 NVMe | local-path | 569 MB/s | 2.6 GB/s | 284 MiB/s over 20 s, 134 MiB/s over 60 s (1 node, 2 CPU limit) |
| OKD 4.19 (VM worker, 120G disk) | local-path | 1.1 GB/s | 1.6 GB/s | 560 MiB/s over 20 s (1 node, 2 CPU limit) |

These measure one setup each, not the hardware's limits. NiFi's figure is bounded by the node's
CPU limit and by the archive work during a longer run as much as by the disk. Compare
configurations on your own hardware, using the same script.

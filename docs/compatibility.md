# Supported NiFi versions

The operator runs the stock Apache NiFi images (`apache/nifi:<version>`) from Docker Hub, NiFi
2.11.0 and later. The Community edition coordinates clusters through Kubernetes only, which needs
NiFi 2.11; older NiFi needs external ZooKeeper, which it does not support.

| NiFi | Coordination backend | Tested with the stock image |
|---|---|---|
| **2.11, 2.12** | Kubernetes | yes |
| 2.10 and older | not supported (needs ZooKeeper) | |

Clusters created on ZooKeeper by earlier operator versions keep running on it, with the older NiFi
versions they use (1.14 and later); see [Cluster coordination](cluster-coordination.md#clusters-on-external-zookeeper).

The default image is `apache/nifi:2.12.0`. "Tested" means a cluster of that version formed and
answered on kind with this release. Every stock image from 2.0 on has the same layout (user
`nifi`, `NIFI_HOME=/opt/nifi/nifi-current`), so the untested 2.x lines are expected to work too.

## How the operator runs a stock image

The operator does not run the image's own `start.sh`, which would rewrite `nifi.properties` from
environment variables. It ships its own entrypoint in each node group's ConfigMap and runs it as
the container's command. The pods run as UID 1000 (the image's `nifi` user) in group 0, unless the
platform assigns the UID itself (chart value `nifiPods.runAsUser: ""`, see
[installation.md](installation.md#pod-security)), and with the supplemental group 1000: the image
opens `lib/` and `bin/nifi.sh` only to that group. Tested on OpenShift's `restricted-v2` SCC
with an assigned UID.

An image built on `apache/nifi` (extra NARs, drivers) works the same way, as long as it keeps
`NIFI_HOME` at `/opt/nifi/nifi-current` and the tools the entrypoint, the init container and the
probes use: a POSIX `sh`, `curl`, coreutils, `awk`, `sed`, `grep` and `find`.

## Why these limits

- **Kubernetes coordination needs NiFi 2.11.0.** NiFi 1.x has no Kubernetes coordination at all,
  and releases before 2.11 read the Lease on every scheduling decision
  ([NIFI-16120](https://issues.apache.org/jira/browse/NIFI-16120)); see
  [cluster-coordination.md](cluster-coordination.md).
- **External ZooKeeper is not available in the Community edition**, so NiFi before 2.11 cannot be
  started on a new cluster.

## What the webhook enforces

The NiFi version is read from `spec.image.tag` (the first `X.Y.Z` in it). A version the cluster's
settings do not support is rejected, with a message naming the minimum. A tag without a version
(`latest`, a custom name) is only warned about: the operator cannot check it. On update the check
runs again when the image or the coordination backend changes.

## Older NiFi on existing ZooKeeper clusters

A cluster created on ZooKeeper by an earlier operator version keeps running, and the operator
still handles older NiFi for it:

- **NiFi before 2.7.0** still reads `nifi.cluster.protocol.is.secure` and defaults it to `false`.
  The operator sets it to match `tls.enabled`, so a TLS cluster's node-to-node protocol is TLS
  (also for a tag without a version on ZooKeeper). 2.7.0 removed the property (NIFI-15140).
- **NiFi 1.x** loads its flow from `flow.xml.gz`, and from 1.16 also writes `flow.json.gz`. The
  operator keeps both on the data volume, so the flow survives a restart.

## Moving between versions

Change `spec.image.tag`. Nodes roll one at a time, so for a while the cluster runs both versions.

- Within a major line (1.x to 1.y, 2.x to 2.y) NiFi's own release notes apply. Try it on a copy of
  the cluster first.
- **NiFi 1.x to 2.x is not a rolling upgrade.** NiFi 2 removed components and changed the flow
  format, and nodes of the two major versions do not form one cluster. Create a new 2.x cluster
  and move the flow to it.

## Building a custom node image (optional)

The stock images need nothing extra. `images/nifi` builds a node image on top of them that also
takes the latest Debian security fixes at build time, bakes in the same entrypoint and runs as
`1000:0`. `make docker-build-nifi` builds it for 2.12.0 (digest-pinned in `images/nifi/Dockerfile`);
`make docker-build-nifi-version NIFI_OTHER=2.8.0` builds one other line from the same Dockerfile.
`make release-push-nifi-versions` builds, pushes and checks every line in `NIFI_OTHER_VERSIONS`, one
at a time, and removes each image locally once it is pushed. None of this is part of a release.

The custom image of the default line takes the latest Debian security fixes at build time. Older NiFi lines sit on
Debian releases that no longer get updates (buster, and bullseye after its LTS ended), so their
images keep the base packages as Apache released them (`OS_UPGRADE=best-effort`). Expect more OS
CVEs in those images, and prefer a current line where you can.

Where `deb.debian.org` is slow, point the build at
another Debian mirror: `make release-push-nifi-versions DEBIAN_MIRROR=http://ftp.de.debian.org/debian`.
The default image (`make docker-build-nifi`) always builds from deb.debian.org.

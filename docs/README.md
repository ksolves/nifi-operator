# Ksolves Operator for Apache NiFi: documentation

The Ksolves Operator for Apache NiFi runs [Apache NiFi](https://nifi.apache.org) clusters on Kubernetes and OpenShift from
a `NiFiCluster` resource: node groups as StatefulSets, cluster coordination (Kubernetes Leases),
TLS from a per-cluster CA, logins, exposure and clean scale-down. NiFi runs from the
stock `apache/nifi` images.

## Getting started

1. [Installation](getting-started/installation.md): install the operator with Helm.
2. [First steps](getting-started/first-steps.md): create a NiFi cluster, log in, scale it.

On a laptop: [kind](getting-started/kind.md) prepares a local cluster and walks through the same
steps, with a few things to try.

## Usage guide

- [Architecture](architecture.md): how the operator, admission webhooks, TLS CSI driver and
  NiFi pods fit together, and the order of a reconcile.
- [Installation reference](installation.md): every chart value and operator flag, the admission
  webhook, monitoring and OpenShift.
- [Status](status.md): NiFi's view of the cluster in `status` - nodes, coordinator,
  primary node, version - and the `ClusterHealthy` condition.
- [TLS](tls.md): the operator CA, CSI-issued node certificates, SANs and rotation.
- [Compatibility](compatibility.md): the NiFi versions the operator runs, and what each needs.
- [Cluster coordination](cluster-coordination.md): one NiFi cluster across node groups,
  Kubernetes Lease/ConfigMap coordination, RBAC the operator creates, and migrating clusters
  from older operator versions.
- [Scaling](scaling.md): clean scale-down (decommission and offload) and removal of departed
  nodes from the NiFi cluster.
- [NiFiNodeGroup objects](autoscaling.md): one node group per cluster, and what happens to
  NiFiNodeGroups and extra node groups of earlier versions.
- [Storage and retention](storage.md): PVC retention on delete and scale-down, per cluster
  or node group, and what a deleted cluster leaves behind (sensitive-props key, component state).
- [Local volumes](local-storage.md): running the content repository on local NVMe/SSD -
  trade-offs, provisioners, required anti-affinity, node loss, and a throughput benchmark.
- [Exposing a cluster](exposure.md): the cluster Service, pointing your own Route, Ingress or
  HTTPRoute at it, session affinity, and certificate hostnames.
- [NiFi login credentials](authentication.md): what Secret backs the login by default when
  nothing is configured, literal credentials, and bringing your own Secret.
- [Security](security.md): hardening checklist, pod security, admission checks, TLS, a sample
  NetworkPolicy, known limitations, and every RBAC rule the operator and CSI driver hold.
- [Security response process](security-response.md): how a reported vulnerability is triaged,
  fixed, backported and disclosed.
- [CSI driver platform notes](csi-driver-platform-notes.md): what the TLS CSI driver needs
  from Pod Security Admission and OpenShift SCCs.

## Reference

- [CRD reference](crd-reference.md): the `NiFiCluster` and `NiFiNodeGroup` resources at a glance -
  printer columns, subresources, examples, field groups, reserved names and annotations.
- [API reference](api-reference.md): every field with its type, default and validation (generated
  by `make api-docs`).
- [Installation reference](installation.md#install): chart values and the operator flags they set.
- [Verifying the images](verify-images.md): digests, build labels, SBOMs and scans.

## Troubleshooting

- [Troubleshooting](troubleshooting.md): conditions, error messages and how to collect diagnostics.

## Project

- [Changelog](../CHANGELOG.md), [Contributing](../CONTRIBUTING.md), [Security policy](../SECURITY.md).

---

Apache NiFi, NiFi, Apache and the Apache feather logo are trademarks of The Apache Software
Foundation. This project is not affiliated with or endorsed by The Apache Software Foundation.

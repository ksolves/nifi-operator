# Changelog

All notable changes to the Ksolves Operator for Apache NiFi. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - Unreleased

First public release: the Community edition. Images: `ksoperator/nifi-operator` on Docker Hub
(operator and CSI driver); NiFi runs from the stock `apache/nifi` images, `apache/nifi:2.12.0` by
default.

### What the operator does
- `NiFiCluster` (`nifi.ksolves.com/v1alpha1`): one NiFi cluster, run as a StatefulSet.
- Cluster coordination with Kubernetes Leases and ConfigMaps; no ZooKeeper to run.
- TLS from a per-cluster CA, issued to each pod by the bundled CSI driver and rotated by it; a
  random keystore password per cluster, or your own (`tls.keystorePasswordSecretName`).
- Login: a generated single-user login, or your own single-user Secret.
- Exposure: a `ClusterIP` or `LoadBalancer` Service. For a Route, Ingress or HTTPRoute, create
  your own pointed at the cluster Service and list its hostname in `spec.proxyHosts`
  ([Exposing a cluster](docs/exposure.md)).
- Safe scale-down: a node is offloaded, then removed from the NiFi cluster, before its pod goes.
- Storage: one volume or split repositories, with a configurable PVC retention policy.
- Configuration overrides for `nifi.properties`, `bootstrap.conf`, `logback.xml` and
  `authorizers.xml`; status conditions and events.
- Pods meet the Kubernetes `restricted` Pod Security Standard and OpenShift's `restricted-v2` SCC.
- A validating and defaulting admission webhook, which also checks that the person making a
  change may do what the operator will do for them.
- Two operator replicas with leader election by default; metrics and an optional ServiceMonitor.
- An optional NetworkPolicy for the operator pods (`networkPolicy.enabled`).

### Limits of the Community edition
- One node group per NiFiCluster, of at most 3 NiFi nodes. New `NiFiNodeGroup` objects are
  rejected.
- Single-user login only.
- `serviceType` is `ClusterIP` or `LoadBalancer`; `LoadBalancer` requires TLS.
- TLS is on by default: the webhook adds `tls.enabled: true` to every new node group without a
  `tls` block. It needs the CSI driver (`csiDriver.enabled`, on by default).
- Coordination through Kubernetes Leases only, which needs NiFi 2.11 or later
  ([Compatibility](docs/compatibility.md)).
- Volume sizes are fixed once a node group is created; expand the PVCs with `kubectl patch pvc`
  ([Storage](docs/storage.md#volume-sizes-are-fixed)).
- One cluster-wide operator per Kubernetes cluster; no namespace-scoped install.
- No alert rules ship with the chart; [Monitoring](docs/installation.md#monitoring) lists the
  metrics to alert on.

### Security
- Built with Go 1.27.4, which fixes GO-2026-6859 (an HTTP/1 server crash in `net/http`,
  reachable from the operator).
- The operator holds `use` on no OpenShift SCC by default. The chart grants `use` only on the
  SCC named in `nifiPods.openshiftSCC`; with the value empty (the default) the operator can use
  no SCC. Installs from `config/` (kustomize) that pass `--nifi-scc` need that rule added to the
  operator's role.
- Setting or changing `tls.keystorePasswordSecretName` needs `get` on that Secret, like the other
  Secret references: the operator copies its password into the cluster's CA Secret.
- The TLS keystore password never appears in plain text in the node group's ConfigMap. Pods get it
  as the `NIFI_KEYSTORE_PASSWORD` env var through a secretKeyRef to the CA Secret.
  `NIFI_KEYSTORE_PASSWORD` is a reserved `podOverrides.env` name.
- The CSI sidecars are pinned by digest (node-driver-registrar v2.18.0), and `make release-scan`
  reports their CVEs. The few HIGH findings left in them are listed in
  [Verifying the images](docs/verify-images.md).
- `helm uninstall` keeps the CRDs, so it never deletes NiFiClusters
  ([Uninstall](docs/installation.md#uninstall)).
- [Security](docs/security.md): a hardening checklist, sample NetworkPolicies for the NiFi pods,
  the known limitations, and every permission the operator holds and why.

### Clusters from pre-release builds
Pre-release builds had OIDC login, Route and Gateway API exposure, NodePort Services and extra
ports, online volume expansion, pausing reconciliation, several node groups and NiFiNodeGroups,
ZooKeeper coordination, namespace-scoped installs and the chart's alert rules. Their clusters
keep running; see [Clusters from pre-release builds](docs/installation.md#clusters-from-pre-release-builds)
before upgrading.

### Upgrading to this build
- Every existing NiFiCluster's pods restart once: the entrypoint Secret now also carries
  `bootstrap.conf`/`logback.xml`/`authorizers.xml` overrides (previously the node group's
  ConfigMap), so the pod template's volumes change.
- Scaling a node group down now needs the operator to reach the NiFi REST API with working
  credentials to confirm each removed node has offloaded its data first; if it can't, the
  scale-down holds (`ScaleDownBlocked` condition) instead of risking data loss - see
  [Scaling](docs/scaling.md#status).

## Pre-release builds

The 0.13.x evaluation builds and the builds that followed them, before the Community edition.
What they had that 1.0.0 does not (ZooKeeper, NiFiNodeGroups, pausing, namespace-scoped installs
and more) is covered in
[Clusters from pre-release builds](docs/installation.md#clusters-from-pre-release-builds).

### Changed since 0.13.1
- NiFi runs from the stock `apache/nifi` images; no custom node image is needed. The operator
  ships its entrypoint in each node group's ConfigMap. New flag `--nifi-run-as-user` (chart
  `nifiPods.runAsUser`, default 1000); pods get the supplemental group 1000.
- The default NiFi image is `apache/nifi:2.12.0`.
- Operator and CSI images moved to the public `ksoperator/nifi-operator` repository.
- NiFi 2.x before 2.11 runs with ZooKeeper coordination; 2.8 to 2.12 are tested with the stock
  images.
- New flag `--nifi-seccomp-profile` (chart `nifiPods.seccompProfile`), for SCCs such as `anyuid`
  that forbid a seccomp profile (#109 backport).
- Each TLS cluster gets its own random keystore password instead of a shared one; optional
  `tls.keystorePasswordSecretName` (#134).

### Fixed since 0.13.1
- The CSI node-driver-registrar restarts when it loses its kubelet registration (#109 backport).
- NiFiNodeGroup admission errors point at the NiFiNodeGroup's own fields; CRD field docs
  corrected (#108 backport).
- Resource requests and limits defaulting, the status of a paused cluster, and deleting a paused
  cluster (#130, #132).
- Go 1.27.3: no reachable Go standard library vulnerabilities (#130).
- Namespace-scoped installs (`rbac.clusterWide: false`) never became Ready: the readiness check
  listed NiFiClusters in every namespace, which the namespaced Role forbids, so the webhook had no
  endpoints and rejected every NiFiCluster change. It now lists only in the watch namespace.

### Upgrading from 0.13.x
Every NiFi pod restarts once. Running clusters keep their `spec.image`. See
[Upgrading](docs/installation.md#upgrading-from-a-release-with-custom-node-images-013x).

[1.0.0]: https://github.com/ksolves/nifi-operator/releases/tag/v1.0.0

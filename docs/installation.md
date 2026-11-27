# Installing the operator

The operator is installed with the Helm chart in `charts/nifi-operator`. It deploys the CRD, the
controller-manager, its RBAC, the admission webhooks and the TLS CSI driver.

## Prerequisites

- Kubernetes 1.32 or newer (the chart is validated against the 1.32 schemas), or OpenShift/OKD 4.19.
- Helm 3 or newer, and cluster-admin rights: the chart creates cluster-scoped objects (the CRD,
  webhook configurations, the `CSIDriver` and the CSI driver's ClusterRole) in every mode.
- Optional: [cert-manager](https://cert-manager.io) for the webhook certificate (see
  [Webhook certificate](#webhook-certificate)).
- A NiFi node image built from `images/nifi`, for the `NiFiCluster`s themselves (`spec.image`):
  NiFi 2.11 or newer (see [Compatibility](compatibility.md)).

## Install

```sh
helm install nifi-operator charts/nifi-operator -n nifi-operator --create-namespace
```

Upgrade with `helm upgrade` and the same values. The chart's `version`, `appVersion` and the
Makefile `VERSION` move together.

## Uninstall

```sh
helm uninstall nifi-operator -n nifi-operator
```

The CRDs (`nificlusters.nifi.ksolves.com`, `nifinodegroups.nifi.ksolves.com`) carry the
annotation `helm.sh/resource-policy: keep`, so `helm uninstall` leaves them in place (and the
ArgoCD sync options `Prune=false,Delete=false`, so an Argo CD Application does too), and with
them every NiFiCluster and NiFiNodeGroup. Their pods, Services, Secrets and volumes stay too, but
nothing reconciles them until the operator is installed again, and with the CSI driver gone new
TLS pods cannot start. A later `helm install` with the same release name and namespace adopts the
CRDs and takes the clusters back over.

Deleting a CRD deletes every object of its kind in every namespace: `kubectl delete crd
nificlusters.nifi.ksolves.com` deletes every NiFiCluster, its pods and Secrets, and its volumes
where `persistentVolumeClaimRetentionPolicy.whenDeleted` is `Delete` (the default, `Retain`, keeps
them). To remove everything deliberately, delete the NiFiClusters while the operator still runs
(it holds a finalizer on each one), then uninstall the chart and delete the CRDs:

```sh
kubectl delete nificlusters --all --all-namespaces
helm uninstall nifi-operator -n nifi-operator
kubectl delete crd nificlusters.nifi.ksolves.com nifinodegroups.nifi.ksolves.com
```

With `crds.install: false` the chart neither installs nor removes the CRDs.

## Clusters from pre-release builds

Pre-release builds of the operator had features the Community edition does not include. Their
clusters keep running after an upgrade to this release, with the changes below. Clusters from a
0.13.x build also restart once, and one without `spec.image` moves to the stock `apache/nifi`
image: see
[Upgrading from a release with custom node images](#upgrading-from-a-release-with-custom-node-images-013x).

- **Clusters with OIDC login:** move them to the single-user login before upgrading, or keep them
  on the pre-release build. After the upgrade the API server drops `spec.authentication.oidc` from
  the object, the pods restart with the generated single-user login, and the users and policies
  NiFi kept for OIDC (on the data volume) no longer apply.
- **Clusters with more than 3 NiFi nodes:** scale them down to 3 first. A cluster that is still
  over the limit after the upgrade is left as it is (`Degraded=True`, `NodeLimitExceeded`) until
  its node groups are scaled down; scaling down and deleting it are still allowed. A cluster with
  several inline node groups can only change `spec.nodeGroups` down to one entry (see below), so
  scale it down before upgrading, or remove its extra node groups afterwards.
- **Clusters with several node groups or NiFiNodeGroups:** they keep running. Updates that leave
  a stored multi-entry `spec.nodeGroups` unchanged still validate (CRD validation ratcheting,
  Kubernetes 1.30+); any change to it must bring it down to one entry. Existing NiFiNodeGroups
  can be updated, scaled and deleted, but no new one can be created. Autoscalers on NiFiNodeGroups
  keep scaling them through `/scale` within the 3-node total; a dedicated validating webhook checks
  the cluster-wide total there too, and past the limit (from before an upgrade added the check, or
  with the webhook disabled) the operator keeps reconciling everything else, it just never scales a
  StatefulSet up further, and sets `Degraded=True` (`NodeLimitExceeded`) until the total is back
  within it. See [NiFiNodeGroup objects and autoscaling](autoscaling.md).
- **Clusters with `spec.route` or `spec.gateway`:** the API server drops the field, and the
  hostname is no longer an allowed proxy host or on the node certificates, so the TLS pods roll.
  Add the hostname to `spec.proxyHosts` first. The Route, HTTPRoute, BackendTLSPolicy and
  `<name>-ca-bundle` ConfigMap the operator created stay in place, no longer updated, until the
  NiFiCluster is deleted ([Exposing a cluster](exposure.md)).
- **Clusters with `serviceType: NodePort` or `spec.service.extraPorts`:** a stored NodePort
  still validates on update (CRD validation ratcheting, Kubernetes 1.30+; older API servers
  reject any update until `serviceType` is changed), but the operator turns the cluster Service
  into ClusterIP and the node ports are released. `extraPorts` are dropped from the Service and
  the NiFi container, which rolls the pods. Create your own Service first if clients use them.
  Remove `extraPorts` from your manifests too: `kubectl apply` rejects the unknown field.
- **A volume expansion in progress:** let it finish before upgrading (`StorageResizing` back to
  `False`), or finish it by hand: `kubectl patch pvc` every PVC of the node group that is still
  below the size in the spec. A cluster whose spec already holds the larger size keeps it; only
  further changes are rejected ([Storage](storage.md#volume-sizes-are-fixed)).
- **Paused clusters** (`nifi.ksolves.com/reconcile-paused`): they resume as soon as the new
  operator runs, and it reverts manual changes made while paused. Finish or undo such changes
  before upgrading. The annotation is now ignored, with an admission warning; to stop the operator
  for maintenance, scale its Deployment to zero.
- **Clusters on ZooKeeper coordination:** they keep running on their ensemble, and updates that
  leave `spec.coordination` unchanged still validate (CRD validation ratcheting, Kubernetes
  1.30+). No new cluster can use ZooKeeper ([Cluster coordination](cluster-coordination.md)).
- **TLS default:** existing node groups keep their TLS setting; only node groups created after the
  upgrade default to TLS. A NiFiNodeGroup added to a cluster that runs without TLS must set
  `tls.enabled: false`. A cluster with `serviceType: LoadBalancer` and TLS off keeps running, but
  an update that changes `serviceType` or TLS without turning TLS on is rejected.
- **Namespace-scoped installs:** remove `rbac` and `watchNamespace` from your values (the chart
  fails to render with `rbac.clusterWide: false` or a `watchNamespace`). The upgrade replaces the
  namespaced Role with the ClusterRole and the webhooks then intercept every namespace, so a
  second, namespace-scoped install in the same Kubernetes cluster must be uninstalled first: two
  operators would reconcile the same clusters.
- **Helm values:** remove `monitoring.prometheusRule`; the chart fails to render when it is
  enabled. `helm upgrade` deletes the `PrometheusRule` it created; keep a copy of the rules first
  if you want them ([Monitoring](#monitoring) lists the metrics to alert on).
- **`helm upgrade --reuse-values`** works: the pre-release defaults of `rbac`, `watchNamespace`
  and `monitoring.prometheusRule` are ignored.

## Images

| Value | Default | Notes |
|---|---|---|
| `image.repository` | `ksoperator/nifi-operator` | operator (controller-manager) image |
| `image.tag` | `nifi-operator-manager-<appVersion>` | used when empty |
| `image.digest` | empty | `sha256:...`; when set the image is pulled by digest |
| `image.pullPolicy` | `IfNotPresent` | |
| `csiDriver.image.repository` / `tag` / `digest` / `pullPolicy` | as above, tag `nifi-operator-csi-driver-<appVersion>` | CSI driver image |
| `imagePullSecrets` | `[]` | Secret names in the release namespace, added to the operator's and the CSI driver's ServiceAccounts |

The pull secrets here are only for the operator's own images. NiFi pods use
`spec.imagePullSecrets` of each `NiFiCluster`.

## CSI driver

`csiDriver.enabled` (default `true`) deploys the `tls.nifi.ksolves.com` CSI node plugin as a
DaemonSet on every node. Node groups with `tls.enabled: true` get their certificates only through
this driver, and TLS is on by default for every new node group, so keep it enabled unless every
cluster sets `tls.enabled: false`. See [TLS](tls.md).

The driver container runs as root with `hostPath` mounts of the kubelet directories, but
unprivileged by default (`csiDriver.privileged: false`): no capabilities, no privilege escalation,
seccomp `RuntimeDefault`, read-only root filesystem, `HostToContainer` mount propagation. It makes
no mounts; it only writes certificate files.

- On vanilla Kubernetes, the release namespace still needs Pod Security Admission
  `privileged` (`pod-security.kubernetes.io/enforce=privileged`), because of the `hostPath`
  volumes.
- On SELinux-enforcing nodes (OpenShift), set `csiDriver.privileged: true`; without it the
  driver can't write into kubelet's pod directories.
- On OpenShift the chart binds the driver's ServiceAccount to the SCC in `csiDriver.scc.name`
  (default `privileged`) when `csiDriver.scc.enabled` is true.

Details: [CSI driver platform notes](csi-driver-platform-notes.md).

## Admission webhook

The validating and mutating webhooks (`webhook.enabled`, default `true`) use `failurePolicy:
Fail`: a NiFiCluster change the webhook cannot check is rejected rather than let through. To keep
that from blocking work:

- **Timeout:** `webhook.timeoutSeconds` (default 5, Kubernetes default 10). A hung call fails
  after 5 s instead of holding the request.
- **Two replicas** serve the webhook (see [Operator high availability](#operator-high-availability)).
  A call that lands on a replica in the second after it crashes can still fail once, before the
  Service drops that endpoint: retry it (`kubectl` and GitOps tools do).

### Permission checks

The operator creates pods and RoleBindings and reads Secrets for every NiFiCluster. So that
it never does this on behalf of someone who could not do it themselves, the webhook asks the API
server (a SubjectAccessReview) whether **the person making the change** may, in the cluster's
namespace. Only new or changed fields are checked:

| Field | Permission needed |
|---|---|
| `configOverrides.secretName`, `authentication.singleUser.secretName`, `tls.keystorePasswordSecretName` | get that Secret |
| `serviceAccountName`, `nodeGroups[].podOverrides`, `podSecurityContext`, `containerSecurityContext` | create pods |
| `configOverrides.bootstrapConf` (cluster or node group): `java`, `java.arg.*`, ... | create pods |
| `spec.image.repository` outside `webhook.trustedImageRepositories` | create pods |
| `coordination.bindPodServiceAccounts` | update leases and configmaps |

`webhook.trustedImageRepositories` (operator flag `--trusted-image-repositories`) lists the NiFi
image repositories anyone allowed to edit a NiFiCluster may use. It defaults to `apache/nifi`,
the stock Apache NiFi images; list only repositories whose every image you would let a
NiFiCluster editor run. A tag change inside a trusted repository is never checked. With the list
empty (or after `helm upgrade --reuse-values` from a chart without it), every new image repository
needs `create pods`. The same checks apply to NiFiNodeGroups, including one created before its
NiFiCluster. With `webhook.enabled: false` none of these checks run: anyone allowed to edit a
NiFiCluster then effectively has every permission in this table.

### Pod security

NiFi pods meet the Kubernetes `restricted` Pod Security Standard and OpenShift's `restricted-v2`
SCC by default:

- the `nifi` container: `runAsNonRoot`, no privilege escalation, all capabilities dropped,
  `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem: true`;
- the pod: `seccompProfile: RuntimeDefault`, and `fsGroup: 0` with
  `fsGroupChangePolicy: OnRootMismatch` (chart value `nifiPods.fsGroup`, operator flag
  `--nifi-fs-group`; empty sets none, for platforms that assign one themselves);
- the UID: `runAsUser: 1000` with `runAsGroup: 0` (chart value `nifiPods.runAsUser`, operator flag
  `--nifi-run-as-user`; empty sets neither, for platforms that assign the UID themselves, such as
  OpenShift's `restricted-v2`). The stock `apache/nifi` images name their user (`nifi`, UID 1000),
  and `runAsNonRoot` can only verify a numeric UID, so elsewhere the pods need one. Not 0;
- `supplementalGroups: [1000]`: the stock images' `lib/` and `bin/nifi.sh` are only open to the
  `nifi` group (1000), so a UID the platform assigns needs that group to start NiFi. Groups set in a
  `podSecurityContext` are added to it, never put in its place;
- the seccomp profile: chart value `nifiPods.seccompProfile`, operator flag
  `--nifi-seccomp-profile`, `RuntimeDefault` by default; empty leaves it unset on the NiFi pods
  and containers and as the default for sidecars, for policies that
  forbid setting one, e.g. OpenShift's `anyuid` SCC. A NiFiCluster's own seccomp setting still
  applies. Changing it changes the pod template, so every NiFiCluster rolls once after the
  operator restarts;
- user `podOverrides.initContainers` and `sidecars` without a `securityContext` get the same
  container settings.

With a read-only root filesystem NiFi writes to emptyDirs: `nifi-scratch` (`work/`, which holds
the unpacked NARs, about 1 GB, plus `run/`, `nar_repository/`, `assets/`, `connector_assets/`,
`nar_extensions/`, `python_extensions/` and `/tmp`), `nifi-conf`
(the image's `conf/`, copied by the `nifi-init` init container for the entrypoint to patch) and
`nifi-passwd` (an `/etc/passwd` with the pod's UID). They count against the node's ephemeral
storage. NiFi's local state (`state/local`) lives on the `data` volume, so it survives a pod
restart.

The data stays usable under another UID. On its first start on each volume, `nifi-init` gives
the files the pod's UID owns group 0, group read-write, and setgid directories (marker file
`.nifi-group-writable`, written once the pass succeeds; it walks the whole volume once, which
takes a while on a large content repository). NiFi runs with primary group 0 (`runAsGroup: 0`, or
the custom node images' `USER 1000:0`) and `umask 0002`, so its new files are group-writable too. A pod with any UID in group 0
(OpenShift's arbitrary UIDs, or another `runAsUser`) can then read and write the data, whatever the
storage supports. To change an existing cluster's UID, run it on 0.13.0 under its old UID first.

Upgrading to 0.13.0 restarts every NiFi pod once (new init container, volumes and security
context). User init containers and sidecars without a `securityContext` now run with the
restricted defaults; give one that needs root or a writable root filesystem its own
`securityContext` (the webhook warns about such containers).

`spec.podSecurityContext` and `spec.containerSecurityContext` (cluster) and
`podOverrides.podSecurityContext` / `podOverrides.containerSecurityContext` (node group) are
merged over these defaults field by field, the node group's last. Setting them needs `create pods`
(below). The webhook warns when a value takes the pods out of the restricted profile, e.g.
`readOnlyRootFilesystem: false` for a processor that writes outside these directories.

### Upgrading from a release with custom node images (0.13.x)

From 1.0.0 on the operator runs the stock `apache/nifi` images:

- **Every NiFi pod restarts once**, node by node: the container's command, the security context
  (`runAsUser`, `runAsGroup`, `supplementalGroups`) and the ConfigMap (now holding the entrypoint)
  change. Plan the upgrade like any rolling restart. The data volumes are kept as they are.
- **Running clusters keep their image.** The webhook wrote the custom image into each cluster's
  `spec.image` when it was created; change `spec.image` to `apache/nifi:<version>` when you are
  ready. A cluster created with the webhook disabled has no `spec.image` and moves to
  `apache/nifi:2.12.0` with the upgrade: set `spec.image` first to keep its current image.
- **`helm upgrade --reuse-values`** keeps the old values. On a platform that assigns UIDs
  (`nifiPods.fsGroup: ""`, e.g. OpenShift) the chart then leaves `runAsUser` unset as well.
  Elsewhere the operator's default, 1000, applies.
- **A `webhook.trustedImageRepositories` you set yourself** is kept as it is: add `apache/nifi`
  to it, or new clusters on the default image need `create pods` permission. Clusters still on a
  custom node image from an earlier release keep working; add that image's repository to the
  list if people without `create pods` permission edit them.

### Default NiFi node image

A NiFiCluster without `spec.image` runs the operator's default node image:
`nifiNodeImage` in the chart values (operator flag `--default-nifi-image`), by default
`apache/nifi:2.12.0`, the stock Apache NiFi image. No custom image is needed: the
operator ships its own entrypoint in each node group's ConfigMap and runs it as the container's
command ([compatibility.md](compatibility.md#how-the-operator-runs-a-stock-image)). The webhook writes it into the cluster's spec when the cluster is created, so a later
operator upgrade doesn't change the image of running clusters: update `spec.image` to move them.
Keep a custom `nifiNodeImage.repository` in `webhook.trustedImageRepositories`. The webhook also
rejects a NiFi version, read from the image tag, that the cluster's settings do not support (see
[Compatibility](compatibility.md)), and only warns when the tag names no version.

**Pinning the NiFi node image.** Docker Hub tags such as `2.12.0` can be re-pushed. In production
set `spec.image.repository` and `tag` explicitly, and pin by digest by appending it to the tag, e.g.
`tag: 2.12.0@sha256:<digest>`; get the digest with
`docker buildx imagetools inspect <image>`. The digest form works in `spec.image.tag` only; do not
put `@sha256:` in the chart's `nifiNodeImage.tag` / `--default-nifi-image`: the flag splits at the
last `:`, so the version check sees no NiFi version (warning) and the trusted-repository checks
compare the wrong repository string. The samples keep plain tags on purpose (digests would
go stale every release). NiFi 2.12.0 is the latest Apache NiFi 2.x this release is tested with.

### Webhook certificate

`webhook.certManager.enabled` chooses where the serving certificate comes from:

| Value | Certificate |
|---|---|
| empty (default) | cert-manager if the cluster serves `cert-manager.io/v1` at install/upgrade time, else the Helm-generated one |
| `true` | cert-manager: a self-signed root `Issuer` issues a CA (`caDuration`, 10 years); a CA `Issuer` issues the serving certificate (`duration` 90 days, renewed `renewBefore` 30 days before expiry). cert-manager injects the **CA** as `caBundle` |
| `false` | Helm generates a CA (10 years) and a serving certificate (1 year), keeps both in the Secret `<release>-webhook-server-cert` and sets the CA as `caBundle` |

**Rotation without downtime.** In both modes the `caBundle` is the CA, which stays the same while
the serving certificate is renewed; the operator reloads the renewed certificate from its Secret
without a restart. With cert-manager this happens on its own. With the Helm-generated certificate
it happens on any `helm upgrade` within 60 days of the serving certificate's expiry (the expiry is
recorded in the Secret's `nifi.ksolves.com/serving-cert-not-after` annotation), so upgrade at least
yearly, or use cert-manager. Alert on the certificate's expiry metric (see
[Monitoring](#monitoring)) to hear of it ahead either way.

Tools that render the chart without cluster access (`helm template`, Argo CD) cannot read the
existing Secret back, so they would generate a new certificate on every render; set
`webhook.certManager.enabled: true` there.

`webhook.enabled: false` removes the webhooks and their certificate and starts the operator with
`ENABLE_WEBHOOKS=false`. The checks the CRD itself enforces (CEL validation rules, see
[API reference](api-reference.md)) still apply; the webhook-only ones (Secret contents,
StorageClass type, permission checks) and its defaulting do not.

## Monitoring

With the Prometheus Operator CRDs installed (OpenShift user-workload monitoring included):

| Value | Creates |
|---|---|
| `monitoring.serviceMonitor.enabled` | a `ServiceMonitor` for the operator's metrics endpoint, scraping with a dedicated ServiceAccount token bound to the `<release>-metrics-reader` ClusterRole |
| `monitoring.labels` | extra labels on it, e.g. the label your Prometheus selects |

The chart ships no alert rules. Metrics worth alerting on, in a `PrometheusRule` of your own:

| Metric | Watch for |
|---|---|
| `nifi_operator_webhook_certificate_expiry_timestamp_seconds` | `min(...) - time() < 14 * 86400`: the webhook serving certificate expires soon; once it has, every NiFiCluster change is rejected. Also `absent(...)`: no operator replica is reporting. |
| `controller_runtime_webhook_requests_total{code=~"5.."}` | the webhook answering with errors: NiFiCluster changes fail |
| `controller_runtime_webhook_latency_seconds_bucket` | high latency: the API server gives up after `timeoutSeconds` (5 s) |
| `controller_runtime_reconcile_errors_total{controller="nificluster"}` | reconciles failing repeatedly |

## OpenShift

- To run NiFi pods under the default `restricted-v2` SCC, set `nifiPods.fsGroup: ""` and
  `nifiPods.runAsUser: ""` (and leave `nifiPods.openshiftSCC` empty): OpenShift then assigns the
  fsGroup and UID from the namespace's range (it rejects the default `fsGroup: 0` and
  `runAsUser: 1000`) and allows the pods' `RuntimeDefault` seccomp profile. NiFi works with any
  UID in group 0. See [Pod security](#pod-security).
- `nifiPods.openshiftSCC` (default empty) names an SCC the operator binds each cluster's NiFi
  ServiceAccount (`<cluster>-nifi`) to instead. The SCC must allow the pods' seccomp profile:
  `anyuid` allows none, so with it also set `nifiPods.seccompProfile: ""` (the chart fails to
  render otherwise); `restricted-v2` keeps `RuntimeDefault`. Binding a ServiceAccount to an SCC
  requires `use` on it, so the chart grants the operator `use` on the SCC named here, and on no
  SCC when the value is empty. The rule and `--nifi-scc` follow the value alone, not the cluster's
  APIs, so `helm template` and Argo CD render them too.
- An existing cluster whose files were written before 0.13.0 under a fixed UID (e.g. `anyuid`)
  keeps them owned by that UID with group-read-only permissions. Before it runs under
  `restricted-v2`, give its volumes group 0 and group write once (e.g. a one-off privileged
  `chgrp -R 0` and `chmod -R g+rwX`), or recreate it with new volumes.
- The CSI driver SCC binding is automatic; see [CSI driver](#csi-driver).
- A values file for OpenShift (`restricted-v2` for NiFi, a privileged CSI driver, monitoring
  through user-workload monitoring):

  ```yaml
  nifiPods:
    openshiftSCC: ""
    fsGroup: ""
    runAsUser: ""
  csiDriver:
    privileged: true
  monitoring:
    serviceMonitor:
      enabled: true
  ```

The CSI driver's SCC binding is rendered when the cluster serves `security.openshift.io/v1`
(`helm install`/`upgrade`, Argo CD). A plain `helm template` can't see the cluster: on OpenShift
set `csiDriver.scc.autoDetect=false` there, or pass `--api-versions security.openshift.io/v1`. The SCC rule and flag for `nifiPods.openshiftSCC` depend on the value only.

## Watch scope

The operator watches NiFiClusters in every namespace through a ClusterRole, and its webhooks
intercept NiFiClusters and NiFiNodeGroups everywhere: install one operator per Kubernetes cluster.
A namespace-scoped install (one namespace, a namespaced Role) is not available in the Community
edition; the chart fails to render with `rbac.clusterWide: false` or a `watchNamespace`.

Every permission and the reason for it: [Operator permissions](security.md#operator-permissions).

## Operator NetworkPolicy

With `networkPolicy.enabled: true` the chart adds a NetworkPolicy for the operator pods. It allows
ingress only to:

- the health port (`probes.healthProbeBindAddress`, default 8081), from any source: the kubelet
  probes it from the node;
- the webhook port, 9443 (with `webhook.enabled`), from `networkPolicy.webhookFrom`;
- the metrics port, 8443 or 8080 (with `metrics.enabled`), from `networkPolicy.metricsFrom`.

Both lists take NetworkPolicy peers (`podSelector`, `namespaceSelector`, `ipBlock`); empty, the
default, means any source. Egress is not restricted: the operator needs the API server and the NiFi
pods. The policy needs a network plugin that enforces NetworkPolicies.

```yaml
networkPolicy:
  enabled: true
  metricsFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
```

Keep `webhookFrom` empty unless you know the addresses the API server connects from: it usually
comes from the node or control-plane network, which pod and namespace selectors don't match, and
a blocked webhook rejects every NiFiCluster change (`failurePolicy: Fail`). NetworkPolicies for the
NiFi pods are up to you; see the sample in [Security](security.md).

## Other values

| Value | Default | Notes |
|---|---|---|
| `crds.install` | `true` | install the `NiFiCluster` CRD with the chart |
| `clusterDomain` | `cluster.local` | the cluster's DNS domain; used in NiFi pod FQDNs and certificate SANs |
| `leaderElection.enabled` | `true` | |
| `metrics.enabled` / `metrics.secure` | `true` / `true` | metrics on `:8443` (HTTPS) or `:8080` |
| `networkPolicy.enabled` | `false` | NetworkPolicy for the operator pods; see [Operator NetworkPolicy](#operator-networkpolicy) |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | | operator pod |

See `charts/nifi-operator/values.yaml` for the full list.

## Operator high availability

The chart runs **two** operator replicas by default (`replicaCount: 2`):

- **Only the leader reconciles.** The replicas elect a leader through the Lease
  `9ffb69f3.ksolves.com` in the operator's namespace; the other one waits. The leader's in-memory
  caches are never shared, and a new leader starts from the API server's state.
- **Every replica serves the admission webhook.** A replica is Ready (and receives webhook
  traffic) only once its webhook server listens and it can reach the API server.
- **A PodDisruptionBudget** (`podDisruptionBudget.minAvailable: 1`) keeps one replica, and so one
  webhook endpoint, through node drains. The webhook uses `failurePolicy: Fail`, so without an
  endpoint every NiFiCluster change would be rejected.
- **No dropped webhook calls on a rolling update:** a stopping replica keeps serving for
  `preStopSleepSeconds` (5 s) so the Service stops routing to it first. This uses the native
  `preStop.sleep` action (Kubernetes 1.30+); set `0` on older clusters.
- **Spread over nodes:** a preferred pod anti-affinity on `kubernetes.io/hostname` by default.
  Setting `affinity` replaces it; `topologySpreadConstraints` adds zone spreading.

Failover time:

| Event | New leader after |
|---|---|
| Leader stops normally (rolling update, drain, `kubectl delete pod`) | `preStopSleepSeconds` (5 s) plus about `leaderElection.retryPeriod` (2 s): the leader keeps serving until it is out of the webhook Service, then releases its lease |
| Leader crashes or its node fails | at most `leaderElection.leaseDuration` (15 s) |

`leaderElection.leaseDuration`, `renewDeadline` and `retryPeriod` tune this. A shorter lease means
faster failover but more API writes and more risk of a leader losing its lease on a slow API server.

For a minimal install set `replicaCount: 1`. The PodDisruptionBudget is then not rendered (it would
block every drain). The kustomize manifests in `config/` (`make deploy`) run one replica for
development; use the chart for production.

## Labels and annotations on generated objects

`spec.labels` / `spec.annotations` (cluster) and `nodeGroups[].labels` / `nodeGroups[].annotations`
(node group) are copied onto the objects the operator generates. Node-group objects (ConfigMaps,
Services, the StatefulSet, the PodDisruptionBudget, PVC templates (labels only), node-group
Secrets) get both levels. Cluster-wide objects (the cluster Service, cluster Secrets, the
ServiceAccount and RBAC) get the cluster-level values only.

- The same key at both levels: the node group's value wins.
- The operator's own labels (`app.kubernetes.io/name`, `app.kubernetes.io/instance`,
  `app.kubernetes.io/managed-by`, `nifi.ksolves.com/node-group`) always win; user values for them
  are ignored. `nifi.ksolves.com/node-group` is reserved too: the operator sets it on node-group
  objects, and drops it from cluster-wide objects if a user sets it in `spec.labels`.
- Pods are excluded: the pod template carries only the operator's labels, so editing labels or
  annotations never restarts NiFi.
- PVCs get the labels when they are created; existing PVCs are not relabelled (a StatefulSet's
  `volumeClaimTemplates` are immutable).
- Edits apply on the next reconcile, including to the create-once Secrets (CA, sensitive
  properties key, login credentials). Adding or changing a key is applied; removing a key from
  `spec.labels` / `spec.annotations` does not remove it from those create-once Secrets (the operator
  only adds or overwrites their metadata). Delete such leftovers by hand, e.g.
  `kubectl label secret <name> <key>-` (or `kubectl annotate secret <name> <key>-`).

```yaml
spec:
  labels:
    team: data-platform
  annotations:
    owner: data-team@example.com
  nodeGroups:
    - name: default
      labels:
        team: ingest        # overrides the cluster's "team"
```

Field reference: [API reference](api-reference.md).

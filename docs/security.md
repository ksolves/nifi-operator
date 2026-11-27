# Security

How the operator protects the NiFi clusters it runs, what you should configure on top, and the
limits of its security model. To report a security issue, see [SECURITY.md](../SECURITY.md).

- [Hardening checklist](#hardening-checklist)
- [Pod security](#pod-security)
- [Admission checks](#admission-checks)
- [TLS and the CSI driver](#tls-and-the-csi-driver)
- [Network policies](#network-policies)
- [Known limitations](#known-limitations)
- [Operator permissions](#operator-permissions)

## Hardening checklist

The defaults already cover most of this list; the rest depends on your cluster.

- [ ] **TLS on** (the default for new node groups): HTTPS for the UI and API, mutual TLS between
      nodes. See [TLS](tls.md).
- [ ] **Your own login Secret**, or keep the generated one, and limit who can read Secrets in the
      cluster's namespace. See [Login credentials](authentication.md).
- [ ] **Admission webhook on** (`webhook.enabled: true`, the default). Without it none of the
      [admission checks](#admission-checks) run.
- [ ] **`webhook.trustedImageRepositories`** lists only repositories whose every image you would let
      a NiFiCluster editor run (default `apache/nifi`).
- [ ] **Namespace labelled for the `restricted` Pod Security Standard**
      (`pod-security.kubernetes.io/enforce=restricted`); on OpenShift keep the default
      `restricted-v2` SCC (`nifiPods.openshiftSCC: ""`). See [Pod security](#pod-security).
- [ ] **NetworkPolicies** for the NiFi pods (see [Network policies](#network-policies)) and the
      operator (`networkPolicy.enabled`).
- [ ] **`serviceType: ClusterIP`** (the default), with your own Ingress, Route or HTTPRoute in front.
      If you use `LoadBalancer`, set `spec.service.loadBalancerSourceRanges` on the NiFiCluster.
- [ ] **Least privilege for people:** give users the `<release>-nificluster-editor-role` or
      `-viewer-role` in their namespace, not cluster-wide edit rights. Rights to create pods or read
      Secrets in a NiFi namespace are as powerful as the operator's own there.
- [ ] **Restrict pod creation** in NiFi namespaces to the StatefulSet controller, if other users
      can create pods there (see [Known limitations](#known-limitations)).
- [ ] **Operator metrics stay authenticated** (`metrics.secure: true`, the default).
- [ ] **Image digests and scans:** pin the operator and NiFi images by digest and scan them; see
      [Verifying the images](verify-images.md).

## Pod security

NiFi pods meet the Kubernetes `restricted` Pod Security Standard and OpenShift's `restricted-v2`
SCC by default. Every container runs as a non-root user with no privilege escalation, all
capabilities dropped, a read-only root filesystem and the `RuntimeDefault` seccomp profile. User
init containers and sidecars without a `securityContext` get the same settings. The NiFi pods mount
a ServiceAccount token only when they need the Kubernetes API (Kubernetes coordination).

Changing the pod or container security context of a NiFiCluster needs permission to create pods,
and the webhook warns when a value takes the pods out of the restricted profile. Details, chart
values and operator flags: [Pod security](installation.md#pod-security).

## Admission checks

The validating webhook rejects a NiFiCluster or NiFiNodeGroup change before the operator acts on it:

- **Permission checks:** the operator creates pods and reads Secrets for every NiFiCluster, so
  the webhook checks (with a SubjectAccessReview) that the person making a change may do the
  same: read a Secret the cluster refers to, create pods for pod overrides, security contexts, a
  ServiceAccount, JVM arguments or an untrusted image repository. See
  [Permission checks](installation.md#permission-checks).
- **Reserved names:** environment variables, volumes, containers and `nifi.properties` keys the
  operator manages can't be overridden.
- **Safe values:** for example no wildcard or IP `tls.extraSANs`, a minimum password length for the
  single-user login, no NFS or CIFS storage class for the repositories, and no change to the size
  or StorageClass of an existing node group's volumes.

The webhook uses `failurePolicy: Fail`, so a change it cannot check is rejected rather than let
through. See [Admission webhook](installation.md#admission-webhook).

## TLS and the CSI driver

With TLS on, each NiFiCluster gets its own CA (RSA 2048, valid 5 years, path length 0), stored in
the `<cluster>-ca-certificate` Secret. The bundled CSI driver issues every NiFi pod its own
certificate when the pod starts, and only to a pod whose controller is that cluster's StatefulSet.
The driver can read only that cluster's CA Secret, through a per-cluster Role. Leaf certificates are
rotated by restarting one pod at a time at half their validity. See [TLS](tls.md) and
[CSI driver platform notes](csi-driver-platform-notes.md).

## Network policies

The operator creates no NetworkPolicy for the NiFi pods: which clients may reach NiFi depends on
your cluster. If your network plugin enforces NetworkPolicies, start from the sample below. For the
operator's own pods the chart has an optional policy (`networkPolicy.enabled`, see
[Operator NetworkPolicy](installation.md#operator-networkpolicy)).

Traffic a NiFi cluster needs:

| From | To | Port | Why |
|---|---|---|---|
| NiFi pods of the same cluster | NiFi pods | 11443 (cluster protocol), 6342 (NiFi's default load-balance port), 8443 or 8080 (API) | Node-to-node traffic |
| Operator pods | NiFi pods | 8443 (TLS) or 8080 | The operator calls the NiFi REST API: cluster status every `nifiStatusInterval`, node removal on scale-down |
| Your clients, Ingress controller or Route router | NiFi pods | 8443 (TLS) or 8080 | UI and API |
| NiFi pods | Kubernetes API server | 443 or 6443 | Kubernetes coordination (Leases, ConfigMaps) |
| NiFi pods | DNS | 53 | Name resolution of the other nodes |

NiFi pods carry the labels `app.kubernetes.io/name: nifi`, `app.kubernetes.io/instance: <cluster>`
and `nifi.ksolves.com/node-group: <group>`. The operator's pods carry `control-plane:
controller-manager` and `app.kubernetes.io/name: nifi-operator`. The NiFi pods' probes run inside
the container (exec), so they need no rule. With TLS on, NiFi listens on 8443 only, not 8080.

Sample for a cluster `nifi` in namespace `nifi-apps`, with TLS on, the operator in
`nifi-operator` and an ingress controller in `ingress-nginx`. It restricts ingress only; add
egress rules for DNS, the API server and the systems your flows reach if you also restrict
egress.

```yaml
# Deny all other ingress to the cluster's NiFi pods.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nifi-default-deny
  namespace: nifi-apps
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nifi
      app.kubernetes.io/instance: nifi
  policyTypes: [Ingress]
---
# Node-to-node traffic within the cluster, on every port.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nifi-intra-cluster
  namespace: nifi-apps
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nifi
      app.kubernetes.io/instance: nifi
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: nifi
              app.kubernetes.io/instance: nifi
---
# HTTPS from the operator (REST API) and the ingress controller (UI and API).
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nifi-https
  namespace: nifi-apps
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nifi
      app.kubernetes.io/instance: nifi
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: nifi-operator
          podSelector:
            matchLabels:
              control-plane: controller-manager
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
      ports:
        - port: 8443
          protocol: TCP
```

With TLS off, use port 8080. On OpenShift, allow the router with the namespace selector
`policy-group.network.openshift.io/ingress: ""` instead of the ingress-nginx namespace. Listening
ports for site-to-site or for processors such as `ListenHTTP` need their own rule.

For the operator itself, use the chart's `networkPolicy.enabled`; installs from `config/` have
kustomize samples in `config/network-policy/`. Whichever way you install it, the API server must reach the webhook on port 9443
of the operator pods (a NetworkPolicy matches the pod's port, not the Service's 443), and your
Prometheus the metrics on port 8443. A blocked webhook rejects every NiFiCluster change.

## Known limitations

- **The CA is not rotated.** It is valid for 5 years. To replace it, delete
  `<cluster>-ca-certificate` and restart every pod of the cluster; until all pods have
  certificates from the new CA, old and new nodes do not trust each other. See
  [Rotation](tls.md#rotation).
- **A forged owner reference can get a certificate.** Someone allowed to create pods in the
  namespace could create a pod that claims the cluster's StatefulSet as its owner while a pod is
  rotated, and the CSI driver would issue it a certificate signed by the cluster CA. Restrict pod
  creation in NiFi namespaces to the StatefulSet controller with a ValidatingAdmissionPolicy or
  Kyverno. See [Known limitation](tls.md#known-limitation).
- **The CSI driver needs host access.** Its container runs unprivileged by default
  (`csiDriver.privileged: false`), but it mounts a host path for the kubelet, so on OpenShift its
  ServiceAccount is bound to the built-in `privileged` SCC (`csiDriver.scc.name`). See
  [CSI driver platform notes](csi-driver-platform-notes.md).
- **Secret access is cluster-wide.** The operator always watches every namespace, so its Secret
  permission covers every namespace; its cache drops the data of Secrets it does not manage. See
  [Secrets](#secrets).
- **No NetworkPolicies for NiFi**, and the operator's own is off by default; see
  [Network policies](#network-policies).
- **Webhook off means no checks.** With `webhook.enabled: false`, anyone allowed to edit a
  NiFiCluster effectively has the operator's permissions in that namespace.
- **One NiFi pod can read and write another NiFiCluster's ConfigMaps in the same namespace.**
  Every NiFi pod's Role grants it `get`/`list`/`create`/`patch`/`delete` on every ConfigMap in its
  namespace, not just its own cluster's: NiFi's own `KubernetesConfigMapStateProvider` creates
  one ConfigMap per component to store state, with names the operator does not control, so the
  grant can't be scoped down to one cluster's ConfigMaps the way the CA Secret's `get` is. Every
  file this would let a compromised pod rewrite to actually run code in, or change the access
  policy of, another cluster's pods - the entrypoint script, `bootstrap.conf.overrides` (JVM args),
  and any `logback.xml`/`authorizers.xml` replacement - is shipped in an operator-owned Secret
  instead, which this Role has no access to at all. Another cluster's component-state ConfigMaps
  (NiFi's own, not the operator's) can still be read or deleted, since their names aren't under
  the operator's control. Run at most one NiFiCluster per namespace if this matters for your
  threat model.

## Operator permissions

This section lists every Kubernetes permission the Helm chart grants the operator and its CSI
driver, and why each is needed. None of the roles uses a wildcard (`*`) verb, resource or API
group, and none grants `escalate`, `bind`, `impersonate`, pod `create` or `pods/exec`.

### Where the rules apply

The operator rules below are a ClusterRole and ClusterRoleBinding: they apply in every namespace
([Watch scope](installation.md#watch-scope)).

### Operator (`<release>-manager-role`)

The rules are generated from the `+kubebuilder:rbac` markers in
`internal/controller/nificluster_controller.go` into `config/rbac/role.yaml`, which the chart
renders as is.

| API group | Resources | Verbs | Why |
|---|---|---|---|
| `nifi.ksolves.com` | `nificlusters`, `nifinodegroups` | get, list, watch, update, patch, delete | Reconciles them and adds or removes its finalizer. `delete` is never called: OpenShift's OwnerReferencesPermissionEnforcement only accepts a blocking owner reference to an object the client may delete, and every object the operator creates references its cluster or node group that way. The operator never creates either kind. |
| `nifi.ksolves.com` | `nificlusters/status`, `nifinodegroups/status` | get, update, patch | Writes `status`. |
| `nifi.ksolves.com` | `nificlusters/finalizers` | update | Also required by OwnerReferencesPermissionEnforcement for owner references with `blockOwnerDeletion`. |
| `apps` | `statefulsets` | get, list, watch, create, update, patch, delete | One StatefulSet per node group. |
| core | `services`, `configmaps` | get, list, watch, create, update, patch, delete | Headless and client Services; NiFi configuration per node group. NiFi itself also stores cluster state in ConfigMaps (Kubernetes coordination). |
| core | `secrets` | get, list, watch, create, update, patch, delete | See [Secrets](#secrets). |
| core | `serviceaccounts` | get, list, watch, create, update, patch, delete | The NiFi pods' ServiceAccount (`<cluster>-nifi`) when `spec.serviceAccountName` is unset. |
| core | `persistentvolumeclaims`, `pods` | get, list, watch | Volume and pod status, readiness, rolling restarts; never creates, changes or deletes a volume. |
| core | `pods/eviction` | create | Rolls pods after a certificate change through the Eviction API, so PodDisruptionBudgets are respected. |
| core | `events` | create, patch | Kubernetes events on NiFiClusters. |
| `policy` | `poddisruptionbudgets` | get, list, watch, create, update, patch, delete | One PodDisruptionBudget per node group. |
| `rbac.authorization.k8s.io` | `roles`, `rolebindings` | get, list, watch, create, update, patch, delete | The NiFi pods' Role (Kubernetes coordination: Leases and ConfigMaps) and RoleBinding, the CSI driver's CA-reader Role and RoleBinding, and the SCC RoleBinding on OpenShift. Kubernetes only lets a client grant permissions it holds itself, so the operator can never grant more than this table. |
| `coordination.k8s.io` | `leases` | get, create, update, patch, delete | Granted on to the NiFi pods for Kubernetes coordination (see above). The operator itself only deletes NiFi's Leases when the cluster is deleted (Leases carry no owner). |
| `storage.k8s.io` | `storageclasses` | get, list, watch | The webhook rejects NFS/CIFS storage classes for the repositories. |
| `authorization.k8s.io` | `subjectaccessreviews` | create | The webhook checks that the person making a change may do what the operator will do for them ([Permission checks](installation.md#permission-checks)). |
| `security.openshift.io` | `securitycontextconstraints` (only the one in `nifiPods.openshiftSCC`) | use | Only when `nifiPods.openshiftSCC` is set: binding the NiFi pods to an SCC requires `use` on it. Not granted by default. The rule follows the value alone, so `helm template` and Argo CD render it too; on a cluster without the `security.openshift.io` API it grants nothing and the operator ignores `--nifi-scc`. |

#### Secrets

The operator creates, updates and deletes these Secrets in each NiFiCluster's namespace, all
labelled `app.kubernetes.io/managed-by` and owned by the cluster:

- the sensitive-properties key (`nifi.sensitive.props.key`);
- the cluster CA and the keystore password, generated or copied from
  `tls.keystorePasswordSecretName` ([TLS](tls.md));
- the generated single-user login and its password hash ([Login credentials](authentication.md));
- the rendered configuration Secret of `configOverrides`.

Of other Secrets it only reads those a NiFiCluster refers to: `authentication.singleUser.secretName`,
`configOverrides.secretName` (cluster or node group) and `tls.keystorePasswordSecretName`. The
webhook first checks that the person who sets or changes such a reference may read that Secret
themselves.

The Kubernetes permission covers every Secret in every namespace. The operator watches Secrets so
it reacts when a referenced one changes, but its in-memory cache drops the data of every Secret it
does not manage (`stripForeignSecretData` in `internal/controller/rbac.go`). Referenced Secrets are
read straight from the API server when needed.

### Leader election (`<release>-leader-election-role`)

A Role in the release namespace: `leases` and `configmaps` (get, list, watch, create, update,
patch, delete) and `events` (create, patch), to elect one active reconciler among the replicas.

### Metrics (`metrics.enabled`)

- `<release>-metrics-auth-role` (ClusterRole): create `tokenreviews` and `subjectaccessreviews`,
  so the metrics endpoint authenticates and authorizes its scrapers.
- `<release>-metrics-reader` (ClusterRole, not bound to the operator): get `/metrics`, for the
  scraper's ServiceAccount.

### CSI driver (`<release>-csi-driver-role`)

A ClusterRole for the driver's own ServiceAccount: get (no list or watch) on `nificlusters`,
`nifinodegroups`, `pods` and `statefulsets`, to check that a pod belongs to the cluster before it
issues the pod a certificate. It has no Secret access of its own: for each TLS cluster the
operator creates a Role and RoleBinding (`<cluster>-csi-ca-reader`) that let it get that
cluster's CA Secret, by name, and nothing else. On OpenShift the driver's ServiceAccount is also bound to the SCC in
`csiDriver.scc.name` (default `privileged`); see
[CSI driver platform notes](csi-driver-platform-notes.md).

### User roles (not used by the operator)

`<release>-nificluster-admin-role`, `-editor-role` and `-viewer-role` are ClusterRoles for
delegating NiFiCluster access to people. They are not bound to anyone by the chart.

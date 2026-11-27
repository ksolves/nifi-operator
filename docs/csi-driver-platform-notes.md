# CSI driver: platform-specific prerequisites

`config/csi/`'s manifests (`csidriver.yaml`, `daemonset.yaml`, `rbac.yaml`) use only upstream
Kubernetes APIs - `storage.k8s.io/CSIDriver`, a plain `DaemonSet`, `hostPath` volumes, and a
`securityContext.privileged: true` Pod. Nothing in them is OKD-, EKS-, GKE-, or AKS-specific, and
they apply unmodified via `kubectl apply -k config/csi` on any of those distributions.

**What is genuinely platform-specific** is whether a given cluster's own admission policy *allows*
a privileged, `hostPath`-mounting DaemonSet to run at all - that's an environment decision, not
something this project's manifests can or should encode.

## Vanilla Kubernetes (kind, minikube, EKS, GKE, AKS)

If the target namespace enforces the restricted or baseline
[Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/),
this DaemonSet's Pods will be rejected outright (`privileged: true` and `hostPath` volumes are
both disallowed under `restricted`, and `hostPath` alone is disallowed under `baseline`). The
operator's own namespace typically runs unrestricted or under `privileged` labeling already
(needed for cert-manager and other cluster add-ons); if not, label the namespace explicitly:

```
kubectl label namespace <namespace> pod-security.kubernetes.io/enforce=privileged
```

## OpenShift / OKD

OKD's Security Context Constraints (SCCs) are stricter than Pod Security Standards by default and
are evaluated instead of (not in addition to) them. This DaemonSet's ServiceAccount
(`nifi-operator-csi-driver`, see `config/csi/rbac.yaml`) needs an SCC binding that permits:
- `allowPrivilegedContainer: true`
- `hostPath` volumes (many built-in SCCs, including `restricted`, disallow this)

A binding to the built-in `hostmount-anyuid` SCC covers both requirements and is the closest
existing built-in match, but **the exact SCC to use for a specific OKD cluster is a decision for
whoever owns SCC assignments there** - a custom, more narrowly-scoped SCC may be preferred over a
built-in one depending on that cluster's own security posture. This is intentionally left as a
decision point here, not baked into `config/csi/rbac.yaml`, so the manifests stay portable across
every other distribution.

Example binding, once the correct SCC is decided:
```
oc adm policy add-scc-to-user hostmount-anyuid -z nifi-operator-csi-driver -n <namespace>
The built-in `privileged` SCC is the default here (`charts/nifi-operator/values.yaml`'s
`csiDriver.scc.name`) - confirmed against a real OKD cluster's actual SCC objects, not assumed
from a name/reputation. `hostmount-anyuid` and `hostaccess` look like
closer, more narrowly-scoped matches by name, but neither actually sets
`allowPrivilegedContainer: true` on stock OKD (only `allowHostDirVolumePlugin: true`) - a pod
requiring both will still be rejected under either. `privileged` is the smallest built-in SCC
that genuinely grants both requirements together; a custom, more narrowly-scoped SCC may still be
preferred over it depending on a given cluster's security posture, which is why the chart's
`csiDriver.scc.name` value stays overridable rather than hardcoded. Always confirm with
`oc get scc <name> -o jsonpath='{.allowPrivilegedContainer}{"\n"}{.allowHostDirVolumePlugin}'`
before assuming an SCC covers both, rather than picking one by name alone.

This chart handles the binding itself (`templates/csi-driver-scc-binding.yaml`, auto-detected via
Helm's `.Capabilities.APIVersions.Has "security.openshift.io/v1"` - renders only on OpenShift/
OKD, nothing to configure on other distributions). A plain `helm template` can't detect the
cluster: on OpenShift set `csiDriver.scc.autoDetect=false` there. The manual command below is only needed
outside the chart (e.g. testing `config/csi/` directly via `kubectl apply -k`):
```
oc adm policy add-scc-to-user privileged -z nifi-operator-csi-driver -n <namespace>
```

### Which namespaces may use the driver

OpenShift's CSI inline-volume admission checks every pod that mounts an inline CSI volume. The
pod's namespace must enforce at least the Pod Security level named by the driver's
`security.openshift.io/csi-ephemeral-volume-profile` label. A driver without the label counts as
`privileged`. In that case the NiFi StatefulSet is rejected in any namespace that doesn't enforce
`privileged`:

```
statefulsets.apps "<name>" is forbidden: <name> uses an inline volume provided by CSIDriver
tls.nifi.ksolves.com and namespace <ns> has a pod security enforce level that is lower than privileged
```

The chart labels the driver `restricted` (`csiDriver.openshiftEphemeralVolumeProfile`), and so
does `config/csi/csidriver.yaml`. A NiFi pod gets only its own read-only keystore and truststore.
The driver issues them only to pods owned by a NiFiCluster's StatefulSet. Set the value to
`baseline` to keep the driver out of `restricted` namespaces, or to `""` to leave it unlabelled.
Other distributions ignore the label.

## Verifying the driver actually registered

Regardless of platform, once the DaemonSet's Pods are `Running`:
```
kubectl get csidrivers
kubectl describe csinode <node-name>
```
should show `tls.nifi.ksolves.com` registered. If the Pods are stuck in `Pending` or
`CreateContainerConfigError` instead of reaching `Running`, the cause is almost always the
admission-policy gap described above, not a bug in the driver itself.

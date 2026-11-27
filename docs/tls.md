# TLS

With `tls.enabled: true` on its node groups, a NiFi cluster serves its UI and REST API over HTTPS
(port 8443) and secures the cluster protocol with mutual TLS. The operator and its CSI driver
issue every certificate; nothing external (cert-manager, a corporate CA) is involved.

TLS is **on by default**: the admission webhook adds `tls.enabled: true` to every node group
created without a `tls` block. A `tls` block without `enabled` means TLS off. Set
`tls.enabled: false` to run a node group over plain HTTP. The default applies only when a node
group is created: an existing node group without a `tls` block keeps TLS off. TLS needs the CSI
driver ([Installation](installation.md#csi-driver)).

```yaml
spec:
  nodeGroups:
    - name: default
      replicas: 3
      storage: 10Gi
      tls:
        enabled: true
        certificateValidityDays: 365    # 1-3650, default 365
        extraSANs:
          - nifi.example.com
```

All node groups of a cluster must use the same `tls.enabled` value (the webhook rejects a mix):
they form one NiFi cluster, whose cluster protocol is secured on every node or on none. A
NiFiNodeGroup joining a cluster that runs without TLS must therefore set `tls.enabled: false`.

`spec.serviceType: LoadBalancer` requires TLS: the webhook rejects a LoadBalancer on a cluster
without it, which would serve NiFi's UI and login over plain HTTP outside the cluster.

`tls.enabled: false` disables more than certificates: NiFi only issues a login token over HTTPS,
so with TLS off nothing can authenticate and every request reaches NiFi as the anonymous user,
however `spec.authentication` is configured. The webhook warns on this; only disable TLS for a
cluster that is itself fully isolated from untrusted traffic. See
[NiFi login credentials](authentication.md).

## How certificates are issued

1. **Operator CA.** When any node group enables TLS, the operator creates a self-signed CA
   (RSA 2048, 5-year validity) in the Secret `<cluster>-ca-certificate` (type
   `kubernetes.io/tls`). It is created once and never replaced, because replacing it would break
   trust with every certificate already issued from it. The CA may sign leaf certificates only
   (path length 0). Only a CA Secret the `NiFiCluster` controls is used: if a Secret of that name
   already exists and wasn't created by the operator, the operator and the CSI driver refuse it
   rather than sign with someone else's key.
2. **CA access for the CSI driver.** The operator creates the Role and RoleBinding
   `<cluster>-csi-ca-reader` in the cluster's namespace. They let the CSI driver's ServiceAccount
   `get` that one Secret and nothing else. Both are removed when no node group uses TLS, and
   garbage-collected with the `NiFiCluster`.
3. **Per-pod certificate.** Each TLS pod mounts an ephemeral volume of the CSI driver
   `tls.nifi.ksolves.com` at `/opt/nifi/tls`. When the pod starts, the driver on its node checks
   that the pod really belongs to the cluster's StatefulSet, reads the CA, and issues a fresh
   leaf certificate (server and client auth). It writes `keystore.p12` and `truststore.p12`
   (PKCS12) into the volume and deletes them when the pod goes away. The leaf key is never stored
   in a Kubernetes object.

The CSI driver is installed by the chart (`csiDriver.enabled`, default `true`); see
[Installation](installation.md#csi-driver). Without it, TLS pods stay in `ContainerCreating`.

## Keystore password

NiFi needs a password to open the PKCS12 keystore and truststore. Each cluster gets its own: the
operator generates a random password when it creates the CA, and keeps it in the same Secret
(`<cluster>-ca-certificate`, key `keystore-password`). To use your own, set
`tls.keystorePasswordSecretName` on every TLS node group to a Secret with a `password` key before
the cluster is created; it is read once, with the CA, and never changes afterwards.

The password never appears in a ConfigMap or the pod spec:

the pods get it as the environment variable `NIFI_KEYSTORE_PASSWORD`, a `secretKeyRef` to the CA
Secret, and the entrypoint writes it into `nifi.properties` inside the container.

`NIFI_KEYSTORE_PASSWORD` is reserved: `podOverrides.env` cannot set it. The password protects only
the file encoding; the private key is exactly as sensitive as the CA Secret it comes from, so keep
Secret read access in the namespace limited.

The operator and the CSI driver must come from the same release: a driver from an earlier pre-release build can write
keystores with a different password, and NiFi then fails with `keystore password was incorrect`
([Troubleshooting](troubleshooting.md#keystore-password-was-incorrect-in-the-nifi-log)).

## Certificate names (SANs)

Every pod's certificate covers:

- the pod's own FQDN, `<pod>.<cluster>-<group>-headless.<namespace>.svc.<clusterDomain>`;
- the cluster Service, `<cluster>.<namespace>.svc` and `<cluster>.<namespace>.svc.<clusterDomain>`;
- every proxy host in `spec.proxyHosts` (without port);
- the node group's `tls.extraSANs`: plain DNS names only; the webhook rejects wildcards and IP
  addresses.

`<clusterDomain>` is the chart value `clusterDomain` (default `cluster.local`).

The pod template carries a hash of these names (`nifi.ksolves.com/cert-sans-hash`), so changing
any of them rolls the node group onto certificates that include the new names.

## Rotation

Leaf certificates are rotated by restarting pods, since a pod gets its certificate only when it
starts. Once a pod has run for half of `certificateValidityDays` (about 182 days with the default
365), the operator evicts it and the replacement gets a new certificate. It rotates one pod at a
time, only when every pod of the node group is present and Ready, and uses the Eviction API so the
node group's PodDisruptionBudget is respected.

The CA is not rotated. To replace it, delete `<cluster>-ca-certificate` and restart every pod of
the cluster: until all of them have certificates from the new CA, old and new nodes do not trust
each other.

Since that replacement is manual, the cluster's `CAExpiringSoon` condition turns `True` once 80%
of the CA's 5-year lifetime has passed (about a year before it actually expires), and the
`nifi_operator_cluster_ca_expiry_timestamp_seconds` Prometheus metric (labeled by cluster) tracks
every cluster's CA expiry, so an alert can fire on it before anyone has to notice `kubectl
describe` on their own. A leaf certificate is never issued with a later expiry than its own CA's,
even if `certificateValidityDays` would otherwise ask for one.

## Clients

Clients must trust the cluster's CA, the `tls.crt` of `<cluster>-ca-certificate`:

```sh
kubectl get secret <cluster>-ca-certificate -n <namespace> -o jsonpath='{.data.tls\.crt}' | base64 -d > nifi-ca.crt
```

For an OpenShift Route or Gateway API HTTPRoute of your own, which terminates or re-encrypts TLS
in front of NiFi, see [Exposing a cluster](exposure.md).

## Known limitation

The CSI driver issues a certificate only to a pod whose controller is the cluster's StatefulSet,
checked by UID. Owner references aren't protected by Kubernetes, though: someone allowed to create
pods in the namespace can forge one. Certificate rotation evicts one pod at a time, which briefly
frees that pod's name, so such a user could create a pod under that name and receive a certificate
signed by the cluster CA. Anyone with pod-create permission in the namespace is already highly
trusted, so this is accepted. To close it, admit pods controlled by a NiFi StatefulSet only from the
StatefulSet controller (`system:serviceaccount:kube-system:statefulset-controller`), with a
ValidatingAdmissionPolicy or Kyverno.

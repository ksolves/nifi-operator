# Installation

This page installs the operator on an existing Kubernetes or OpenShift cluster. Next,
[First steps](first-steps.md) creates a NiFi cluster with it. To try everything on a laptop
instead, follow [kind](kind.md).

## Prerequisites

- Kubernetes 1.32 or newer, or OpenShift/OKD 4.19, and `kubectl` access with cluster-admin rights
  (the chart installs CRDs, a ClusterRole and a CSI driver).
- [Helm](https://helm.sh/) 3 or newer.
- A default StorageClass (or set `storageClassName` on each node group) that can provision
  `ReadWriteOnce` volumes. Volume expansion is optional; it lets node groups grow their disks
  later.
- Optional: [cert-manager](https://cert-manager.io/) for the admission webhook certificate. Without
  it the chart generates one.

The images are public on Docker Hub: the operator and its CSI driver in
`ksoperator/nifi-operator`, and NiFi itself from the stock `apache/nifi` image. No registry login
is needed.

## Install the operator

The operator runs in its own namespace. Its certificate helper, a CSI driver that writes each NiFi
pod's TLS keystore, mounts host directories, which Pod Security Admission only allows in a
`privileged` namespace:

```sh
kubectl create namespace nifi-operator
kubectl label namespace nifi-operator pod-security.kubernetes.io/enforce=privileged
```

Install the chart, from a checkout of the repository:

```sh
git clone https://github.com/ksolves/nifi-operator.git
helm install nifi-operator nifi-operator/charts/nifi-operator -n nifi-operator --wait
```

or from the packaged chart attached to a [GitHub release](https://github.com/ksolves/nifi-operator/releases):

```sh
helm install nifi-operator nifi-operator-1.0.0.tgz -n nifi-operator --wait
```

or straight from Docker Hub, with no download (Helm 3.8 or newer pulls the chart over OCI from the
same `ksoperator/nifi-operator` repo as the images):

```sh
helm install nifi-operator oci://registry-1.docker.io/ksoperator/nifi-operator --version 1.0.0 \
  -n nifi-operator --wait
```

## Check the installation

```sh
kubectl -n nifi-operator get pods
kubectl -n nifi-operator wait --for=condition=Available deployment --all --timeout=5m
kubectl get crd nificlusters.nifi.ksolves.com nifinodegroups.nifi.ksolves.com
```

Expect two `nifi-operator-controller-manager` pods (`1/1`, only the elected leader reconciles; both
serve the admission webhook) and one `nifi-operator-csi-driver` pod per node (`3/3`), all
`Running`.

Use the full resource name `nificlusters.nifi.ksolves.com` with `kubectl` if another NiFi operator
is installed in the same cluster: a short name such as `nificluster` may resolve to its CRD.

## OpenShift

The defaults run on OpenShift too, with these values for the default `restricted-v2` SCC (the
platform assigns the NiFi pods' UID and fsGroup) and SELinux-enforcing nodes:

```yaml
# openshift-values.yaml
nifiPods:
  fsGroup: ""
  runAsUser: ""
csiDriver:
  privileged: true
```

```sh
helm install nifi-operator nifi-operator/charts/nifi-operator -n nifi-operator -f openshift-values.yaml --wait
```

[Installation reference: OpenShift](../installation.md#openshift) covers the other SCC options.

## Upgrade and uninstall

```sh
helm upgrade nifi-operator nifi-operator/charts/nifi-operator -n nifi-operator --wait
helm uninstall nifi-operator -n nifi-operator
```

`helm uninstall` keeps the CRDs, so NiFiClusters and their data survive it; deleting the CRDs
deletes every NiFiCluster ([Uninstall](../installation.md#uninstall)). Read
[Clusters from pre-release builds](../installation.md#clusters-from-pre-release-builds) before
moving from a pre-release build.

## What's next

- [First steps](first-steps.md): create a NiFi cluster, log in and scale it.
- [Installation reference](../installation.md): every chart value, the admission webhook and
  monitoring.

# Ksolves Operator for Apache NiFi

[![Tests](https://github.com/ksolves/nifi-operator/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/ksolves/nifi-operator/actions/workflows/test.yml)
[![Lint](https://github.com/ksolves/nifi-operator/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/ksolves/nifi-operator/actions/workflows/lint.yml)
[![Release](https://img.shields.io/github/v/release/ksolves/nifi-operator)](https://github.com/ksolves/nifi-operator/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Docker Hub](https://img.shields.io/badge/images-ksoperator%2Fnifi--operator-blue)](https://hub.docker.com/r/ksoperator/nifi-operator)

A Kubernetes operator for [Apache NiFi](https://nifi.apache.org) 2.x. A `NiFiCluster` resource
(`nifi.ksolves.com/v1alpha1`) describes one NiFi cluster, one node group of up to 3 nodes. The
operator runs the node group as a StatefulSet and manages cluster coordination (Kubernetes
Leases), TLS, login credentials, the cluster Service and clean scale-down.

Each NiFiCluster has **one node group of at most 3 NiFi nodes**, and users log in with NiFi's
single-user login. Larger clusters, several node groups (NiFiNodeGroup) and single sign-on (OIDC)
are part of Ksolves' commercial edition; see [Support](#support).

## Prerequisites

- Kubernetes 1.32 or newer, or OpenShift/OKD 4.19.
- Helm 3 or newer.
- Nothing else for NiFi itself: the operator runs the stock `apache/nifi` images, NiFi 2.11 or
  newer ([compatibility](docs/compatibility.md)).
- Optional: cert-manager for the webhook certificate; without it the chart generates one.

To build from source: Go 1.27 (the version in `go.mod`) and Docker.

## Install

Download the chart from the [latest release](https://github.com/ksolves/nifi-operator/releases/latest)
and install it into a namespace that allows privileged pods (the CSI driver mounts host
directories):

```sh
kubectl create namespace nifi-operator
kubectl label namespace nifi-operator pod-security.kubernetes.io/enforce=privileged
helm install nifi-operator nifi-operator-1.0.0.tgz -n nifi-operator --wait
```

Or install straight from Docker Hub, with no download (Helm 3.8 or newer pulls the chart over
OCI from the same `ksoperator/nifi-operator` repo as the images):

```sh
helm install nifi-operator oci://registry-1.docker.io/ksoperator/nifi-operator --version 1.0.0 \
  -n nifi-operator --wait
```

The images are public on Docker Hub (`ksoperator/nifi-operator`, and `apache/nifi` for NiFi);
[Verifying the images](docs/verify-images.md) shows how to check them. Values for images, the CSI driver, the webhook certificate and OpenShift
are described in [Installation](docs/installation.md); [Getting started](docs/getting-started/installation.md)
walks through it step by step.

The operator watches NiFiClusters in every namespace, with a ClusterRole; one operator per
Kubernetes cluster.

## A minimal NiFiCluster

```yaml
apiVersion: nifi.ksolves.com/v1alpha1
kind: NiFiCluster
metadata:
  name: nifi
  namespace: nifi-operator
spec:
  nodeGroups:
    - name: default
      replicas: 3
      storage: 10Gi
```

Without `spec.image` the cluster runs the stock `apache/nifi:2.12.0` image (see
[Installation](docs/installation.md#default-nifi-node-image)). The operator generates a login (user `admin`) in the Secret `nifi-auth-credentials`; see
[NiFi login credentials](docs/authentication.md). More examples are in `config/samples/`, and
`config/samples/nifi-full-customization.yaml` shows every field.

## Documentation

Start with the [docs index](docs/README.md):

- [Getting started](docs/getting-started/installation.md): install the operator, then
  [create a first NiFi cluster](docs/getting-started/first-steps.md), or
  [try it all on a laptop with kind](docs/getting-started/kind.md).
- [CRD reference](docs/crd-reference.md) and the generated [API reference](docs/api-reference.md).
- [Troubleshooting](docs/troubleshooting.md): error messages, conditions and diagnostics.

## Support

- Questions and bugs: [GitHub issues](https://github.com/ksolves/nifi-operator/issues). Please read
  [Troubleshooting](docs/troubleshooting.md) first and include what it asks for.
- Security problems: see [SECURITY.md](SECURITY.md), not a public issue.
- Commercial support and services: [Ksolves](https://www.ksolves.com), support@ksolves.com.

## Development

The chart is maintained by hand under `charts/nifi-operator`. Its generated CRD and RBAC
rules are embedded from `charts/nifi-operator/files/`.

**NOTE:** After changing API types or RBAC markers, run `make manifests`. It regenerates
`config/` and copies the CRD and RBAC rules into the chart (`make helm-sync`). Then
`make verify-chart` fails if anything generated is stale or uncommitted.

Before sending a change, run `make lint`, `make test`, `make lint-scripts`, `make lint-chart` and
`make verify-chart`. `make help` lists all targets.

## License

Copyright 2026 Ksolves India Limited. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Third-party licences and notices for the Go modules compiled into the operator and CSI driver are
in [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt), and in `/licenses` inside both images.

## Trademarks

Apache NiFi, NiFi, Apache and the Apache feather logo are trademarks of The Apache Software
Foundation. This project is not affiliated with or endorsed by The Apache Software Foundation.

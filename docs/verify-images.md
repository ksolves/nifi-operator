# Verifying the images

The operator runs two images of its own, from the public Docker Hub repository
`ksoperator/nifi-operator`, and the stock Apache NiFi image `apache/nifi`:

| Image | Tag (release 1.0.0) |
|---|---|
| Operator (controller manager) | `ksoperator/nifi-operator:nifi-operator-manager-1.0.0` |
| TLS CSI driver | `ksoperator/nifi-operator:nifi-operator-csi-driver-1.0.0` |
| NiFi | `apache/nifi:2.12.0` (published by the Apache NiFi project) |
| CSI sidecars | `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.18.0` and `livenessprobe:v2.20.0`, pinned by digest in the chart (published by Kubernetes SIG Storage) |

## Check the digest

Each [GitHub release](https://github.com/ksolves/nifi-operator/releases) lists the digest of
every image it ships. Compare it with what the registry serves:

```sh
docker buildx imagetools inspect ksoperator/nifi-operator:nifi-operator-manager-1.0.0 \
  --format '{{json .Manifest.Digest}}'
```

To make sure a cluster runs exactly that build, pin the digest in the chart values:

```yaml
image:
  digest: sha256:<digest from the release notes>
csiDriver:
  image:
    digest: sha256:<digest from the release notes>
```

For NiFi itself, append the digest to the tag in a NiFiCluster (`spec.image.tag:
2.12.0@sha256:<digest>`), see [Installation](installation.md#default-nifi-node-image).

## Check the build labels

The images carry OCI labels with the version and the git commit they were built from:

```sh
docker pull ksoperator/nifi-operator:nifi-operator-manager-1.0.0
docker inspect --format '{{json .Config.Labels}}' ksoperator/nifi-operator:nifi-operator-manager-1.0.0
```

`org.opencontainers.image.revision` is the commit in this repository; `org.opencontainers.image.version`
the release.

## Software bill of materials and scans

Each release attaches a CycloneDX SBOM per image (`nifi-operator-manager-<version>.cdx.json`,
`nifi-operator-csi-driver-<version>.cdx.json`). Release images are scanned with Trivy and Docker
Scout before they are pushed, and a release is not published with a fixable HIGH or CRITICAL
vulnerability in them. To scan yourself:

```sh
trivy image ksoperator/nifi-operator:nifi-operator-manager-1.0.0
trivy sbom nifi-operator-manager-1.0.0.cdx.json
```

The stock NiFi image and the two CSI sidecars are scanned and reported too, but their fixes come
from their publishers. Known findings in the sidecars at release 1.0.0 (Trivy, HIGH; no CRITICAL),
all in the Go toolchain they were built with, fixed by a later upstream build:

| Sidecar | Findings | Fixed in |
|---|---|---|
| `csi-node-driver-registrar:v2.18.0`, `livenessprobe:v2.20.0` | Go standard library 1.26.6: CVE-2026-111943, CVE-2026-78667, CVE-2026-78669, CVE-2026-97031; `golang.org/x/net` 0.58.0: CVE-2026-78669 | Go 1.26.10 or 1.27.3, `x/net` 0.60.0 |

The sidecars only talk to the CSI driver over its local socket and to the kubelet's plugin
registration socket; neither listens on the network except the liveness probe's health port
inside the pod.

## Verifying the signature

The operator and CSI driver images are signed keylessly by the Release workflow (`cosign`,
[Sigstore](https://www.sigstore.dev/)): the signature is tied to that specific GitHub Actions
run's identity, not a key that could leak. Verify with:

```sh
cosign verify ksoperator/nifi-operator:nifi-operator-manager-1.0.0 \
  --certificate-identity-regexp '^https://github.com/ksolves/nifi-operator/\.github/workflows/release\.yml@refs/tags/.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Repeat for `nifi-operator-csi-driver-1.0.0`. A successful verify also prints the certificate's
transparency log entry (Rekor), which anyone can independently look up.

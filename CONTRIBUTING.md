# Contributing

Thanks for your interest in the Ksolves Operator for Apache NiFi. Bug reports, documentation fixes and code changes
are all welcome.

## Before you start

- For a bug, open an issue with the operator version, the Kubernetes or OpenShift version, the
  NiFiCluster YAML (without secrets) and what `kubectl describe nificlusters.nifi.ksolves.com
  <name>` shows. See [Troubleshooting](docs/troubleshooting.md) first.
- For a larger change, open an issue to discuss it before writing code.
- Security problems: do not open a public issue; see [SECURITY.md](SECURITY.md).

## Development setup

You need Go (the version in `go.mod`), Docker, `make` and, for local clusters,
[kind](https://kind.sigs.k8s.io/). Tools such as `controller-gen`, `golangci-lint`, `helm` and
`crd-ref-docs` are downloaded into `bin/` by the Makefile on first use.

```sh
make test        # unit and envtest tests
make lint        # golangci-lint
make lint-scripts lint-chart
```

After changing an API type or an RBAC marker, regenerate everything and commit the result:

```sh
make generate manifests api-docs
make verify-chart   # fails if anything generated is stale or uncommitted
```

To try a change on a local cluster, build the operator image, load it into kind and install the
chart with that tag; [docs/getting-started/kind.md](docs/getting-started/kind.md) shows the kind setup.

## Pull requests

- Open pull requests against the `opensource` branch.
- Keep one change per pull request, with tests for new behaviour.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for the title
  (`fix: ...`, `feat: ...`, `docs: ...`).
- Sign off your commits (`git commit -s`) to certify the
  [Developer Certificate of Origin](https://developercertificate.org/).
- New Go files carry the license header from `hack/boilerplate.go.txt`.

## Releases (maintainers)

1. Bump the version in one pull request: `make set-version VERSION=x.y.z`, add the release to
   `CHANGELOG.md`, merge.
2. On the merged commit: `make release` builds and scans the operator and CSI driver images and
   packages the chart into `dist/`.
3. `make release-push` pushes the images and checks they are in the registry, then
   `make release-push-chart` pushes the packaged chart to Docker Hub as an OCI artifact
   (`oci://registry-1.docker.io/ksoperator/nifi-operator:x.y.z`) so it installs without GitHub.
   Both need a prior `docker login` and `helm registry login registry-1.docker.io`.
4. Tag the commit `vx.y.z` and push the tag. The Release workflow checks the commit and creates the
   GitHub Release with the chart and a source SBOM.

### Security releases

A reported or discovered vulnerability follows a different path than a normal release: who
triages it, the fix/backport flow and the advisory process are in
[docs/security-response.md](docs/security-response.md), not the steps above.

## Code of conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md).

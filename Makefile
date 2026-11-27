# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
# Keep equal to version and appVersion in charts/nifi-operator/Chart.yaml; they move together.
VERSION ?= 1.0.0

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# ksolves.com/nifi-operator-bundle:$VERSION and ksolves.com/nifi-operator-catalog:$VERSION.
IMAGE_TAG_BASE ?= ksolves.com/nifi-operator

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.3
# Image URL for the docker-build/docker-push/docker-buildx/deploy targets. Release images are built by
# `make release`; set IMG explicitly to push a dev build elsewhere.
IMG ?= nifi-operator:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	@$(MAKE) --no-print-directory helm-sync

CHART_FILES = charts/nifi-operator/files

.PHONY: helm-sync
helm-sync: ## Copy generated CRD and RBAC rules into the Helm chart (the chart embeds them via .Files.Get).
	@mkdir -p $(CHART_FILES)/crd $(CHART_FILES)/rbac
	cp config/crd/bases/nifi.ksolves.com_*.yaml $(CHART_FILES)/crd/
	cp config/rbac/role.yaml $(CHART_FILES)/rbac/

.PHONY: api-docs
api-docs: crd-ref-docs ## Generate docs/api-reference.md from the API types.
	$(CRD_REF_DOCS) --source-path=./api --config=hack/crd-ref-docs.yaml --renderer=markdown \
		--templates-dir=hack/crd-ref-docs/templates/markdown --output-path=docs/api-reference.md

.PHONY: verify-chart
verify-chart: manifests api-docs ## Fail if config/, the chart's generated files or the API reference are stale or uncommitted.
	git diff --exit-code -- config/crd config/rbac charts/ docs/api-reference.md
	@test -z "$$(git status --porcelain -- config/crd config/rbac charts/ docs/api-reference.md)" || { git status --porcelain -- config/crd config/rbac charts/ docs/api-reference.md; echo "untracked generated files: run make manifests and commit"; exit 1; }

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# Extra go test flags, e.g. `make test GOTEST_FLAGS=-race` (CI runs with -race).
GOTEST_FLAGS ?=

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $(GOTEST_FLAGS) $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

.PHONY: lint-scripts
lint-scripts: hadolint ## Lint shell scripts (shellcheck) and Dockerfiles (hadolint).
	$(SHELLCHECK) images/nifi/scripts/*.sh hack/*.sh hack/try/*.sh
	$(HADOLINT) --failure-threshold warning Dockerfile Dockerfile.csi-driver images/nifi/Dockerfile

.PHONY: lint-chart
lint-chart: helm kubeconform ## Lint the Helm chart and validate its rendered manifests against the Kubernetes schemas.
	$(HELM) lint --strict charts/nifi-operator
	@# Rendered to a file, not piped: a failing `helm template` must fail the target, and make's
	@# /bin/sh has no pipefail to stop kubeconform from passing on empty input.
	$(HELM) template nifi-operator charts/nifi-operator --namespace nifi-operator > $(LOCALBIN)/chart-rendered.yaml
	$(KUBECONFORM) -strict -summary -ignore-missing-schemas -kubernetes-version $(KUBECONFORM_K8S_VERSION) \
		$(LOCALBIN)/chart-rendered.yaml
	$(HELM) template nifi-operator charts/nifi-operator --namespace nifi-operator \
		--set networkPolicy.enabled=true > $(LOCALBIN)/chart-rendered-networkpolicy.yaml
	$(KUBECONFORM) -strict -summary -ignore-missing-schemas -kubernetes-version $(KUBECONFORM_K8S_VERSION) \
		$(LOCALBIN)/chart-rendered-networkpolicy.yaml
	HELM=$(HELM) hack/check-chart-ha.sh
	HELM=$(HELM) hack/check-chart-networkpolicy.sh

.PHONY: vulncheck
vulncheck: govulncheck ## Fail on known vulnerabilities reachable from the code.
	$(GOVULNCHECK) ./...

.PHONY: third-party-notices
third-party-notices: go-licenses ## Regenerate THIRD_PARTY_NOTICES.txt from the modules in the binaries.
	GO_LICENSES=$(GO_LICENSES) hack/third-party-notices.sh

.PHONY: license-types-check
license-types-check: go-licenses ## Fail on forbidden, restricted or unknown dependency licences.
	$(GO_LICENSES) check ./cmd/... --ignore $$(go list -m) --disallowed_types=forbidden,restricted,unknown

.PHONY: license-check
license-check: third-party-notices license-types-check ## Fail on forbidden, restricted or unknown dependency licences, or a stale THIRD_PARTY_NOTICES.txt.
	git diff --exit-code -- THIRD_PARTY_NOTICES.txt
	@test -z "$$(git status --porcelain -- THIRD_PARTY_NOTICES.txt)" || { echo "THIRD_PARTY_NOTICES.txt is new or stale: run make third-party-notices and commit"; exit 1; }

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name nifi-operator-builder
	$(CONTAINER_TOOL) buildx use nifi-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm nifi-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image $(RELEASE_REPO)=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Release

# Release images: the operator and the CSI driver, in one repository with
# component-prefixed tags, the names the chart derives from its appVersion. NiFi itself runs from
# the stock apache/nifi image. A custom node image (images/nifi, docker-build-nifi) is
# optional and not part of a release; it gets an immutable per-release tag besides the bare
# nifi-node-<NiFi version> tag, which is rebuilt in place.
RELEASE_REPO ?= ksoperator/nifi-operator
NIFI_VERSION ?= 2.12.0
RELEASE_PLATFORM ?= linux/amd64
OPERATOR_IMG ?= $(RELEASE_REPO):nifi-operator-manager-$(VERSION)
CSI_IMG ?= $(RELEASE_REPO):nifi-operator-csi-driver-$(VERSION)
NIFI_NODE_IMG ?= $(RELEASE_REPO):nifi-node-$(NIFI_VERSION)-$(VERSION)
NIFI_NODE_FLOATING_IMG ?= $(RELEASE_REPO):nifi-node-$(NIFI_VERSION)
# The NiFi image a NiFiCluster runs by default; the release scan reports its CVEs.
NIFI_STOCK_IMG ?= apache/nifi:$(NIFI_VERSION)
RELEASE_IMAGES = $(OPERATOR_IMG) $(CSI_IMG)
# The chart is pushed to Docker Hub as an OCI artifact so it installs without GitHub. Helm
# appends the chart name (nifi-operator), so it lands in the same repo as the images, under
# the plain $(VERSION) tag (images use nifi-operator-manager-$(VERSION) etc., so no clash).
CHART_REGISTRY ?= registry-1.docker.io
CHART_OCI_REPO ?= oci://$(CHART_REGISTRY)/$(patsubst %/,%,$(dir $(RELEASE_REPO)))
CHART_TGZ = $(DIST)/nifi-operator-$(VERSION).tgz
# The upstream CSI sidecars the chart runs next to the driver, as pinned in its values.
CSI_SIDECAR_IMAGES = $(shell sed -n 's|^ *image: \(registry\.k8s\.io/sig-storage/[^ ]*\)$$|\1|p' charts/nifi-operator/values.yaml)
# OCI label values baked into the release images (Dockerfile ARGs VERSION, REVISION).
IMAGE_LABEL_ARGS = --build-arg VERSION=$(VERSION) --build-arg REVISION=$(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DIST ?= dist
# the NiFi image is ~2 GB; trivy's 5m default is not enough for it
TRIVY_TIMEOUT ?= 20m

.PHONY: release-check-version
release-check-version: ## Fail unless VERSION matches the chart's version and appVersion and the image tags in config/.
	@stale=$$({ grep -Hn 'nifi-operator-manager-' config/manager/manager.yaml; \
		grep -Hn 'nifi-operator-csi-driver-' config/csi/daemonset.yaml; } | grep -v -- '-$(VERSION)$$' || true); \
	if [ -n "$$stale" ]; then \
		echo "$$stale"; echo "these image tags do not end in -$(VERSION); run: make set-version VERSION=$(VERSION)"; exit 1; \
	fi
	@chart=$$(sed -n 's/^version: *//p' charts/nifi-operator/Chart.yaml); \
	app=$$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/p' charts/nifi-operator/Chart.yaml); \
	if [ "$$chart" != "$(VERSION)" ] || [ "$$app" != "$(VERSION)" ]; then \
		echo "VERSION=$(VERSION) but Chart.yaml has version=$$chart appVersion=$$app"; exit 1; \
	fi; echo "version $(VERSION) consistent"

.PHONY: print-release-images
print-release-images: ## Print the operator and CSI driver image references for VERSION, one per line.
	@for img in $(RELEASE_IMAGES); do echo "$$img"; done

.PHONY: set-version
set-version: ## Set VERSION everywhere: make set-version VERSION=x.y.z
	@[ "$(origin VERSION)" = "command line" ] || { echo "set-version: pass the new version: make set-version VERSION=x.y.z"; exit 1; }
	@echo '$(VERSION)' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "set-version: '$(VERSION)' is not a semantic version"; exit 1; }
	sed -i 's/^VERSION ?= .*/VERSION ?= $(VERSION)/' Makefile
	sed -i -e 's/^version: .*/version: $(VERSION)/' -e 's/^appVersion: .*/appVersion: "$(VERSION)"/' charts/nifi-operator/Chart.yaml
	sed -i 's/\(nifi-operator-manager-\)[^"[:space:]]*/\1$(VERSION)/' config/manager/manager.yaml
	sed -i 's/\(nifi-operator-csi-driver-\)[^"[:space:]]*/\1$(VERSION)/' config/csi/daemonset.yaml

.PHONY: docker-build-csi
docker-build-csi: ## Build the CSI driver image (CSI_IMG).
	$(CONTAINER_TOOL) build --platform $(RELEASE_PLATFORM) $(IMAGE_LABEL_ARGS) -t $(CSI_IMG) -f Dockerfile.csi-driver .

.PHONY: docker-build-nifi
docker-build-nifi: ## Build an optional custom NiFi node image (NIFI_NODE_IMG, also tagged NIFI_NODE_FLOATING_IMG).
	$(CONTAINER_TOOL) build --platform $(RELEASE_PLATFORM) -t $(NIFI_NODE_IMG) -t $(NIFI_NODE_FLOATING_IMG) images/nifi

# The other NiFi lines the operator runs (docs/compatibility.md): the latest patch of
# each minor from 1.14 on. NIFI_VERSION's own image is built by docker-build-nifi above.
NIFI_OTHER_VERSIONS ?= 1.14.0 1.15.3 1.16.3 1.17.0 1.18.0 1.19.1 1.20.0 1.21.0 1.22.0 1.23.2 1.24.0 \
	1.25.0 1.26.0 1.27.0 1.28.1 2.0.0 2.1.0 2.2.0 2.3.0 2.4.0 2.5.0 2.6.0 2.7.2 2.8.0 2.9.0 2.10.0 2.11.0

# A Debian mirror for the node image builds (images/nifi/Dockerfile DEBIAN_MIRROR); empty keeps
# deb.debian.org.
DEBIAN_MIRROR ?=

.PHONY: docker-build-nifi-version
docker-build-nifi-version: ## Build the node image of one other NiFi line: make docker-build-nifi-version NIFI_OTHER=1.28.1
	@[ -n "$(NIFI_OTHER)" ] || { echo "docker-build-nifi-version: pass NIFI_OTHER=x.y.z"; exit 1; }
	$(CONTAINER_TOOL) build --platform $(RELEASE_PLATFORM) --build-arg NIFI_IMAGE=apache/nifi:$(NIFI_OTHER) --build-arg OS_UPGRADE=best-effort --build-arg DEBIAN_MIRROR=$(DEBIAN_MIRROR) \
		-t $(RELEASE_REPO):nifi-node-$(NIFI_OTHER)-$(VERSION) -t $(RELEASE_REPO):nifi-node-$(NIFI_OTHER) images/nifi

# One line at a time, removed locally once pushed: each node image is about 3 GB, too many to keep.
# The build cache goes too: each build leaves about 1.5 GB of it (the chgrp layer).
.PHONY: release-push-nifi-versions
release-push-nifi-versions: release-check-version ## Build, push and verify the node image of every NIFI_OTHER_VERSIONS line.
	@set -e; for v in $(NIFI_OTHER_VERSIONS); do \
		$(MAKE) --no-print-directory docker-build-nifi-version NIFI_OTHER=$$v; \
		for img in $(RELEASE_REPO):nifi-node-$$v-$(VERSION) $(RELEASE_REPO):nifi-node-$$v; do \
			$(CONTAINER_TOOL) push $$img; \
			$(CONTAINER_TOOL) manifest inspect $$img >/dev/null 2>&1 || { echo "$$img is NOT in the registry"; exit 1; }; \
			echo "release-push-nifi-versions: $$img is in the registry"; \
		done; \
		$(CONTAINER_TOOL) rmi $(RELEASE_REPO):nifi-node-$$v-$(VERSION) $(RELEASE_REPO):nifi-node-$$v apache/nifi:$$v >/dev/null; \
		$(CONTAINER_TOOL) builder prune -f >/dev/null; \
	done

# The images carry HEAD as their revision label, so HEAD must be clean and pushed.
.PHONY: release-check-tree
release-check-tree: ## Fail unless the tree is clean and HEAD is on a remote branch.
	@git diff --quiet HEAD -- || { echo "release-check-tree: uncommitted changes; commit and push first"; exit 1; }
	@[ -n "$$(git branch -r --contains HEAD)" ] || { echo "release-check-tree: HEAD $$(git rev-parse --short HEAD) is on no remote branch; push it first"; exit 1; }

.PHONY: release-images
release-images: release-check-version release-check-tree docker-build-csi ## Build the release images (operator, CSI driver).
	rm -f $(DIST)/scan/.passed-$(VERSION)
	$(CONTAINER_TOOL) build --platform $(RELEASE_PLATFORM) $(IMAGE_LABEL_ARGS) -t $(OPERATOR_IMG) .

# Operator and CSI images fail the scan on fixable HIGH/CRITICAL CVEs. The stock NiFi image and
# the upstream CSI sidecars are only reported: their CVEs are fixed upstream (Apache, Kubernetes
# SIG Storage) and are tracked separately. Trivy and Docker
# Scout both run, because Trivy misses CVEs inside shaded jars that Scout catches.
.PHONY: release-scan
release-scan: trivy ## Scan the release images with Trivy and Docker Scout; reports in $(DIST)/scan, SBOMs in $(DIST)/sbom.
	@mkdir -p $(DIST)/scan $(DIST)/sbom
	@rm -f $(DIST)/scan/.passed-$(VERSION)
	@set -e; for img in $(OPERATOR_IMG) $(CSI_IMG); do \
		f=$(DIST)/scan/$$(echo $$img | sed 's#.*:##'); \
		$(TRIVY) image --quiet --timeout $(TRIVY_TIMEOUT) --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 \
			--output $$f.trivy.txt $$img \
			|| { echo "fixable HIGH/CRITICAL CVEs in $$img, see $$f.trivy.txt"; exit 1; }; \
		$(CONTAINER_TOOL) scout cves --only-fixed --only-severity critical,high --exit-code --output $$f.scout.txt $$img \
			|| { echo "fixable HIGH/CRITICAL CVEs in $$img (Docker Scout), see $$f.scout.txt"; exit 1; }; \
	done
	@f=$(DIST)/scan/nifi-$(NIFI_VERSION); \
	$(TRIVY) image --quiet --timeout $(TRIVY_TIMEOUT) --severity HIGH,CRITICAL --output $$f.trivy.txt $(NIFI_STOCK_IMG) \
		|| echo "WARNING: trivy could not scan $(NIFI_STOCK_IMG); see above"; \
	$(CONTAINER_TOOL) scout cves --only-severity critical,high --output $$f.scout.txt $(NIFI_STOCK_IMG) || true
	@[ -n "$(CSI_SIDECAR_IMAGES)" ] || { echo "no CSI sidecar images found in charts/nifi-operator/values.yaml"; exit 1; }
	@for img in $(CSI_SIDECAR_IMAGES); do \
		f=$(DIST)/scan/sidecar-$$(echo $${img%%@*} | sed 's#.*/##; s#:#-#'); \
		$(TRIVY) image --quiet --timeout $(TRIVY_TIMEOUT) --severity HIGH,CRITICAL --output $$f.trivy.txt $$img \
			|| echo "WARNING: trivy could not scan $$img; see above"; \
		$(CONTAINER_TOOL) scout cves --only-severity critical,high --output $$f.scout.txt $$img || true; \
	done
	@set -e; for img in $(RELEASE_IMAGES); do \
		$(TRIVY) image --quiet --timeout $(TRIVY_TIMEOUT) --format cyclonedx -o $(DIST)/sbom/$${img##*:}.cdx.json $$img; \
	done
	@$(CONTAINER_TOOL) image inspect --format '{{.Id}}' $(RELEASE_IMAGES) > $(DIST)/scan/.passed-$(VERSION)
	@echo "scan reports in $(DIST)/scan, SBOMs in $(DIST)/sbom"

.PHONY: release-chart
release-chart: release-check-version helm ## Package the Helm chart into $(DIST).
	@mkdir -p $(DIST)
	$(HELM) package charts/nifi-operator --destination $(DIST)

.PHONY: release
release: release-images release-scan release-chart ## Build, scan and package a release locally. Pushes nothing.
	@echo "Release $(VERSION) built and scanned. Review $(DIST)/scan, then: make release-push release-push-chart"

.PHONY: release-push
release-push: release-check-version ## Push the release images (run after `make release`).
	@[ -f $(DIST)/scan/.passed-$(VERSION) ] || { echo "release-push: no passing scan for $(VERSION); run make release first"; exit 1; }
	@[ "$$($(CONTAINER_TOOL) image inspect --format '{{.Id}}' $(RELEASE_IMAGES))" = "$$(cat $(DIST)/scan/.passed-$(VERSION))" ] \
		|| { echo "release-push: images changed since the scan; run make release again"; exit 1; }
	@for img in $(RELEASE_IMAGES); do $(CONTAINER_TOOL) push $$img || exit 1; done
	@# Read every tag back from the registry: a release whose image is missing is not installable.
	@for img in $(RELEASE_IMAGES); do \
		$(CONTAINER_TOOL) manifest inspect $$img >/dev/null 2>&1 || { echo "release-push: $$img is NOT in the registry"; exit 1; }; \
		echo "release-push: $$img is in the registry"; \
	done

.PHONY: release-push-chart
release-push-chart: release-check-version helm ## Push the packaged Helm chart to Docker Hub as an OCI artifact (run after `make release`).
	@[ -f $(CHART_TGZ) ] || { echo "release-push-chart: $(CHART_TGZ) not found; run make release first"; exit 1; }
	$(HELM) push $(CHART_TGZ) $(CHART_OCI_REPO)
	@# Read the chart back from the registry: a release whose chart is missing is not installable.
	@$(HELM) show chart $(CHART_OCI_REPO)/nifi-operator --version $(VERSION) >/dev/null 2>&1 \
		|| { echo "release-push-chart: $(CHART_OCI_REPO)/nifi-operator:$(VERSION) is NOT in the registry"; exit 1; }
	@echo "release-push-chart: chart $(CHART_OCI_REPO)/nifi-operator:$(VERSION) is in the registry"

.PHONY: release-source-sbom
release-source-sbom: trivy ## Write a CycloneDX SBOM of the source tree to $(DIST) (used by CI).
	@mkdir -p $(DIST)
	$(TRIVY) fs --skip-dirs bin --skip-dirs $(DIST) --format cyclonedx -o $(DIST)/nifi-operator-$(VERSION)-source.cdx.json .

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image $(RELEASE_REPO)=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOVULNCHECK ?= $(LOCALBIN)/govulncheck
GO_LICENSES ?= $(LOCALBIN)/go-licenses
GINKGO ?= $(LOCALBIN)/ginkgo
HELM ?= $(LOCALBIN)/helm
KUBECONFORM ?= $(LOCALBIN)/kubeconform
HADOLINT ?= $(LOCALBIN)/hadolint
TRIVY ?= $(LOCALBIN)/trivy
SHELLCHECK ?= shellcheck

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
CRD_REF_DOCS_VERSION ?= v0.3.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION ?= v1.8.0
GO_LICENSES_VERSION ?= v1.6.0
# The ginkgo CLI must match the github.com/onsi/ginkgo/v2 version in go.mod.
GINKGO_VERSION ?= $(shell go list -m -f "{{ .Version }}" github.com/onsi/ginkgo/v2)
HELM_VERSION ?= v4.3.0
KUBECONFORM_VERSION ?= v0.8.0
HADOLINT_VERSION ?= v2.15.1
# sha256 of hadolint-linux-x86_64 for HADOLINT_VERSION (matches the digest GitHub publishes for the asset).
HADOLINT_SHA256 ?= c7187db94eeeeca956519a6af171adc31453941a1e777961f6e680f697c8c507
TRIVY_VERSION ?= 0.75.0
# Schema version the rendered chart is validated against: OKD 4.19 runs Kubernetes 1.32.
KUBECONFORM_K8S_VERSION ?= 1.32.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: govulncheck
.PHONY: ginkgo
ginkgo: $(GINKGO) ## Download the ginkgo CLI locally if necessary (parallel E2E runs).
$(GINKGO): $(LOCALBIN)
	$(call go-install-tool,$(GINKGO),github.com/onsi/ginkgo/v2/ginkgo,$(GINKGO_VERSION))

govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: go-licenses
go-licenses: $(GO_LICENSES) ## Download go-licenses locally if necessary.
$(GO_LICENSES): $(LOCALBIN)
	$(call go-install-tool,$(GO_LICENSES),github.com/google/go-licenses,$(GO_LICENSES_VERSION))

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	$(call go-install-tool,$(HELM),helm.sh/helm/v4/cmd/helm,$(HELM_VERSION))

.PHONY: kubeconform
kubeconform: $(KUBECONFORM) ## Download kubeconform locally if necessary.
$(KUBECONFORM): $(LOCALBIN)
	$(call go-install-tool,$(KUBECONFORM),github.com/yannh/kubeconform/cmd/kubeconform,$(KUBECONFORM_VERSION))

.PHONY: hadolint
hadolint: $(HADOLINT) ## Download hadolint locally if necessary (linux/amd64 release binary, checksum-verified).
$(HADOLINT): $(LOCALBIN)
	@[ -f "$(HADOLINT)-$(HADOLINT_VERSION)" ] || { \
	set -e; \
	echo "Downloading hadolint $(HADOLINT_VERSION)"; \
	curl -sSfL -o "$(HADOLINT)-$(HADOLINT_VERSION).tmp" \
		https://github.com/hadolint/hadolint/releases/download/$(HADOLINT_VERSION)/hadolint-linux-x86_64; \
	echo "$(HADOLINT_SHA256)  $(HADOLINT)-$(HADOLINT_VERSION).tmp" | sha256sum -c -; \
	chmod +x "$(HADOLINT)-$(HADOLINT_VERSION).tmp"; \
	mv "$(HADOLINT)-$(HADOLINT_VERSION).tmp" "$(HADOLINT)-$(HADOLINT_VERSION)"; \
	} ;\
	ln -sf $(HADOLINT)-$(HADOLINT_VERSION) $(HADOLINT)

.PHONY: trivy
trivy: $(TRIVY) ## Download trivy locally if necessary (linux/amd64 release tarball, checked against the release's checksums file).
$(TRIVY): $(LOCALBIN)
	@[ -f "$(TRIVY)-v$(TRIVY_VERSION)" ] || { \
	set -e; \
	echo "Downloading trivy $(TRIVY_VERSION)"; \
	tmp=$$(mktemp -d); \
	curl -sSfL -o "$$tmp/trivy_$(TRIVY_VERSION)_Linux-64bit.tar.gz" \
		https://github.com/aquasecurity/trivy/releases/download/v$(TRIVY_VERSION)/trivy_$(TRIVY_VERSION)_Linux-64bit.tar.gz; \
	curl -sSfL -o "$$tmp/checksums.txt" \
		https://github.com/aquasecurity/trivy/releases/download/v$(TRIVY_VERSION)/trivy_$(TRIVY_VERSION)_checksums.txt; \
	(cd "$$tmp" && grep ' trivy_$(TRIVY_VERSION)_Linux-64bit.tar.gz$$' checksums.txt | sha256sum -c -); \
	tar -xzf "$$tmp/trivy_$(TRIVY_VERSION)_Linux-64bit.tar.gz" -C "$$tmp" trivy; \
	mv "$$tmp/trivy" "$(TRIVY)-v$(TRIVY_VERSION)"; \
	rm -rf "$$tmp"; \
	} ;\
	ln -sf $(TRIVY)-v$(TRIVY_VERSION) $(TRIVY)

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image $(RELEASE_REPO)=$(IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(CONTAINER_TOOL) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)

#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Prepares a laptop to try the NiFi operator: installs what is missing (Docker, kind, kubectl,
# helm), then creates a one-node Kubernetes cluster with kind and loads the operator's images into
# it. Installing the operator and NiFi is up to you: docs/getting-started/kind.md, from step 2.
#
#   hack/try/kind.sh            # prepare (safe to run again)
#   hack/try/kind.sh delete     # delete the cluster (the tools stay)
#
# Settings (environment variables):
#   UI_PORT     port on this machine for the NiFi UI   (8443; set when the cluster is created)
#   CLUSTER     kind cluster name                      (nifi-try)
#   BIN_DIR     where the tools are installed          ($HOME/.local/bin)
set -euo pipefail

KIND_VERSION=v0.29.0                     # its default node image is the one below
KUBECTL_VERSION=v1.33.1
HELM_VERSION=v3.16.4
# Kubernetes 1.33 (the operator needs 1.32+). Older kind releases crash on this image's containerd.
NODE_IMAGE=kindest/node:v1.33.1@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f
CLUSTER="${CLUSTER:-nifi-try}"
UI_PORT="${UI_PORT:-8443}"
UI_NODE_PORT=30443                      # docs/getting-started/nifi-ui-service.yaml
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGES_REPO=ksoperator/nifi-operator
VERSION="$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$REPO_ROOT/charts/nifi-operator/Chart.yaml")"
# The CSI driver's sidecars come from registry.k8s.io; behind a slow proxy the node takes so long to
# pull them that the CSI driver keeps failing its liveness probe. Loaded up front like the rest.
SIDECARS=()
while IFS= read -r img; do SIDECARS+=("$img"); done < <(
  sed -n 's/^ *image: *\(registry\.k8s\.io\/[^ #]*\).*/\1/p' "$REPO_ROOT/charts/nifi-operator/values.yaml")
IMAGES=(
  "$IMAGES_REPO:nifi-operator-manager-$VERSION"
  "$IMAGES_REPO:nifi-operator-csi-driver-$VERSION"
  "apache/nifi:2.12.0"
  "${SIDECARS[@]}"
)

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
fail() { printf '\n\033[31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
# No stall timeout: a proxy that scans downloads sends nothing until it has scanned the whole file.
CURL_OPTS=(-fL --retry 5 --retry-delay 3 --connect-timeout 20)
fetch() { curl -s -S "${CURL_OPTS[@]}" "$@"; }                     # small files: quiet
fetch_big() { curl --progress-bar "${CURL_OPTS[@]}" "$@"; }         # binaries: show progress
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

if [ "${1:-}" = delete ]; then
  PATH="$BIN_DIR:$PATH" kind delete cluster --name "$CLUSTER"
  exit 0
fi
[ $# -eq 0 ] || fail "unknown argument '$1'; use no argument, or 'delete'"

case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) fail "this script supports Linux and macOS" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  arm64 | aarch64) ARCH=arm64
    echo "Warning: the NiFi images are built for x86_64 only; on this machine they run emulated and slowly." ;;
  *) fail "unsupported CPU architecture $(uname -m)" ;;
esac
mkdir -p "$BIN_DIR"
PATH_BEFORE="$PATH"
export PATH="$BIN_DIR:$PATH"

# install_tool NAME URL CHECKSUM_URL: downloads one binary into BIN_DIR, after checking it against
# the checksum its project publishes.
install_tool() {
  local name=$1 url=$2 sum_url=$3 tmp want got
  tmp=$(mktemp -d)
  echo "installing $name into $BIN_DIR (behind a company proxy that scans downloads, the progress"
  echo "can stay at 0% for a few minutes and then jump to 100%; that is normal)"
  fetch_big -o "$tmp/download" "$url" || fail "could not download $url; check your internet connection and run this again"
  want=$(fetch "$sum_url" | awk '{print $1}') || true
  [ -n "$want" ] || fail "could not download the checksum $sum_url; try again"
  got=$(sha256 "$tmp/download")
  [ "$want" = "$got" ] || fail "checksum mismatch for $url (expected $want, got $got); try again"
  case "$url" in
    *.tar.gz) tar -xzf "$tmp/download" -C "$tmp" && mv "$tmp/$OS-$ARCH/$name" "$BIN_DIR/$name" ;;
    *) mv "$tmp/download" "$BIN_DIR/$name" ;;
  esac
  chmod +x "$BIN_DIR/$name"
  rm -rf "$tmp"
}

step "Docker (with containerd)"
if ! command -v docker >/dev/null; then
  if [ "$OS" = darwin ]; then
    fail "Docker is not installed. Install Docker Desktop (https://docs.docker.com/desktop/setup/install/mac-install/),
give it at least 4 GB of memory (Settings > Resources), start it, and run this script again."
  fi
  printf 'Docker is not installed. Install Docker Engine now with get.docker.com (uses sudo)? [y/N] '
  read -r answer
  case "$answer" in y | Y | yes) ;; *) fail "Docker is needed: https://docs.docker.com/engine/install/" ;; esac
  fetch https://get.docker.com -o /tmp/get-docker.sh
  sudo sh /tmp/get-docker.sh
  sudo usermod -aG docker "$USER"
  echo "Docker is installed. Log out and back in (so your user may use Docker), then run this script again."
  exit 0
fi
if ! docker info >/dev/null 2>&1; then
  if [ "$OS" = linux ] && [ -S /var/run/docker.sock ] && ! [ -w /var/run/docker.sock ]; then
    fail "your user may not use Docker yet: run 'sudo usermod -aG docker $USER', log out and back in, and run this again"
  fi
  fail "Docker is installed but not running. Start it (Linux: 'sudo systemctl start docker'; macOS: open Docker Desktop) and run this again."
fi
docker --version
mem_gib=$(( $(docker info --format '{{.MemTotal}}') / 1024 / 1024 / 1024 ))
[ "$mem_gib" -ge 4 ] || echo "Warning: Docker has ${mem_gib} GiB of memory; one NiFi node needs about 4 GiB in all."

step "kind, kubectl and helm"
# kind must be new enough for the node image (older releases crash its containerd).
kind_minor=$(kind version 2>/dev/null | sed -n 's/^kind v0\.\([0-9]*\).*/\1/p' || true)
if [ "${kind_minor:-0}" -ge 29 ]; then kind version; else
  [ -z "$kind_minor" ] || echo "kind v0.$kind_minor is too old for Kubernetes 1.33; installing $KIND_VERSION"
  install_tool kind "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-$OS-$ARCH" \
    "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-$OS-$ARCH.sha256sum"
fi
# kubectl must be within one minor version of Kubernetes 1.33.
kubectl_minor=$(kubectl version --client -o json 2>/dev/null | sed -n 's/.*"minor": *"\([0-9]*\).*/\1/p' | head -n1 || true)
if [ "${kubectl_minor:-0}" -ge 32 ]; then kubectl version --client 2>/dev/null | head -n1; else
  [ -z "$kubectl_minor" ] || echo "kubectl 1.$kubectl_minor is too old for Kubernetes 1.33; installing $KUBECTL_VERSION"
  if [ "$OS" = linux ] && [ "$ARCH" = amd64 ]; then
    # The kind node image carries the kubectl of its Kubernetes version: no download needed.
    echo "installing kubectl $KUBECTL_VERSION into $BIN_DIR, from the Kubernetes node image"
    docker image inspect "$NODE_IMAGE" >/dev/null 2>&1 || docker pull "$NODE_IMAGE" >/dev/null
    node_box=$(docker create "$NODE_IMAGE")
    docker cp "$node_box:/usr/bin/kubectl" "$BIN_DIR/kubectl" >/dev/null
    docker rm "$node_box" >/dev/null
    chmod +x "$BIN_DIR/kubectl"
  else
    install_tool kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$OS/$ARCH/kubectl" \
      "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$OS/$ARCH/kubectl.sha256"
  fi
fi
if command -v helm >/dev/null; then helm version --short; else
  install_tool helm "https://get.helm.sh/helm-$HELM_VERSION-$OS-$ARCH.tar.gz" \
    "https://get.helm.sh/helm-$HELM_VERSION-$OS-$ARCH.tar.gz.sha256sum"
fi

step "The images (operator release $VERSION and its CSI sidecars)"
for img in "${IMAGES[@]}"; do
  docker image inspect "$img" >/dev/null 2>&1 && continue
  echo "pulling $img"
  docker pull -q "$img" >/dev/null || fail "cannot pull $img; check your internet connection and run this again"
done

step "The Kubernetes cluster '$CLUSTER'"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  mapped=$(docker port "$CLUSTER-control-plane" "$UI_NODE_PORT/tcp" 2>/dev/null | head -n1 | sed 's/.*://')
  [ -n "$mapped" ] ||
    fail "the cluster '$CLUSTER' was not created by this script; delete it ('hack/try/kind.sh delete') and run this again"
  UI_PORT="$mapped"
  echo "already exists, reusing it"
else
  if (exec 3<>"/dev/tcp/127.0.0.1/$UI_PORT") 2>/dev/null; then
    fail "port $UI_PORT is already in use on this machine; pick another one: UI_PORT=9443 $0"
  fi
  # Port UI_PORT on this machine leads to NodePort UI_NODE_PORT inside: the NiFi UI (step 4).
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: $UI_NODE_PORT
        hostPort: $UI_PORT
        listenAddress: 127.0.0.1
EOF
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# On some hosts the node's containerd crashes once in its first minute and is restarted at once.
# Loading images or creating pods in that moment fails, so wait until the node is Ready and
# containerd has been up for a minute.
echo "waiting for the node to settle"
kubectl wait --for=condition=Ready nodes --all --timeout=180s >/dev/null
for _ in $(seq 1 30); do
  up=$(docker exec "$CLUSTER-control-plane" sh -c \
    'echo $(( $(date +%s) - $(date -d "$(systemctl show containerd -p ActiveEnterTimestamp --value)" +%s) ))' \
    2>/dev/null || echo 0)
  [ "${up:-0}" -ge 60 ] && break
  sleep 5
done

for img in "${IMAGES[@]}"; do
  case "$img" in */*/*) ref="$img" ;; *) ref="docker.io/$img" ;; esac
  # Images pinned as name:tag@digest are also needed under the bare name@digest (see below).
  bare_digest_ref=""
  case "$ref" in *@*) bare_digest_ref="${ref%@*}"; bare_digest_ref="${bare_digest_ref%:*}@${ref#*@}" ;; esac
  if docker exec "$CLUSTER-control-plane" crictl inspecti "$ref" >/dev/null 2>&1 &&
    { [ -z "$bare_digest_ref" ] ||
      docker exec "$CLUSTER-control-plane" crictl inspecti "$bare_digest_ref" >/dev/null 2>&1; }; then
    continue
  fi
  echo "loading $img into the cluster (the NiFi image is large: about a minute)"
  # An image pinned as name:tag@digest (the CSI sidecars, pinned for CVE scanning) has no name
  # docker save can write, so containerd would import it as a synthetic "import-<date>@<digest>".
  # Kubelet then fails to create its container ('image "docker.io/library/import-...": not
  # found': CRI looks the alias up under a normalized name it was never stored under). Load it
  # under its name:tag instead and add the digest references below.
  load="$img"
  case "$img" in
    *@*) load="${img%@*}"; docker tag "$img" "$load" || fail "could not tag $img as $load" ;;
  esac
  # kind load fails on some kind/node-image pairs ("failed to detect containerd snapshotter");
  # importing into the node's containerd directly works with any of them.
  loaded=false
  for _ in 1 2 3; do
    if kind load docker-image --name "$CLUSTER" "$load" >/dev/null 2>&1 ||
      docker save "$load" | docker exec -i "$CLUSTER-control-plane" \
        ctr -n k8s.io images import --digests --snapshotter overlayfs - >/dev/null 2>&1; then
      loaded=true
      break
    fi
    echo "retrying in 10 seconds"
    sleep 10
  done
  $loaded || fail "could not load $img into the cluster; run this script again"
  [ -n "$bare_digest_ref" ] || continue
  # Loaded as name:tag: add $ref (name:tag@digest) and, since kubelet resolves a digest-pinned
  # pull against the bare "name@digest" form even when the pod spec also carries a tag, that form
  # too. The digest is the registry's multi-arch index, not the single-platform manifest docker
  # save wrote, but kubelet only looks the reference up by name.
  loaded_ref="${ref%@*}"
  docker exec "$CLUSTER-control-plane" ctr -n k8s.io images tag --force "$loaded_ref" "$ref" >/dev/null ||
    fail "loaded $img but it is not present under $loaded_ref; run this script again"
  docker exec "$CLUSTER-control-plane" ctr -n k8s.io images tag --force "$loaded_ref" "$bare_digest_ref" >/dev/null ||
    fail "could not tag $loaded_ref as $bare_digest_ref; run this script again"
done
kubectl get nodes

path_note=""
case ":$PATH_BEFORE:" in *":$BIN_DIR:"*) ;; *) path_note="
The tools are in $BIN_DIR. Add it to your PATH (and to ~/.bashrc or ~/.zshrc):
  export PATH=\"$BIN_DIR:\$PATH\"
" ;; esac
cat <<EOF

$(printf '\033[1;32m')The cluster is ready.$(printf '\033[0m') kubectl now talks to it (context kind-$CLUSTER).
$path_note
Next: docs/getting-started/kind.md, step 2: install the operator.
The NiFi UI will be at https://localhost:$UI_PORT/nifi/ once you reach step 4.
EOF

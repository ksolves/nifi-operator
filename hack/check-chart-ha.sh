#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Checks the chart's operator high-availability defaults on rendered output:
# two replicas, a PodDisruptionBudget only when there is more than one, the lease flags, and the
# default anti-affinity that a user-supplied affinity replaces. Run by `make lint-chart`.
set -euo pipefail

HELM="${HELM:-helm}"
CHART="${1:-charts/nifi-operator}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Only the operator's own Deployment and PDB: other manifests (the CRD's field descriptions)
# mention the same keywords.
render() {
  "$HELM" template nifi-operator "$CHART" --namespace nifi-operator \
    --show-only templates/deployment.yaml --show-only templates/pdb.yaml "$@" 2>/dev/null || true
}
fail() { echo "check-chart-ha: $*" >&2; exit 1; }

render > "$tmp/default.yaml"
grep -q '^kind: PodDisruptionBudget$' "$tmp/default.yaml" || fail "no PodDisruptionBudget by default"
grep -q '^  minAvailable: 1$' "$tmp/default.yaml" || fail "PodDisruptionBudget minAvailable is not 1"
grep -q '^  replicas: 2$' "$tmp/default.yaml" || fail "operator does not default to 2 replicas"
for flag in --leader-elect --leader-election-lease-duration=15s --leader-election-renew-deadline=10s \
  --leader-election-retry-period=2s; do
  grep -q -- "- $flag\$" "$tmp/default.yaml" || fail "missing manager arg $flag"
done
grep -q 'podAntiAffinity:' "$tmp/default.yaml" || fail "no default pod anti-affinity"
grep -q 'seconds: 5$' "$tmp/default.yaml" || fail "no preStop sleep before the webhook server stops"

"$HELM" template nifi-operator "$CHART" >/dev/null || fail "chart does not render"
render --set replicaCount=1 > "$tmp/single.yaml"
if grep -q '^kind: PodDisruptionBudget$' "$tmp/single.yaml"; then
  fail "PodDisruptionBudget rendered for a single replica (it would block every drain)"
fi

render --set 'affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=role' \
  --set 'affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists' \
  > "$tmp/affinity.yaml"
grep -q 'nodeAffinity:' "$tmp/affinity.yaml" || fail "user affinity not rendered"
if grep -q 'podAntiAffinity:' "$tmp/affinity.yaml"; then
  fail "default anti-affinity kept next to a user-supplied affinity"
fi

# Webhook resilience: a timeout, no namespace scoping, monitoring only on request.
"$HELM" template nifi-operator "$CHART" --namespace nifi-operator --show-only templates/webhook.yaml > "$tmp/webhook.yaml"
# Two webhooks (mutating, validating) for each of NiFiCluster and NiFiNodeGroup, plus the
# validating-only NiFiNodeGroup /scale subresource webhook (NIFI-248 B1(b)).
[ "$(grep -c 'timeoutSeconds: 5$' "$tmp/webhook.yaml")" = 5 ] || fail "every webhook needs timeoutSeconds: 5"
if grep -q 'namespaceSelector:' "$tmp/webhook.yaml"; then fail "cluster-wide install must not scope its webhooks"; fi
"$HELM" template nifi-operator "$CHART" --set webhook.certManager.enabled=true \
  --show-only templates/webhook-certificate.yaml > "$tmp/certs.yaml"
grep -q 'isCA: true' "$tmp/certs.yaml" || fail "cert-manager path must issue from a CA, so renewals keep the caBundle"
if "$HELM" template nifi-operator "$CHART" | grep -qE '^kind: (ServiceMonitor|PrometheusRule)$'; then
  fail "ServiceMonitor rendered without being enabled"
fi
# The chart ships no alert rules; the removed value must not be dropped silently.
if "$HELM" template nifi-operator "$CHART" --set monitoring.prometheusRule.enabled=true > /dev/null 2>&1; then
  fail "monitoring.prometheusRule must fail the render"
fi
# Namespace-scoped installs are gone; their values must not silently install cluster-wide.
for v in rbac.clusterWide=false watchNamespace=nifi-apps; do
  if "$HELM" template nifi-operator "$CHART" --set "$v" > /dev/null 2>&1; then
    fail "$v must fail the render"
  fi
done
# The pre-release builds' defaults still render, so `helm upgrade --reuse-values` works.
"$HELM" template nifi-operator "$CHART" --set rbac.clusterWide=true --set-string watchNamespace= \
  --set monitoring.prometheusRule.enabled=false > /dev/null ||
  fail "the pre-release default values must still render"
grep -q '^kind: ClusterRole$' <("$HELM" template nifi-operator "$CHART" --show-only templates/rbac.yaml) ||
  fail "the operator's rules must be a ClusterRole"

# The SCC rule and --nifi-scc follow nifiPods.openshiftSCC alone: a plain helm template sees no
# cluster APIs, so a capabilities gate would drop them on OpenShift.
scc_render() {
  "$HELM" template nifi-operator "$CHART" --namespace nifi-operator \
    --show-only templates/rbac.yaml --show-only templates/deployment.yaml "$@"
}
scc_render > "$tmp/scc-off.yaml"
if grep -qE 'securitycontextconstraints|--nifi-scc' "$tmp/scc-off.yaml"; then
  fail "SCC rule or --nifi-scc rendered without nifiPods.openshiftSCC"
fi
scc_render --set nifiPods.openshiftSCC=anyuid --set-string nifiPods.seccompProfile= > "$tmp/scc-on.yaml"
grep -q '^    - securitycontextconstraints$' "$tmp/scc-on.yaml" || fail "no SCC use rule for nifiPods.openshiftSCC"
grep -B2 '^    - securitycontextconstraints$' "$tmp/scc-on.yaml" | grep -q '^    - anyuid$' ||
  fail "SCC use rule not limited to the configured SCC"
grep -q -- '- --nifi-scc=anyuid$' "$tmp/scc-on.yaml" || fail "no --nifi-scc for nifiPods.openshiftSCC"

# The CSI driver's SCC binding: auto-detected by default, forced with scc.autoDetect=false.
# --show-only fails when the template renders nothing: that one error means "no binding"; any
# other helm error fails the check instead of passing the negative assertions below.
csi_scc() {
  if ! "$HELM" template nifi-operator "$CHART" --show-only templates/csi-driver-scc-binding.yaml "$@" \
    2>"$tmp/csi_scc.err"; then
    grep -q 'could not find template' "$tmp/csi_scc.err" || fail "helm template failed: $(cat "$tmp/csi_scc.err")"
  fi
}
if csi_scc | grep -q 'kind: RoleBinding'; then fail "CSI SCC binding rendered off OpenShift"; fi
if csi_scc --set csiDriver.scc.autoDetect=null | grep -q 'kind: RoleBinding'; then
  fail "a missing csiDriver.scc.autoDetect (--reuse-values) must auto-detect"
fi
csi_scc --api-versions security.openshift.io/v1 | grep -q 'name: system:openshift:scc:privileged' ||
  fail "no CSI SCC binding on OpenShift"
csi_scc --set csiDriver.scc.autoDetect=false | grep -q 'name: system:openshift:scc:privileged' ||
  fail "csiDriver.scc.autoDetect=false must render the CSI SCC binding"
csi_scc --set-string csiDriver.scc.autoDetect=false | grep -q 'name: system:openshift:scc:privileged' ||
  fail "csiDriver.scc.autoDetect=\"false\" (a string) must render the CSI SCC binding"
if csi_scc --set csiDriver.scc.autoDetect=false --set csiDriver.scc.enabled=false | grep -q 'kind: RoleBinding'; then
  fail "csiDriver.scc.enabled=false must not render the CSI SCC binding"
fi

# CRDs stay on `helm uninstall`: deleting one deletes every NiFiCluster.
"$HELM" template nifi-operator "$CHART" --show-only templates/crd.yaml > "$tmp/crd.yaml"
[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/crd.yaml")" = 2 ] || fail "expected 2 CRDs"
[ "$(grep -c '^    helm.sh/resource-policy: keep$' "$tmp/crd.yaml")" = 2 ] ||
  fail "every CRD needs helm.sh/resource-policy: keep"
[ "$(grep -c '^    argocd.argoproj.io/sync-options: ServerSideApply=true,Prune=false,Delete=false$' "$tmp/crd.yaml")" = 2 ] ||
  fail "every CRD needs the ArgoCD sync-options Prune=false,Delete=false"

echo "check-chart-ha: ok"

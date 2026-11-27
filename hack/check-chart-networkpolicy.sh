#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Checks the chart's optional operator NetworkPolicy on rendered output: off by default, and when
# on it selects the operator pods and opens the health, webhook and metrics ports by name, so they
# follow the container ports. Run by `make lint-chart`.
set -euo pipefail

HELM="${HELM:-helm}"
CHART="${1:-charts/nifi-operator}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { echo "check-chart-networkpolicy: $*" >&2; exit 1; }
# --show-only fails when the template renders nothing: that one error means "no NetworkPolicy";
# any other helm error fails the check.
render() {
  if ! "$HELM" template nifi-operator "$CHART" --namespace nifi-operator \
    --show-only templates/networkpolicy.yaml "$@" 2>"$tmp/err"; then
    grep -q 'could not find template' "$tmp/err" || fail "helm template failed: $(cat "$tmp/err")"
  fi
}

render > "$tmp/default.yaml"
if grep -q '^kind: NetworkPolicy$' "$tmp/default.yaml"; then
  fail "NetworkPolicy rendered by default"
fi

render --set networkPolicy.enabled=true > "$tmp/on.yaml"
grep -q '^kind: NetworkPolicy$' "$tmp/on.yaml" || fail "no NetworkPolicy with networkPolicy.enabled=true"
grep -A3 'podSelector:' "$tmp/on.yaml" | grep -q 'control-plane: controller-manager' ||
  fail "policy does not select the operator pods"
for port in health webhook-server metrics; do
  grep -q -- "- port: $port\$" "$tmp/on.yaml" || fail "port $port not allowed"
done
if grep -q 'from:' "$tmp/on.yaml"; then
  fail "default webhookFrom/metricsFrom must allow any source"
fi

render --set networkPolicy.enabled=true --set webhook.enabled=false --set metrics.enabled=false > "$tmp/min.yaml"
grep -q -- '- port: health$' "$tmp/min.yaml" || fail "health port missing without webhook and metrics"
if grep -qE -- '- port: (webhook-server|metrics)$' "$tmp/min.yaml"; then
  fail "webhook or metrics port allowed while disabled"
fi

render --set networkPolicy.enabled=true \
  --set 'networkPolicy.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring' \
  > "$tmp/from.yaml"
grep -q 'kubernetes.io/metadata.name: monitoring' "$tmp/from.yaml" || fail "metricsFrom not rendered"
echo "check-chart-networkpolicy: ok"

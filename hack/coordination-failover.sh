#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Measures Kubernetes-coordination failover of one NiFiCluster: deletes the pod holding
# the Cluster Coordinator Lease and reports how long until another node holds it, and until the
# NiFiCluster reports ClusterHealthy=True again. Disruptive: run it against a test cluster.
#
# Usage: hack/coordination-failover.sh <namespace> <cluster>   (uses the current kubectl context)
set -euo pipefail

ns="${1:?usage: $0 <namespace> <cluster>}"
cluster="${2:?usage: $0 <namespace> <cluster>}"
k() { kubectl -n "$ns" "$@"; }
holder() { k get lease "$1" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true; }
healthy() {
  k get nificlusters.nifi.ksolves.com "$cluster" \
    -o jsonpath='{.status.conditions[?(@.type=="ClusterHealthy")].status}' 2>/dev/null || true
}

lease="${cluster}-cluster-coordinator"
old="$(holder "$lease")"
[ -n "$old" ] || { echo "no holder on Lease $ns/$lease" >&2; exit 1; }
pod="${old%%.*}"
echo "coordinator: $old (pod $pod); primary node: $(holder "${cluster}-primary-node")"

start=$(date +%s)
k delete pod "$pod" --wait=false >/dev/null
echo "deleted $pod"

while :; do
  now="$(holder "$lease")"
  if [ -n "$now" ] && [ "$now" != "$old" ]; then
    echo "new coordinator after $(( $(date +%s) - start ))s: $now"
    break
  fi
  [ $(( $(date +%s) - start )) -lt 600 ] || { echo "no new coordinator within 600s" >&2; exit 1; }
  sleep 1
done

seen_unhealthy=""
while :; do
  h="$(healthy)"
  [ "$h" = "True" ] || seen_unhealthy=1
  if [ -n "$seen_unhealthy" ] && [ "$h" = "True" ]; then
    echo "ClusterHealthy=True again after $(( $(date +%s) - start ))s"
    break
  fi
  [ $(( $(date +%s) - start )) -lt 900 ] || { echo "ClusterHealthy not True within 900s" >&2; exit 1; }
  sleep 2
done
echo "primary node now: $(holder "${cluster}-primary-node")"

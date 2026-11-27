#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Measures the API-server load of Kubernetes coordination: Lease and ConfigMap requests
# per second, by verb, over a window, plus any API Priority and Fairness rejections or HTTP 429s.
# apiserver_request_total is cluster-wide, so measure once with the NiFi cluster running and once
# without it (scaled to 0 or not yet created) and subtract: the difference is the cluster's load.
# Needs read access to the API server's /metrics (cluster-admin, or a role with
# nonResourceURLs: ["/metrics"] get). With several API server replicas, each answers for itself;
# run it a few times or behind the same endpoint for a consistent view.
#
# Usage: hack/coordination-load.sh <minutes>   (uses the current kubectl context)
set -euo pipefail

minutes="${1:?usage: $0 <minutes>}"
snapshot() {
  kubectl get --raw /metrics | awk '
    /^apiserver_request_total\{/ && /resource="(leases|configmaps)"/ {
      match($0, /resource="[^"]+"/); r = substr($0, RSTART + 10, RLENGTH - 11)
      match($0, /verb="[^"]+"/);     v = substr($0, RSTART + 6, RLENGTH - 7)
      c = ($0 ~ /code="429"/) ? "429" : "ok"
      sum[r " " v " " c] += $NF
    }
    /^apiserver_flowcontrol_rejected_requests_total\{/ { sum["apf rejected -"] += $NF }
    END { for (k in sum) printf "%s %.0f\n", k, sum[k] }' | sort
}

before="$(mktemp)"; after="$(mktemp)"
trap 'rm -f "$before" "$after"' EXIT
snapshot > "$before"
echo "measuring for ${minutes} minute(s)..." >&2
sleep $(( minutes * 60 ))
snapshot > "$after"

echo "resource verb status requests/s"
join -a 2 -e 0 -o '0,1.2,2.2' <(awk '{print $1"|"$2"|"$3, $4}' "$before" | sort) \
  <(awk '{print $1"|"$2"|"$3, $4}' "$after" | sort) |
  awk -v secs=$(( minutes * 60 )) '{ split($1, k, "|"); d = $3 - $2; if (d > 0) printf "%s %s %s %.3f\n", k[1], k[2], k[3], d / secs }'

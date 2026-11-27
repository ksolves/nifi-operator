#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Measures how fast a NiFiCluster's content repository volume is, from inside node 0:
#   disk  - sequential write and read of 1 GiB with dd, bypassing the page cache (oflag/iflag=direct),
#           on the content repository's own directory, so it measures the PVC the node really uses;
#   nifi  - NiFi itself: GenerateFlowFile (1 MiB unique FlowFiles) feeding an auto-terminating
#           UpdateAttribute for <seconds>, reporting the content bytes written per second.
# The nifi mode needs an HTTP (non-TLS) cluster and creates, then deletes, a process group named
# bench-content-repo. Both modes write to the volume: use a test cluster.
#
# Usage: hack/bench-content-repo.sh <namespace> <cluster> [disk|nifi|all, default all] [seconds, default 60]
set -euo pipefail

ns="${1:?usage: $0 <namespace> <cluster> [disk|nifi|all] [seconds]}"
cluster="${2:?usage: $0 <namespace> <cluster> [disk|nifi|all] [seconds]}"
mode="${3:-all}"
seconds="${4:-60}"
pod="$(kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${cluster}" \
  -o jsonpath='{.items[0].metadata.name}')"
node="$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
sc="$(kubectl -n "$ns" get pvc -l "app.kubernetes.io/instance=${cluster}" \
  -o jsonpath='{.items[0].spec.storageClassName}')"
x() { kubectl -n "$ns" exec "$pod" -c nifi -- sh -c "$1"; }
echo "cluster $cluster, pod $pod on node $node, StorageClass $sc"

disk() {
  # shellcheck disable=SC2016 # expanded by the pod's shell
  # The content repository lives on the "data" PVC, or on its own with splitRepositoryStorage.
  x 'cd "$NIFI_HOME"; d=./data/content_repository; [ -d ./content-repository ] && d=./content-repository
     f="$d/bench-content-repo.tmp"; trap "rm -f $f" EXIT
     echo "content repository: $d ($(df -h "$d" | awk "NR==2{print \$2\" volume, \"\$4\" free\"}"))"
     printf "sequential write 1 GiB: "; dd if=/dev/zero of="$f" bs=1M count=1024 oflag=direct 2>&1 | tail -1
     printf "sequential read  1 GiB: "; dd if="$f" of=/dev/null bs=1M iflag=direct 2>&1 | tail -1'
}

# api METHOD PATH [JSON] calls NiFi's REST API from inside the pod; the body travels on stdin.
api() {
  local data=""
  [ -n "${3:-}" ] && data="-H Content-Type:application/json --data-binary @-"
  printf '%s' "${3:-}" | kubectl -n "$ns" exec -i "$pod" -c nifi -- \
    sh -c "curl -sSf -m 30 -X $1 $data \"http://\$(hostname -f):8080/nifi-api$2\""
}
json() { python3 -c "import sys,json; v=json.load(sys.stdin)
for k in sys.argv[1:]: v=v[int(k)] if k.isdigit() else v[k]
print(v)" "$@"; }

# Globals, not locals: the EXIT trap that removes them runs after nifi() has returned.
pg=""
# cleanup stops and removes the benchmark process group whatever happened before - a flow left
# running would keep filling the volume (and, on local-path, the node's own disk).
cleanup() {
  [ -n "$pg" ] || return 0
  set +e
  api PUT "/flow/process-groups/$pg" "{\"id\":\"$pg\",\"state\":\"STOPPED\"}" >/dev/null
  sleep 5
  for c in $(api GET "/process-groups/$pg/connections" | python3 -c \
    'import sys,json; print(" ".join(c["id"] for c in json.load(sys.stdin)["connections"]))'); do
    api POST "/flowfile-queues/$c/drop-requests" >/dev/null
  done
  sleep 5
  v="$(api GET "/process-groups/$pg" | json revision version)"
  api DELETE "/process-groups/$pg?version=$v" >/dev/null && echo "removed process group bench-content-repo" ||
    echo "WARNING: could not remove process group $pg - stop and delete it in the NiFi UI" >&2
}
trap cleanup EXIT

nifi() {
  local root gen sink
  root="$(api GET /flow/process-groups/root | json processGroupFlow id)"
  pg="$(api POST "/process-groups/$root/process-groups" \
    '{"revision":{"version":0},"component":{"name":"bench-content-repo","position":{"x":0,"y":0}}}' | json id)"
  gen="$(api POST "/process-groups/$pg/processors" '{"revision":{"version":0},"component":{
    "type":"org.apache.nifi.processors.standard.GenerateFlowFile","name":"bench-generate","position":{"x":0,"y":0},
    "config":{"schedulingPeriod":"0 sec","concurrentlySchedulableTaskCount":2,
      "properties":{"File Size":"1 MB","Batch Size":"10","Data Format":"Binary","Unique FlowFiles":"true"}}}}' | json id)"
  sink="$(api POST "/process-groups/$pg/processors" '{"revision":{"version":0},"component":{
    "type":"org.apache.nifi.processors.attributes.UpdateAttribute","name":"bench-sink","position":{"x":0,"y":300},
    "config":{"concurrentlySchedulableTaskCount":2,"autoTerminatedRelationships":["success"]}}}' | json id)"
  api POST "/process-groups/$pg/connections" "{\"revision\":{\"version\":0},\"component\":{
    \"source\":{\"id\":\"$gen\",\"groupId\":\"$pg\",\"type\":\"PROCESSOR\"},
    \"destination\":{\"id\":\"$sink\",\"groupId\":\"$pg\",\"type\":\"PROCESSOR\"},\"selectedRelationships\":[\"success\"]}}" >/dev/null
  api PUT "/flow/process-groups/$pg" "{\"id\":\"$pg\",\"state\":\"RUNNING\"}" >/dev/null
  echo "NiFi: GenerateFlowFile 1 MiB -> UpdateAttribute for ${seconds}s..."
  sleep "$seconds"
  # The status window is NiFi's last 5 minutes, which covers the whole run.
  api GET "/flow/processors/$gen/status" | python3 -c '
import sys, json
s = json.load(sys.stdin)["processorStatus"]["aggregateSnapshot"]
secs = int(sys.argv[1])
mib, ff = s["bytesWritten"] / secs / 2**20, s["flowFilesOut"] / secs
print(f"NiFi content written: {mib:.1f} MiB/s, {ff:.0f} FlowFiles/s (over {secs}s)")' "$seconds"
}

case "$mode" in
disk) disk ;;
nifi) nifi ;;
all) disk; nifi ;;
*) echo "unknown mode $mode (disk|nifi|all)" >&2; exit 2 ;;
esac

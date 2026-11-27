/*
Copyright 2026 Ksolves India Limited.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// Kubernetes Event reasons this reconciler emits on a NiFiCluster for lifecycle actions,
// so `kubectl describe nificluster` shows why something happened without reading operator logs,
// and event exporters (e.g. forwarding Warnings to Slack/PagerDuty) have a stable reason to match
// on. Each is emitted at most once per actual action - never once per reconcile - so a cluster
// that is simply up to date produces no repeated events; this is what keeps them "rate-limited"
// without a separate throttling mechanism, on top of client-go's own per-object spam filter
// (installed by controller-runtime's manager for every EventRecorder it hands out).
const (
	// reasonScaling fires when a node group's replica count actually changes, right before the
	// StatefulSet update that applies it - not on every reconcile that merely confirms the
	// current replica count still matches.
	reasonScaling = "Scaling"

	// reasonRollingRestart fires when a node group's pod template changes for a reason other than
	// its container image (a config override, a Secret rotation, a pod override, etc.) - see
	// reasonUpgradeStarted for an image-version change specifically.
	reasonRollingRestart = "RollingRestart"

	// reasonUpgradeStarted/reasonUpgradeCompleted bracket a node group's rollout onto a new NiFi
	// image: Started fires the moment the StatefulSet update carrying the new image is applied;
	// Completed fires once every node group is back to fully ready. There is no UpgradeBlocked
	// event yet - detecting a stalled rollout (vs. one still in progress) needs its own design,
	// tracked as a follow-up rather than guessed at here.
	reasonUpgradeStarted   = "UpgradeStarted"
	reasonUpgradeCompleted = "UpgradeCompleted"

	// reasonCertRotated fires each time the TLS rotation loop evicts one pod to pick up a freshly
	// issued leaf certificate (see reconcileTLSRotation) - one event per pod rotated, not one per
	// reconcile of the rotation check.
	reasonCertRotated = "CertRotated"

	// reasonOffloadStarted fires once a scale-down step has asked NiFi to disconnect and offload
	// the node it is about to remove (NIFI-248 B4). The StatefulSet's replica count is not lowered
	// - and that node's PersistentVolumeClaim is never at risk - until NiFi confirms the node holds
	// no more queued data (OFFLOADED); see confirmOffloaded.
	reasonOffloadStarted = "OffloadStarted"

	// reasonOffloadInProgress is a Normal event repeated every offloadRequeueInterval while a
	// scale-down step's target node is still short of OFFLOADED but within offloadStuckTimeout -
	// a routine, expected wait, not a problem (NIFI-248 PR #210 review).
	reasonOffloadInProgress = "OffloadInProgress"

	// reasonOffloadStuck is a Warning repeated every reconcile once a scale-down step's target
	// node has stayed short of OFFLOADED for longer than offloadStuckTimeout: a NiFi-side problem
	// (an unresponsive node can offload forever, Apache NIFI-6189) rather than a brief delay. The
	// step never proceeds without confirmation, so this cluster's scale-down stays held -
	// intervene in NiFi directly, or scale back up.
	reasonOffloadStuck = "OffloadStuck"

	// reasonReconnectingStrandedNode fires when reconnectStrandedNodes deletes a pod left
	// disconnected/offloading/offloaded by a scale-down step that was cancelled by scaling back
	// up before it finished (NIFI-248 PR #210 review).
	reasonReconnectingStrandedNode = "ReconnectingStrandedNode"

	// reasonNodeRemoved fires once a departed NiFi node (one no longer in the desired replica
	// range) has been removed from the cluster - see noderemoval.go.
	reasonNodeRemoved = "NodeRemoved"

	// reasonNodeGroupDraining fires for each pod a removed node group loses before its
	// StatefulSet is deleted - see drainRemovedStatefulSets.
	reasonNodeGroupDraining = "NodeGroupDraining"

	// reasonReconcileError fires when Reconcile returns an error, after every retry attempt -
	// client-go's own per-object event spam filter (not a mechanism of this operator's own)
	// prevents a persistently failing cluster from flooding identical events. The message is the
	// error's own text: audited to never interpolate a Secret's value, only names/keys of things
	// that failed.
	reasonReconcileError = "ReconcileError"

	// upgradingConditionType is True from reasonUpgradeStarted until reasonUpgradeCompleted fires
	// for the same node group's rollout - tracked so updateStatus (run once per reconcile, after
	// every node group has been reconciled) can tell whether an image upgrade that started on an
	// earlier reconcile has now finished, without the operator keeping any other state for it.
	upgradingConditionType = "Upgrading"
)

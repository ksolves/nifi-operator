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

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

const (
	dataVolumeName   = "data"
	logsVolumeName   = "logs"
	configVolumeName = "config"
	// confTemplateMountPath is where the ConfigMap is mounted read-only; the entrypoint (mounted
	// separately, see entrypointVolumeName) copies from here into the writable NIFI_HOME/conf,
	// patching in the one ordinal-dependent property (embedded ZooKeeper quorum membership) the
	// entrypoint alone can determine.
	confTemplateMountPath = "/opt/nifi/conf-template"
	// sensitiveConfigVolumeName/sensitiveConfMountPath carry the node group's Secret-sourced
	// nifi.properties overrides (configSecretName), applied by the entrypoint after the
	// ConfigMap's - kept out of the ConfigMap so they are never stored in plain text.
	sensitiveConfigVolumeName = "config-secret"
	sensitiveConfMountPath    = "/opt/nifi/conf-secret"
	// entrypointVolumeName/entrypointMountPath carry the node group's entrypoint script
	// (entrypointSecretName), mounted separately from the ConfigMap's own config template: a
	// NiFiCluster's pod Role grants broad access to every ConfigMap in the namespace (NIFI-248
	// B2; see entrypointSecretName), so the one file that's actually executed can't live there.
	entrypointVolumeName = "entrypoint"
	entrypointMountPath  = "/opt/nifi/entrypoint"

	// nifiHomePath mirrors NIFI_HOME as baked into images/nifi/Dockerfile (the base apache/nifi
	// image's own default install location) - centralized here since it's referenced by both
	// the data/logs volume mount paths and the preStop decommission command below.
	nifiHomePath = "/opt/nifi/nifi-current"

	// tlsVolumeName/confTLSMountPath - only added when nodeGroup.TLS.Enabled (see below). The
	// CSI driver (internal/csidriver) writes keystore.p12/truststore.p12 here at pod-mount
	// time; configmap.go's TLS-enabled properties reference these exact paths.
	tlsVolumeName    = "tls"
	confTLSMountPath = "/opt/nifi/tls"

	// tlsCSIDriverName must match config/csi/csidriver.yaml's registered CSIDriver name.
	tlsCSIDriverName = "tls.nifi.ksolves.com"

	// defaultTerminationGracePeriodSeconds mirrors the CRD's own default so a NodeGroup created
	// before that default existed (or via an older manifest) still gets a safe grace period.
	defaultTerminationGracePeriodSeconds int64 = 120

	// defaultLogStorage mirrors the CRD's own default for the same reason.
	defaultLogStorage = "5Gi"

	// NifiContainerName is the operator's own main container - reserved so a PodOverrides
	// InitContainer/Sidecar can never collide with (and thus silently replace, since the
	// Containers list is keyed by name) it.
	NifiContainerName = "nifi"

	// Volume names and default sizes for the per-repository storage split (NodeGroup's
	// SplitRepositoryStorage), only provisioned when that's enabled. Defaults are internal
	// (not exposed as separate CRD fields) to keep that feature's CRD surface to a single
	// toggle - content is sized largest since it holds the actual flowing data; the rest hold
	// much smaller metadata/state.
	flowfileVolumeName           = "flowfile-repository"
	contentVolumeName            = "content-repository"
	provenanceVolumeName         = "provenance-repository"
	databaseVolumeName           = "database-repository"
	extensionsVolumeName         = "extensions"
	defaultFlowFileRepoStorage   = "5Gi"
	defaultContentRepoStorage    = "10Gi"
	defaultProvenanceRepoStorage = "5Gi"
	defaultDatabaseRepoStorage   = "2Gi"
	defaultExtensionsStorage     = "5Gi"

	// extensionsNarSubPath/extensionsPythonSubPath are subPaths within the single extensions
	// PVC, mounted at two different locations so one PVC backs both the image's default NAR
	// auto-load directory and its default Python extensions directory (see
	// nifiPropNarLibraryAutoloadDir/nifiPropPythonExtensionsSourceDir in configmap.go - both are
	// left at their image-default relative paths, just backed by persistent storage instead of
	// the image's own ephemeral filesystem).
	extensionsNarSubPath    = "nars"
	extensionsPythonSubPath = "python"
)

// ReservedPodEnvNames are the "nifi" container's own env var names, set directly in
// buildStatefulSet or via authEnvVars - listed once here so both the validating webhook and
// buildStatefulSet's own defensive skip share a single source of truth, the same pattern as
// ReservedNifiPropertyKeys in configmap.go.
var ReservedPodEnvNames = map[string]struct{}{
	"POD_NAME":                 {},
	"POD_NAMESPACE":            {},
	"NIFI_CLUSTER_DOMAIN":      {},
	"NIFI_SENSITIVE_PROPS_KEY": {},
	"NIFI_AUTH_USERNAME":       {},
	"NIFI_AUTH_PASSWORD":       {},
	"NIFI_AUTH_PASSWORD_HASH":  {},
	keystorePasswordEnv:        {},
}

// ReservedVolumeNames are the volume names the operator generates itself.
var ReservedVolumeNames = map[string]struct{}{
	dataVolumeName:            {},
	logsVolumeName:            {},
	configVolumeName:          {},
	sensitiveConfigVolumeName: {},
	entrypointVolumeName:      {},
	tlsVolumeName:             {},
	flowfileVolumeName:        {},
	contentVolumeName:         {},
	provenanceVolumeName:      {},
	databaseVolumeName:        {},
	extensionsVolumeName:      {},
	scratchVolumeName:         {},
	confVolumeName:            {},
	passwdVolumeName:          {},
}

// Startup and liveness probe timings. The startup probe allows up to 10 minutes for
// the web server to come up - first boot plus NiFi's 5-minute flow election
// (nifi.cluster.flow.election.max.wait.time). Liveness then restarts a node only after its web
// server has not answered at all for about 2 minutes.
const (
	webServerProbeTimeoutSeconds  = 5
	startupProbePeriodSeconds     = 10
	startupProbeFailureThreshold  = 60
	livenessProbePeriodSeconds    = 20
	livenessProbeFailureThreshold = 6
)

// readinessProbeTimeoutSeconds bounds one readiness probe run, which under TLS can take up to
// three curl calls of --max-time 3 each, plus headroom for the shell itself (see
// readinessProbeHandler).
const readinessProbeTimeoutSeconds = 12

// singleUserTokenCmd is a shell command printing a bearer token for the pod's single-user
// credentials (authEnvVars), obtained from base + "/access/token". The password reaches curl on
// stdin (password@-), never on its command line, so it does not show up in the pod's process
// list.
func singleUserTokenCmd(base string, maxTimeSeconds int) string {
	return fmt.Sprintf(`printf '%%s' "${NIFI_AUTH_PASSWORD}" | curl -sk -m %d -X POST "%s/access/token" `+
		`--data-urlencode "username=${NIFI_AUTH_USERNAME}" --data-urlencode "password@-"`, maxTimeSeconds, base)
}

// bearerHeaderFileCmd is a shell command that logs in (singleUserTokenCmd) and writes the
// resulting "Authorization: Bearer <token>" header to file, mode 600, for curl -H @file. printf is
// a shell builtin, so the token never appears on a command line; an empty login leaves the file
// empty.
func bearerHeaderFileCmd(base string, maxTimeSeconds int, file string) string {
	return `(umask 077; tok=$(` + singleUserTokenCmd(base, maxTimeSeconds) + `); ` +
		`if [ -n "$tok" ]; then printf 'Authorization: Bearer %s\n' "$tok" > ` + file + `; else : > ` + file + `; fi)`
}

// keystorePasswordEnv is the env var carrying the cluster's keystore password
// (TLSKeystorePasswordDataKey) into TLS pods: the entrypoint writes it into nifi.properties, and
// the probes unlock the node certificate with it. A secretKeyRef, so the password is in neither
// the ConfigMap nor the pod spec.
const keystorePasswordEnv = "NIFI_KEYSTORE_PASSWORD" //nolint:gosec // G101: the env var name, not a password

// tlsEnvVars is the keystorePasswordEnv env var of a TLS node group's pods, none otherwise. The
// password is set once, when the CA Secret is created (or backfilled), so its value needs no hash
// on the pod template: the pods that predate it roll anyway on that upgrade.
func tlsEnvVars(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) []corev1.EnvVar {
	if !tlsEnabled(nodeGroup) {
		return nil
	}
	return []corev1.EnvVar{keystorePasswordEnvVar(cluster)}
}

// keystorePasswordEnvVar is the keystorePasswordEnv env var of a TLS node group's pods.
func keystorePasswordEnvVar(cluster *nifiv1alpha1.NiFiCluster) corev1.EnvVar {
	return corev1.EnvVar{
		Name: keystorePasswordEnv,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: tlsCASecretName(cluster)},
				Key:                  TLSKeystorePasswordDataKey,
			},
		},
	}
}

// readinessProbeHandler returns the readiness probe's ProbeHandler. Always an exec probe hitting
// /nifi-api/flow/cluster/summary, never httpGet against /nifi-api/system-diagnostics (the
// original check) - confirmed live that system-diagnostics only proves the JVM/web server is up,
// not that this node is actually connected to the NiFi cluster: killing the cluster coordinator
// dropped every node to DISCONNECTED for ~85s while every pod kept reporting Ready the entire
// time, since system-diagnostics never reflects cluster
// membership at all - readiness was true while the "high availability" the PodDisruptionBudget
// exists to protect wasn't actually there.
//
// /nifi-api/flow/cluster/summary's connectedToCluster field is documented as reflecting the
// perspective of the node fielding the request - i.e. "am I, the node answering this call,
// connected right now" - exactly the per-pod signal needed. Confirmed live against a real
// cluster: {"clusterSummary":{...,"connectedToCluster":true,...}}.
//
// httpGet can't do this even for the plain-HTTP case, for two reasons found live while building
// TLS support: (1) it always connects by pod IP, never a hostname, which fails Jetty's SNI/Host
// check outright once TLS is enabled (the certificate's SAN lists this pod's FQDN, not its IP,
// and httpGet has no way to override that per-pod-ordinal from one shared PodTemplateSpec - an
// exec probe naturally resolves `hostname -f` to *that* pod's own FQDN instead); (2) it can't
// inspect response body content the way checking connectedToCluster requires.
//
// Under TLS the endpoint needs an authenticated user. The probe used to treat the
// unauthenticated 401 as healthy, which let a restarted pod report Ready before it had rejoined
// the cluster, so a rolling restart moved on while it was still disconnected (ghost node). It
// now logs in with the pod's single-user credentials (authEnvVars), the same way preStopHandler
// does - the node certificate is not authorized for this endpoint under the single-user
// authorizer. The token is cached in /tmp and refreshed only on a 401, so a probe every 10s
// doesn't log in every time. A 401 that survives a fresh login means not Ready.
//
// Every curl gets --max-time 3, so even the worst case (probe, login, probe again) finishes
// within readinessProbeTimeoutSeconds instead of being SIGKILLed by the kubelet.
func readinessProbeHandler(nodeGroup *nifiv1alpha1.NodeGroup) corev1.ProbeHandler {
	const body = "/tmp/readiness-probe-body"
	script := fmt.Sprintf(
		`resp=$(curl -s --max-time 3 -o %s -w '%%{http_code}' "http://$(hostname -f):%d/nifi-api/flow/cluster/summary"); `+
			`[ "$resp" = "200" ] && grep -q '"connectedToCluster":true' %s`,
		body, nifiHTTPPort, body,
	)
	if tlsEnabled(nodeGroup) {
		// The cached Authorization header lives in a mode-600 file handed to curl as -H @file,
		// so the bearer token never appears on a command line.
		const cacheFile = "/tmp/readiness-probe-auth"
		base := fmt.Sprintf("https://$(hostname -f):%d/nifi-api", nifiHTTPSPort)
		script = `probe() { hdr=""; [ -s ` + cacheFile + ` ] && hdr="-H @` + cacheFile + `"; ` +
			`resp=$(curl -sk --max-time 3 $hdr -o ` + body + ` -w '%{http_code}' "` + base + `/flow/cluster/summary"); }; ` +
			`probe; ` +
			`if [ "$resp" = "401" ] && [ -n "${NIFI_AUTH_USERNAME:-}" ]; then ` +
			bearerHeaderFileCmd(base, 3, cacheFile) + `; probe; fi; ` +
			`[ "$resp" = "200" ] && grep -q '"connectedToCluster":true' ` + body
	}

	return corev1.ProbeHandler{
		Exec: &corev1.ExecAction{
			Command: []string{"sh", "-c", script},
		},
	}
}

// webServerProbeHandler returns the startup and liveness probes' handler: it passes when NiFi's
// web server answers HTTP at all - any status code, 401 and 503 included - and fails only when
// nothing answers (curl's code 000: refused, timed out, or a failed handshake). It deliberately
// does not look at cluster membership: a node that is disconnected, or waiting out a flow
// election that can take minutes, is not Ready (readinessProbeHandler) but is alive, and
// restarting it would only restart that wait. No login and no client certificate: an
// unauthenticated request is enough to prove the JVM and Jetty respond (NiFi 2 asks for client
// certificates but does not require them).
func webServerProbeHandler(nodeGroup *nifiv1alpha1.NodeGroup) corev1.ProbeHandler {
	url := fmt.Sprintf("http://$(hostname -f):%d/nifi-api/flow/about", nifiHTTPPort)
	if tlsEnabled(nodeGroup) {
		url = fmt.Sprintf("https://$(hostname -f):%d/nifi-api/flow/about", nifiHTTPSPort)
	}
	script := fmt.Sprintf(`code=$(curl -sk --max-time %d -o /dev/null -w '%%{http_code}' "%s"); `+
		`[ -n "$code" ] && [ "$code" != "000" ]`, webServerProbeTimeoutSeconds-1, url)
	return corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", script}}}
}

// preStopHandler offloads this node's data and disconnects it from the cluster before the
// StatefulSet controller sends SIGTERM, so a scale-down or restart doesn't drop in-flight
// flowfiles or leave a stale node in the cluster view - except when this node is the only one
// connected, where it skips straight to a plain shutdown instead (see below for why).
//
// Retries nifi.sh decommission with backoff rather than a single attempt: confirmed live that
// decommission can intermittently fail with IllegalNodeDisconnectionException ("Cannot disconnect
// node ... because it is the only node currently connected") when multiple nodes are scaled down
// close together - a transient race in the cluster coordinator's own view of node connectivity
// catching up, not an actual "last node
// standing" situation. Retrying gives that view time to settle. Five attempts with a growing
// delay between them only (4s, 6s, 8s, 10s - 28s total worst case, never sleeping after the
// final attempt since there's nothing left to wait for) stay well under
// terminationGracePeriodSeconds' default of 120s, so a genuinely stuck decommission still leaves
// time for Kubernetes' own SIGTERM/force-kill fallback instead of the retry loop consuming the
// entire grace period itself.
//
// That retry loop doesn't help a *genuinely* single-connected-node case, though - confirmed live
// on a real 3-node cluster scaled down to 1 replica: decommission's whole purpose is to
// redistribute this node's flowfiles to its peers before it leaves, and with zero peers there is
// nowhere to send them, so NiFi correctly (and permanently, not transiently) refuses every one of
// the 5 retries. This isn't a rare edge case - it fires on every single restart of any
// single-replica NiFiCluster (rolling updates, node drains, evictions, and any replicas: 1
// deployment, a common cheap dev/test setup), wasting the full ~28s retry budget each time before
// falling through to a clean shutdown anyway (NiFi catches the failure internally - no crash, no
// data corruption, no hang, just a noisy exception every restart). Fixed by checking this node's
// own view of cluster membership first, via its own local REST API, and skipping straight to
// nifi.sh stop when 0-1 nodes are (still) connected - the decommission retry loop above only ever
// runs for the >=2-node case it actually helps.
//
// Queries the node's own API, logged in with the single-user credentials when TLS is enabled, or
// over plain HTTP otherwise. A query that cannot be answered no longer counts as "0 connected"
// (which silently skipped the offload): decommission is still attempted, with a short retry, and
// a plain stop follows if it fails.
func preStopHandler(nodeGroup *nifiv1alpha1.NodeGroup) *corev1.LifecycleHandler {
	// login sets $auth to a bearer-token header when the pod carries the single-user credentials
	// (authEnvVars). It is how a TLS-secured node may ask its own API who is connected: its node
	// certificate is not authorized for /controller/cluster under the single-user authorizer, so
	// a certificate-authenticated query always looked like "0 connected" and decommission was
	// skipped on every TLS cluster - nodes stopped without offloading their data.
	// query defines q, which GETs one API path as this node.
	var base, query string
	if tlsEnabled(nodeGroup) {
		base = fmt.Sprintf("https://$(hostname -f):%d/nifi-api", nifiHTTPSPort)
		const headerFile = "/tmp/prestop-auth"
		query = `hdr=""; if [ -n "${NIFI_AUTH_USERNAME:-}" ]; then ` + bearerHeaderFileCmd(base, 5, headerFile) + `; ` +
			`[ -s ` + headerFile + ` ] && hdr="-H @` + headerFile + `"; fi; ` +
			`q() { curl -sk -m 5 $hdr "$1"; }; `
	} else {
		base = fmt.Sprintf("http://$(hostname -f):%d/nifi-api", nifiHTTPPort)
		query = `q() { curl -sk -m 5 "$1"; }; `
	}

	script := query +
		// "connected" stays empty when the node's view of the cluster cannot be read at all (no
		// credentials, NiFi not up); then decommission is still attempted, just not retried long.
		`body=$(q "` + base + `/controller/cluster" 2>/dev/null); ` +
		`connected=""; case "$body" in *'"nodes"'*) connected=$(printf '%s' "$body" | grep -o '"status":"CONNECTED"' | wc -l);; esac; ` +
		`if [ -n "$connected" ] && [ "$connected" -le 1 ]; then ` + nifiHomePath + `/bin/nifi.sh stop; exit $?; fi; ` +
		`max_attempts=5; [ -z "$connected" ] && max_attempts=2; ` +
		`attempt=1; while [ $attempt -le $max_attempts ]; do ` +
		nifiHomePath + `/bin/nifi.sh decommission && exit 0; ` +
		`attempt=$((attempt + 1)); [ $attempt -le $max_attempts ] && sleep $((attempt * 2)); ` +
		`done; ` + nifiHomePath + `/bin/nifi.sh stop`
	return &corev1.LifecycleHandler{
		Exec: &corev1.ExecAction{
			Command: []string{"sh", "-c", script},
		},
	}
}

// authEnvVars returns the single-user login env vars. NIFI_AUTH_USERNAME/NIFI_AUTH_PASSWORD come
// from the authentication Secret: the user's own when SecretName is set, or the operator's own
// synthesized one (authSecretName) otherwise - kept in sync by reconcileAuthSecret from literal
// Username/Password when those are set, or populated once with an operator-generated default
// login when Authentication is left unset entirely. Either way every node reads the same Secret,
// so (unlike NiFi's own per-node random single-user fallback) every node shares one login. The
// probes use them to get a token, piping the password to curl on stdin.
//
// NIFI_AUTH_PASSWORD_HASH is the password's bcrypt hash from singleUserHashSecretName, which
// entrypoint.sh writes into conf/login-identity-providers.xml on every boot (the file is not
// persisted across a restart). Writing the hash directly replaces "nifi.sh
// set-single-user-credentials <user> <password>", which put the password in a process argv any
// user on the node could list.
func authEnvVars(cluster *nifiv1alpha1.NiFiCluster) []corev1.EnvVar {
	secretName := effectiveAuthSecretName(cluster)

	envVar := func(name, secretName, key string) corev1.EnvVar {
		return corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  key,
				},
			},
		}
	}
	return []corev1.EnvVar{
		envVar("NIFI_AUTH_USERNAME", secretName, authSecretUsernameKey),
		envVar("NIFI_AUTH_PASSWORD", secretName, authSecretPasswordKey),
		envVar("NIFI_AUTH_PASSWORD_HASH", singleUserHashSecretName(cluster), singleUserHashKey),
	}
}

// applyPodOverrides merges a NodeGroup's PodOverrides onto an already-fully-built PodSpec - the
// same one buildStatefulSet is about to embed into the StatefulSet's pod template. Called
// unconditionally with a possibly-nil overrides (a no-op then), so unsecured/override-free node
// groups stay byte-for-byte unchanged, matching every other optional feature in this file.
//
// Reserved-name collisions (see ReservedPodEnvNames/NifiContainerName/ReservedVolumeNames) are
// skipped here defensively, the same two-layers-of-protection pattern as
// buildNifiProperties/ReservedNifiPropertyKeys: the validating webhook is the primary defense,
// this is the backup in case it's ever bypassed.
func applyPodOverrides(podSpec *corev1.PodSpec, overrides *nifiv1alpha1.PodOverridesSpec) {
	if overrides == nil {
		return
	}

	if len(overrides.NodeSelector) > 0 {
		podSpec.NodeSelector = overrides.NodeSelector
	}
	if len(overrides.Tolerations) > 0 {
		podSpec.Tolerations = overrides.Tolerations
	}
	if overrides.Affinity != nil {
		podSpec.Affinity = overrides.Affinity
	}
	if len(overrides.TopologySpreadConstraints) > 0 {
		// The default constraint (built by buildStatefulSet) selects exactly this node group's pods;
		// a user constraint without a selector would count none, so it inherits that one.
		var selector *metav1.LabelSelector
		if len(podSpec.TopologySpreadConstraints) > 0 {
			selector = podSpec.TopologySpreadConstraints[0].LabelSelector
		}
		constraints := make([]corev1.TopologySpreadConstraint, len(overrides.TopologySpreadConstraints))
		for i, c := range overrides.TopologySpreadConstraints {
			c = *c.DeepCopy()
			if c.LabelSelector == nil && selector != nil {
				c.LabelSelector = selector.DeepCopy()
			}
			constraints[i] = c
		}
		podSpec.TopologySpreadConstraints = constraints
	}
	if overrides.PriorityClassName != "" {
		podSpec.PriorityClassName = overrides.PriorityClassName
	}
	if overrides.ServiceAccountName != "" {
		podSpec.ServiceAccountName = overrides.ServiceAccountName
	}
	if len(overrides.ImagePullSecrets) > 0 {
		podSpec.ImagePullSecrets = overrides.ImagePullSecrets
	}
	if len(overrides.HostAliases) > 0 {
		podSpec.HostAliases = append(podSpec.HostAliases, overrides.HostAliases...)
	}
	if overrides.ImagePullPolicy != "" {
		for i := range podSpec.Containers {
			if podSpec.Containers[i].Name == NifiContainerName {
				podSpec.Containers[i].ImagePullPolicy = overrides.ImagePullPolicy
			}
		}
	}

	for _, c := range withDefaultSecurityContext(overrides.InitContainers) {
		if c.Name == NifiContainerName || c.Name == InitContainerName {
			continue
		}
		podSpec.InitContainers = append(podSpec.InitContainers, c)
	}
	for _, c := range withDefaultSecurityContext(overrides.Sidecars) {
		if c.Name == NifiContainerName || c.Name == InitContainerName {
			continue
		}
		podSpec.Containers = append(podSpec.Containers, c)
	}
	for _, v := range overrides.Volumes {
		if _, reserved := ReservedVolumeNames[v.Name]; reserved {
			continue
		}
		podSpec.Volumes = append(podSpec.Volumes, v)
	}

	if len(overrides.Env) == 0 {
		return
	}
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name != NifiContainerName {
			continue
		}
		for _, e := range overrides.Env {
			if _, reserved := ReservedPodEnvNames[e.Name]; reserved {
				continue
			}
			podSpec.Containers[i].Env = append(podSpec.Containers[i].Env, e)
		}
	}
}

// buildStatefulSet renders the StatefulSet for a node group. Storage is provisioned exclusively
// via VolumeClaimTemplates; no NFS/CIFS or emptyDir volumes are used for NiFi's repositories.
// configHash is folded into the pod template annotations so a ConfigMap change triggers a
// StatefulSet-native rolling restart; the entrypoint Secret's own hash is folded in the same way
// (entrypointHashAnnotation), so an operator upgrade that changes the embedded entrypoint script,
// or a spec change to bootstrap/logback/authorizers overrides, rolls the pods too.
//
// authSecretHash is referencedAuthSecretHash of the Secret backing
// spec.authentication.singleUser (see referencedAuthSecretVersion), or "" when singleUser isn't
// set or the Secret doesn't exist yet.
func buildStatefulSet(
	cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup, configHash, authSecretHash, serviceAccountName string,
	options ...statefulSetOptions,
) (*appsv1.StatefulSet, error) {
	storageQty, err := resource.ParseQuantity(nodeGroup.Storage)
	if err != nil {
		return nil, fmt.Errorf("parsing storage quantity %q for node group %q: %w", nodeGroup.Storage, nodeGroup.Name, err)
	}

	logStorage := nodeGroup.LogStorage
	if logStorage == "" {
		logStorage = defaultLogStorage
	}
	logStorageQty, err := resource.ParseQuantity(logStorage)
	if err != nil {
		return nil, fmt.Errorf("parsing logStorage quantity %q for node group %q: %w", logStorage, nodeGroup.Name, err)
	}

	repoClaims, err := repositoryVolumeClaimTemplates(nodeGroup, commonLabels(cluster, nodeGroup))
	if err != nil {
		return nil, err
	}

	labels := commonLabels(cluster, nodeGroup)
	replicas := nodeGroup.Replicas

	extraAnnotations := map[string]string{
		configHashAnnotation:     configHash,
		entrypointHashAnnotation: entrypointSecretHash(cluster, nodeGroup),
	}
	if tlsEnabled(nodeGroup) {
		// Leaf certificates are minted once, when the pod mounts its TLS volume, so a change to
		// any name they must carry (a new proxy host, extra SAN) only takes effect
		// on pods started afterwards. Hashing those names into the pod template rolls the pods.
		extraAnnotations[certSANsHashAnnotation] = stringsHash(sharedCertificateSANs(cluster, nodeGroup))
	}
	if cluster.Spec.Authentication != nil && cluster.Spec.Authentication.SingleUser != nil && authSecretHash != "" {
		extraAnnotations[authHashAnnotation] = authSecretHash
	}
	// Only the operator's own hash annotations go on the pod template, not spec.annotations or
	// spec.labels: those are object metadata, and copying them into the template made editing an
	// owner label roll every NiFi node.
	podAnnotations := extraAnnotations

	terminationGracePeriodSeconds := defaultTerminationGracePeriodSeconds
	if nodeGroup.TerminationGracePeriodSeconds != nil {
		terminationGracePeriodSeconds = *nodeGroup.TerminationGracePeriodSeconds
	}

	selectorLabels := map[string]string{
		labelAppName:     appNameValue,
		labelAppInstance: cluster.Name,
		labelNodeGroup:   nodeGroup.Name,
	}

	var opts statefulSetOptions
	if len(options) > 0 {
		opts = options[0]
	}
	containerSecurity := effectiveContainerSecurityContext(cluster, nodeGroup)
	automountToken := needsKubernetesAPI(cluster)

	podSpec := corev1.PodSpec{
		// Always an explicit ServiceAccount (never the namespace's "default"): Kubernetes
		// coordination needs the Lease/ConfigMap Role bound to it (see reconcileNiFiRBAC), and
		// that grant should reach the NiFi pods only.
		ServiceAccountName: serviceAccountName,
		// Only Kubernetes coordination needs the token (NiFi's Kubernetes client reads it);
		// otherwise the NiFi pods have no business calling the API server.
		AutomountServiceAccountToken:  &automountToken,
		ImagePullSecrets:              cluster.Spec.ImagePullSecrets,
		SecurityContext:               effectivePodSecurityContext(cluster, nodeGroup, opts),
		TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
		Affinity: &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
					{
						Weight: 100,
						PodAffinityTerm: corev1.PodAffinityTerm{
							TopologyKey:   "kubernetes.io/hostname",
							LabelSelector: &metav1.LabelSelector{MatchLabels: selectorLabels},
						},
					},
				},
			},
		},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
			{
				MaxSkew:           1,
				TopologyKey:       "topology.kubernetes.io/zone",
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: selectorLabels},
			},
		},
		Containers: []corev1.Container{
			{
				Name:  NifiContainerName,
				Image: nifiImage(cluster),
				// The operator's entrypoint, not the image's: stock apache/nifi images would run
				// their own start.sh.
				Command: []string{"sh", entrypointMountPath + "/" + entrypointKey},
				// Restricted by default (see defaultContainerSecurityContext), with the cluster's
				// and node group's overrides merged in.
				SecurityContext: containerSecurity,
				Ports: []corev1.ContainerPort{
					{Name: webPortNameHTTP, ContainerPort: nifiHTTPPort},
					{Name: "cluster-proto", ContainerPort: nifiClusterProtocolPort},
				},
				Env: append([]corev1.EnvVar{
					{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					}},
					// Via the downward API rather than the ServiceAccount token mount, which
					// ZooKeeper-coordinated pods do not get.
					{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
					}},
					// The entrypoint builds this pod's FQDN (its NiFi node address) with it.
					{Name: "NIFI_CLUSTER_DOMAIN", Value: ClusterDomain},
					{Name: "NIFI_SENSITIVE_PROPS_KEY", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: sensitivePropsSecretName(cluster)},
							Key:                  sensitivePropsKeyDataKey,
						},
					}},
					// nil (no Authentication configured) yields nothing from authEnvVars,
					// leaving NiFi's own auto-generated single-user credentials as the
					// fallback, unchanged from today's behavior.
				}, append(authEnvVars(cluster), tlsEnvVars(cluster, nodeGroup)...)...),
				Resources: nodeGroup.Resources,
				// Startup and liveness only check that the web server answers (webServerProbeHandler),
				// never cluster membership: flow election can legitimately take minutes, and killing
				// a node mid-election would just restart that wait. Readiness (membership) and
				// liveness start once the startup probe has passed.
				StartupProbe: &corev1.Probe{
					ProbeHandler:     webServerProbeHandler(nodeGroup),
					PeriodSeconds:    startupProbePeriodSeconds,
					FailureThreshold: startupProbeFailureThreshold,
					TimeoutSeconds:   webServerProbeTimeoutSeconds,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler:     webServerProbeHandler(nodeGroup),
					PeriodSeconds:    livenessProbePeriodSeconds,
					FailureThreshold: livenessProbeFailureThreshold,
					TimeoutSeconds:   webServerProbeTimeoutSeconds,
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:     readinessProbeHandler(nodeGroup),
					PeriodSeconds:    10,
					FailureThreshold: 3,
					TimeoutSeconds:   readinessProbeTimeoutSeconds,
				},
				Lifecycle: &corev1.Lifecycle{
					PreStop: preStopHandler(nodeGroup),
				},
				VolumeMounts: append([]corev1.VolumeMount{
					{Name: dataVolumeName, MountPath: nifiHomePath + "/data"},
					{Name: logsVolumeName, MountPath: nifiHomePath + "/logs"},
					{Name: configVolumeName, MountPath: confTemplateMountPath, ReadOnly: true},
					{Name: entrypointVolumeName, MountPath: entrypointMountPath, ReadOnly: true},
				}, readOnlyRootMounts()...),
			},
		},
		Volumes: append([]corev1.Volume{
			{
				Name: configVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(cluster, nodeGroup)},
					},
				},
			},
			{
				Name: entrypointVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: entrypointSecretName(cluster, nodeGroup)},
				},
			},
		}, readOnlyRootVolumes()...),
	}

	// Only added when a ConfigOverrides Secret is referenced, so other clusters keep an
	// unchanged pod template. Optional: the
	// operator creates the Secret only once a referenced Secret contributes a value, and the pod
	// must start before that too.
	if configOverridesSecretName(cluster.Spec.ConfigOverrides) != "" || configOverridesSecretName(nodeGroup.ConfigOverrides) != "" {
		optional := true
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: sensitiveConfigVolumeName, MountPath: sensitiveConfMountPath, ReadOnly: true,
		})
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: sensitiveConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: configSecretName(cluster, nodeGroup), Optional: &optional},
			},
		})
	}

	// Only added when TLS is enabled - an unsecured node group's generated StatefulSet must
	// stay byte-for-byte identical to the output without TLS, so this never touches podSpec otherwise.
	if nodeGroup.TLS != nil && nodeGroup.TLS.Enabled {
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: tlsVolumeName, MountPath: confTLSMountPath, ReadOnly: true,
		})
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{
					Driver: tlsCSIDriverName,
					VolumeAttributes: map[string]string{
						"clusterName":   cluster.Name,
						"namespace":     cluster.Namespace,
						"nodeGroupName": nodeGroup.Name,
					},
				},
			},
		})
	}

	// Only added when SplitRepositoryStorage is enabled - an existing node group's generated
	// StatefulSet must stay byte-for-byte identical when it isn't, since switching these mount
	// paths on an already-running cluster would orphan its existing data (see the field's own
	// doc comment). VolumeMounts added here, not VolumeClaimTemplates - those are added further
	// down, alongside the data/logs ones, once storageQty-style quantities are parsed.
	if SplitRepositoryStorageEnabled(nodeGroup) {
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: flowfileVolumeName, MountPath: nifiHomePath + "/flowfile-repository"},
			corev1.VolumeMount{Name: contentVolumeName, MountPath: nifiHomePath + "/content-repository"},
			corev1.VolumeMount{Name: provenanceVolumeName, MountPath: nifiHomePath + "/provenance-repository"},
			corev1.VolumeMount{Name: databaseVolumeName, MountPath: nifiHomePath + "/database-repository"},
			// Two mounts from the same PVC via SubPath: one backing the image's default NAR
			// auto-load directory, one backing its default Python extensions directory - both
			// otherwise resolve to the image's own ephemeral filesystem (see configmap.go's
			// nifiPropNarLibraryAutoloadDir/nifiPropPythonExtensionsSourceDir).
			corev1.VolumeMount{Name: extensionsVolumeName, MountPath: nifiHomePath + "/extensions", SubPath: extensionsNarSubPath},
			corev1.VolumeMount{Name: extensionsVolumeName, MountPath: nifiHomePath + "/python/extensions", SubPath: extensionsPythonSubPath},
		)
	}

	applyPodOverrides(&podSpec, nodeGroup.PodOverrides)
	// First, before any user init container: it prepares conf/ and /etc/passwd for the
	// read-only root filesystem. Same image and pull policy as the "nifi" container.
	podSpec.InitContainers = append([]corev1.Container{
		buildInitContainer(podSpec.Containers[0].Image, podSpec.Containers[0].ImagePullPolicy, containerSecurity,
			podSpec.Containers[0].VolumeMounts),
	}, podSpec.InitContainers...)

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      podLabels(cluster, nodeGroup),
			Annotations: podAnnotations,
		},
		Spec: podSpec,
	}

	// Stored on the StatefulSet itself (not the pod template) purely so the reconciler can tell
	// "did anything I manage actually change" by comparing this one value, instead of comparing
	// the whole template against the server's copy - which the API server has since decorated
	// with its own Pod-spec defaults ours never sets. See podTemplateHash's own comment.
	stsAnnotations := mergeStringMaps(mergedAnnotations(cluster, nodeGroup), map[string]string{
		templateHashAnnotation:        podTemplateHash(template),
		coordinationBackendAnnotation: string(coordinationBackend(cluster)),
	})

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        statefulSetName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      labels,
			Annotations: stsAnnotations,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			// Parallel, not the default OrderedReady: with a real readinessProbe (above) and
			// NiFi's flow election waiting for votes from every expected candidate,
			// OrderedReady deadlocks a fresh multi-replica cluster outright - ordinal 0 would
			// wait on ordinals 1/2 to vote, but they'd never be created because the
			// StatefulSet controller waits for ordinal 0 to be Ready first.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			// Always explicit, never Kubernetes' implicit default: Retain unless the user chose
			// Delete for the cluster or this node group, so NiFi repository/log data is
			// never silently destroyed by a scale-down or a NiFiCluster deletion.
			PersistentVolumeClaimRetentionPolicy: statefulSetPVCRetention(cluster, nodeGroup),
			ServiceName:                          headlessServiceName(cluster, nodeGroup),
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: template,
			VolumeClaimTemplates: append([]corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName, Labels: labels},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: nodeGroup.StorageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: storageQty},
						},
					},
				},
				{
					// NiFi's logs are persisted on their own PVC, never on emptyDir, so a pod
					// restart doesn't lose log history.
					ObjectMeta: metav1.ObjectMeta{Name: logsVolumeName, Labels: labels},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: nodeGroup.StorageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: logStorageQty},
						},
					},
				},
			}, repoClaims...),
		},
	}

	return sts, nil
}

// SplitRepositoryStorageEnabled reports whether a NodeGroup has opted into the per-repository
// storage split - nil-safe, since the field itself is optional. Shared by statefulset.go,
// configmap.go, and the validating webhook, so the "is this enabled" check can't drift between
// them.
func SplitRepositoryStorageEnabled(nodeGroup *nifiv1alpha1.NodeGroup) bool {
	return nodeGroup.SplitRepositoryStorage != nil && nodeGroup.SplitRepositoryStorage.Enabled
}

// repositoryVolumeClaimTemplates returns the extra VolumeClaimTemplates for
// NodeGroup.SplitRepositoryStorage - nil (no-op) when it's disabled, matching every other
// optional feature in this file. Each repository's size is user-configurable
// (SplitRepositoryStorageSpec's own fields), falling back to a sensible internal default when
// left unset, so this does need to propagate a parse error - same pattern as
// storageQty/logStorageQty earlier in buildStatefulSet.
func repositoryVolumeClaimTemplates(nodeGroup *nifiv1alpha1.NodeGroup, labels map[string]string) ([]corev1.PersistentVolumeClaim, error) {
	if !SplitRepositoryStorageEnabled(nodeGroup) {
		return nil, nil
	}
	split := nodeGroup.SplitRepositoryStorage

	pvc := func(name, size, defaultSize string) (corev1.PersistentVolumeClaim, error) {
		if size == "" {
			size = defaultSize
		}
		qty, err := resource.ParseQuantity(size)
		if err != nil {
			return corev1.PersistentVolumeClaim{}, fmt.Errorf(
				"parsing %s storage quantity %q for node group %q: %w", name, size, nodeGroup.Name, err)
		}
		return corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: nodeGroup.StorageClassName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
				},
			},
		}, nil
	}

	specs := []struct{ name, size, defaultSize string }{
		{flowfileVolumeName, split.FlowFileRepositoryStorage, defaultFlowFileRepoStorage},
		{contentVolumeName, split.ContentRepositoryStorage, defaultContentRepoStorage},
		{provenanceVolumeName, split.ProvenanceRepositoryStorage, defaultProvenanceRepoStorage},
		{databaseVolumeName, split.DatabaseRepositoryStorage, defaultDatabaseRepoStorage},
		{extensionsVolumeName, split.ExtensionsStorage, defaultExtensionsStorage},
	}
	claims := make([]corev1.PersistentVolumeClaim, 0, len(specs))
	for _, s := range specs {
		claim, err := pvc(s.name, s.size, s.defaultSize)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

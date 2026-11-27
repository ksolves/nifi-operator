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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func minimalCluster() *nifiv1alpha1.NiFiCluster {
	return &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
		},
	}
}

func TestBuildStatefulSetTLSVolumeWhenEnabled(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		TLS: &nifiv1alpha1.TLSSpec{Enabled: true},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}

	podSpec := sts.Spec.Template.Spec
	var tlsVolume *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == tlsVolumeName {
			tlsVolume = &podSpec.Volumes[i]
		}
	}
	if tlsVolume == nil {
		t.Fatal("TLS volume not found in pod spec when TLS.Enabled = true")
	}
	if tlsVolume.CSI == nil {
		t.Fatal("TLS volume has no CSI source")
	}
	if tlsVolume.CSI.Driver != tlsCSIDriverName {
		t.Errorf("TLS volume CSI.Driver = %q, want %q", tlsVolume.CSI.Driver, tlsCSIDriverName)
	}
	wantAttrs := map[string]string{"clusterName": "test-cluster", "namespace": "default", "nodeGroupName": "default"}
	for k, want := range wantAttrs {
		if got := tlsVolume.CSI.VolumeAttributes[k]; got != want {
			t.Errorf("TLS volume attribute %q = %q, want %q", k, got, want)
		}
	}

	found := false
	for _, vm := range podSpec.Containers[0].VolumeMounts {
		if vm.Name == tlsVolumeName {
			found = true
			if vm.MountPath != confTLSMountPath {
				t.Errorf("TLS volume mount path = %q, want %q", vm.MountPath, confTLSMountPath)
			}
		}
	}
	if !found {
		t.Error("TLS volume mount not found on the nifi container when TLS.Enabled = true")
	}
}

func TestBuildStatefulSetNoTLSVolumeWhenDisabledOrUnset(t *testing.T) {
	cluster := minimalCluster()

	for _, nodeGroup := range []*nifiv1alpha1.NodeGroup{
		{Name: "default", Replicas: 1, Storage: "1Gi"}, // TLS unset
		{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: false}},
	} {
		sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
		if err != nil {
			t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
		}
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == tlsVolumeName {
				t.Errorf("TLS volume present when TLS is unset/disabled (TLS=%+v)", nodeGroup.TLS)
			}
		}
		for _, vm := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
			if vm.Name == tlsVolumeName {
				t.Errorf("TLS volume mount present when TLS is unset/disabled (TLS=%+v)", nodeGroup.TLS)
			}
		}
	}
}

func TestBuildStatefulSetReadinessProbeChecksClusterMembership(t *testing.T) {
	cluster := minimalCluster()

	// Both variants must be exec probes (never httpGet - see readinessProbeHandler's doc: httpGet
	// can't inspect response body content, which checking connectedToCluster requires, and fails
	// outright under TLS due to SNI/pod-IP-only probing), must hit the cluster-summary endpoint
	// (not system-diagnostics, which only proves the JVM is up: a node can be
	// DISCONNECTED from the cluster while its web server stays perfectly healthy), and must
	// require connectedToCluster:true rather than just a 2xx response.
	unsecured := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	sts, err := buildStatefulSet(cluster, unsecured, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}
	probe := sts.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe.Exec == nil {
		t.Fatal("unsecured readiness probe has no Exec action, want an exec-based probe")
	}
	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, fmt.Sprintf("http://$(hostname -f):%d", nifiHTTPPort)) {
		t.Errorf("unsecured readiness probe command = %q, want it to hit plain HTTP on port %d", cmd, nifiHTTPPort)
	}
	if !strings.Contains(cmd, "/nifi-api/flow/cluster/summary") {
		t.Errorf("readiness probe command = %q, want it to check the cluster summary endpoint, not system-diagnostics", cmd)
	}
	if !strings.Contains(cmd, `"connectedToCluster":true`) {
		t.Errorf("readiness probe command = %q, want it to require connectedToCluster:true", cmd)
	}

	secured := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}
	sts, err = buildStatefulSet(cluster, secured, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}
	probe = sts.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe.Exec == nil {
		t.Fatal("TLS-enabled readiness probe has no Exec action, want an exec-based probe")
	}
	cmd = strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, fmt.Sprintf("https://$(hostname -f):%d", nifiHTTPSPort)) {
		t.Errorf("TLS-enabled readiness probe command = %q, want it to hit HTTPS on port %d", cmd, nifiHTTPSPort)
	}
	if strings.Contains(cmd, `"401" ] && exit 0`) {
		t.Errorf("TLS-enabled readiness probe command = %q, must not treat 401 as Ready (ghost node)", cmd)
	}
	if !strings.Contains(cmd, "/access/token") || !strings.Contains(cmd, `"connectedToCluster":true`) {
		t.Errorf("TLS-enabled readiness probe command = %q, want it to log in and require connectedToCluster:true", cmd)
	}
	if probe.TimeoutSeconds != readinessProbeTimeoutSeconds {
		t.Errorf("readiness probe TimeoutSeconds = %d, want %d", probe.TimeoutSeconds, readinessProbeTimeoutSeconds)
	}
}

// startup and liveness probes only check that the web server answers, with timings that
// outlast a flow election; readiness is no longer delayed, the startup probe gates it.
func TestBuildStatefulSetStartupAndLivenessProbes(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}
	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatal(err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if c.StartupProbe == nil || c.LivenessProbe == nil {
		t.Fatalf("startup %v / liveness %v probe missing", c.StartupProbe, c.LivenessProbe)
	}
	for name, p := range map[string]*corev1.Probe{"startup": c.StartupProbe, "liveness": c.LivenessProbe} {
		cmd := strings.Join(p.Exec.Command, " ")
		if !strings.Contains(cmd, "/nifi-api/flow/about") || strings.Contains(cmd, "connectedToCluster") || strings.Contains(cmd, "access/token") {
			t.Errorf("%s probe %q must only check that the web server answers", name, cmd)
		}
	}
	if got := c.StartupProbe.PeriodSeconds * c.StartupProbe.FailureThreshold; got < 600 {
		t.Errorf("startup probe allows %ds, want at least 600s (first boot + 5-minute flow election)", got)
	}
	if got := c.LivenessProbe.PeriodSeconds * c.LivenessProbe.FailureThreshold; got < 120 {
		t.Errorf("liveness probe restarts after %ds, want at least 120s", got)
	}
	if c.ReadinessProbe.InitialDelaySeconds != 0 {
		t.Errorf("readiness InitialDelaySeconds = %d, want 0 (the startup probe gates it)", c.ReadinessProbe.InitialDelaySeconds)
	}
}

func TestPreStopHandlerRetriesDecommission(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 3, Storage: "1Gi"}
	handler := preStopHandler(nodeGroup)
	if handler.Exec == nil {
		t.Fatal("preStopHandler() has no Exec action")
	}
	cmd := strings.Join(handler.Exec.Command, " ")

	// nifi.sh decommission can intermittently fail with IllegalNodeDisconnectionException
	// during close-together scale-downs - a single attempt isn't enough, so the script must
	// actually loop and retry rather than run the command once.
	for _, want := range []string{
		nifiHomePath + "/bin/nifi.sh decommission",
		"while",
		"max_attempts=5",
		"sleep",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("preStopHandler() command = %q, want it to contain %q", cmd, want)
		}
	}
}

func TestPreStopHandlerSkipsDecommissionWhenLastNodeConnected(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	handler := preStopHandler(nodeGroup)
	if handler.Exec == nil {
		t.Fatal("preStopHandler() has no Exec action")
	}
	cmd := strings.Join(handler.Exec.Command, " ")

	// The script must check the node's own view of cluster membership and skip straight to a
	// plain shutdown when 0-1 nodes are connected - decommission has nowhere to send flowfiles
	// with no peers, and NiFi permanently (not transiently) refuses it in that case, so retrying
	// is pointless and just wastes the full ~28s retry budget on every single-replica restart.
	for _, want := range []string{
		"nifi-api/controller/cluster",
		`-le 1`,
		nifiHomePath + "/bin/nifi.sh stop",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("preStopHandler() command = %q, want it to contain %q", cmd, want)
		}
	}

	// Must not `exec` into nifi.sh stop: exec replaces the preStop hook's own process, and
	// on this shutdown path the JVM exiting can race with the container itself tearing down,
	// making kubelet see the hook process die abnormally even though shutdown succeeded.
	if strings.Contains(cmd, "exec "+nifiHomePath+"/bin/nifi.sh stop") {
		t.Errorf("preStopHandler() command = %q, should call nifi.sh stop plainly, not via exec", cmd)
	}
}

// On a TLS cluster the node's own certificate is not authorized to read /controller/cluster, so
// preStop must log in with the single-user credentials instead (see preStopHandler).
func TestPreStopHandlerLogsInOnTLSClusters(t *testing.T) {
	secured := &nifiv1alpha1.NodeGroup{Name: "ng", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}
	cmd := strings.Join(preStopHandler(secured).Exec.Command, " ")
	for _, want := range []string{"/nifi-api/access/token", "NIFI_AUTH_USERNAME", "Authorization: Bearer"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("TLS preStop command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "nodecert") {
		t.Errorf("TLS preStop must not rely on the node certificate:\n%s", cmd)
	}
}

func TestBuildStatefulSetServiceAccountAndImagePullSecrets(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.ServiceAccountName = "nifi-runner"
	cluster.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-pull-secret"}}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}

	podSpec := sts.Spec.Template.Spec
	if podSpec.ServiceAccountName != "nifi-runner" {
		t.Errorf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, "nifi-runner")
	}
	if len(podSpec.ImagePullSecrets) != 1 || podSpec.ImagePullSecrets[0].Name != "registry-pull-secret" {
		t.Errorf("ImagePullSecrets = %+v, want [{registry-pull-secret}]", podSpec.ImagePullSecrets)
	}
}

func TestBuildStatefulSetServiceAccountDefaultsToOperatorManaged(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}

	podSpec := sts.Spec.Template.Spec
	if want := nifiServiceAccountName(cluster); podSpec.ServiceAccountName != want {
		t.Errorf("ServiceAccountName = %q, want the operator-managed %q (never the namespace default)",
			podSpec.ServiceAccountName, want)
	}
	if podSpec.ImagePullSecrets != nil {
		t.Errorf("ImagePullSecrets = %+v, want nil", podSpec.ImagePullSecrets)
	}
}

func TestBuildStatefulSetPVCRetentionPolicyIsExplicitRetain(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}

	policy := sts.Spec.PersistentVolumeClaimRetentionPolicy
	if policy == nil {
		t.Fatal("PersistentVolumeClaimRetentionPolicy is nil, want an explicit policy")
	}
	if policy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Errorf("WhenDeleted = %q, want %q", policy.WhenDeleted, appsv1.RetainPersistentVolumeClaimRetentionPolicyType)
	}
	if policy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Errorf("WhenScaled = %q, want %q", policy.WhenScaled, appsv1.RetainPersistentVolumeClaimRetentionPolicyType)
	}
}

func findEnvVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func TestAuthEnvVarsUseOperatorDefaultSecretWhenAuthenticationUnset(t *testing.T) {
	cluster := minimalCluster()

	env := authEnvVars(cluster)
	username := findEnvVar(env, "NIFI_AUTH_USERNAME")
	password := findEnvVar(env, "NIFI_AUTH_PASSWORD")
	if username == nil || password == nil {
		t.Fatalf("authEnvVars() = %+v, want both NIFI_AUTH_USERNAME and NIFI_AUTH_PASSWORD even when Authentication is unset", env)
	}
	wantSecret := authSecretName(cluster)
	if username.ValueFrom.SecretKeyRef.Name != wantSecret || username.ValueFrom.SecretKeyRef.Key != authSecretUsernameKey {
		t.Errorf("NIFI_AUTH_USERNAME secretKeyRef = %+v, want Secret %q key %q", username.ValueFrom.SecretKeyRef, wantSecret, authSecretUsernameKey)
	}
	if password.ValueFrom.SecretKeyRef.Name != wantSecret || password.ValueFrom.SecretKeyRef.Key != authSecretPasswordKey {
		t.Errorf("NIFI_AUTH_PASSWORD secretKeyRef = %+v, want Secret %q key %q", password.ValueFrom.SecretKeyRef, wantSecret, authSecretPasswordKey)
	}
}

func TestAuthEnvVarsUseOperatorSecretForLiteralCredentials(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
		SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "SuperSecret123!"},
	}

	env := authEnvVars(cluster)
	username := findEnvVar(env, "NIFI_AUTH_USERNAME")
	password := findEnvVar(env, "NIFI_AUTH_PASSWORD")
	if username == nil || password == nil {
		t.Fatalf("authEnvVars() = %+v, want both NIFI_AUTH_USERNAME and NIFI_AUTH_PASSWORD", env)
	}
	wantSecret := authSecretName(cluster)
	if username.ValueFrom.SecretKeyRef.Name != wantSecret || username.ValueFrom.SecretKeyRef.Key != authSecretUsernameKey {
		t.Errorf("NIFI_AUTH_USERNAME secretKeyRef = %+v, want Secret %q key %q", username.ValueFrom.SecretKeyRef, wantSecret, authSecretUsernameKey)
	}
	if password.ValueFrom.SecretKeyRef.Name != wantSecret || password.ValueFrom.SecretKeyRef.Key != authSecretPasswordKey {
		t.Errorf("NIFI_AUTH_PASSWORD secretKeyRef = %+v, want Secret %q key %q", password.ValueFrom.SecretKeyRef, wantSecret, authSecretPasswordKey)
	}
}

func TestAuthEnvVarsUseUserSecretWhenReferenced(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
		SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "my-own-login-secret"},
	}

	env := authEnvVars(cluster)
	username := findEnvVar(env, "NIFI_AUTH_USERNAME")
	password := findEnvVar(env, "NIFI_AUTH_PASSWORD")
	if username == nil || password == nil {
		t.Fatalf("authEnvVars() = %+v, want both NIFI_AUTH_USERNAME and NIFI_AUTH_PASSWORD", env)
	}
	if username.ValueFrom.SecretKeyRef.Name != "my-own-login-secret" {
		t.Errorf("NIFI_AUTH_USERNAME secretKeyRef.Name = %q, want %q", username.ValueFrom.SecretKeyRef.Name, "my-own-login-secret")
	}
	if password.ValueFrom.SecretKeyRef.Name != "my-own-login-secret" {
		t.Errorf("NIFI_AUTH_PASSWORD secretKeyRef.Name = %q, want %q", password.ValueFrom.SecretKeyRef.Name, "my-own-login-secret")
	}
}

func TestBuildStatefulSetIncludesAuthEnvVars(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
		SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "SuperSecret123!"},
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}

	env := sts.Spec.Template.Spec.Containers[0].Env
	if findEnvVar(env, "NIFI_AUTH_USERNAME") == nil || findEnvVar(env, "NIFI_AUTH_PASSWORD") == nil {
		t.Errorf("container Env = %+v, want NIFI_AUTH_USERNAME and NIFI_AUTH_PASSWORD present", env)
	}
}

// TestBuildStatefulSetRestartsOnLiteralSecretChange is the regression test for a gap found in
// review: authEnvVars only ever changes a pod's env var by SecretKeyRef *name*, never by value,
// so editing spec.authentication.singleUser.password in the CR alone would leave podTemplateHash
// unchanged - silently leaving already-running pods authenticating with the stale password.
// reconcileAuthSecret writes the new password into the operator-owned Secret, whose new
// resourceVersion changes authHashAnnotation and so the pod template.
func TestBuildStatefulSetRestartsOnLiteralSecretChange(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	cluster := minimalCluster()
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
		SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "first-password"},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "uid-1", ResourceVersion: "100"}}

	before, err := buildStatefulSet(cluster, nodeGroup, "hash", referencedAuthSecretHash(secret), nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	secret.ResourceVersion = "101"
	after, err := buildStatefulSet(cluster, nodeGroup, "hash", referencedAuthSecretHash(secret), nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	if podTemplateHash(before.Spec.Template) == podTemplateHash(after.Spec.Template) {
		t.Error("podTemplateHash unchanged after the literal-credentials Secret changed - pods would keep the stale password")
	}
}

// TestBuildStatefulSetNoAuthHashAnnotationForSecretRef confirms no authHashAnnotation is
// fabricated for a secretName reference whose Secret hasn't been resolved (authSecretHash "").
func TestBuildStatefulSetNoAuthHashAnnotationForSecretRef(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
		SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "my-own-login-secret"},
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}
	if _, ok := sts.Spec.Template.Annotations[authHashAnnotation]; ok {
		t.Errorf("pod template annotations = %+v, want no %q annotation for a secretName reference", sts.Spec.Template.Annotations, authHashAnnotation)
	}
}

// TestBuildStatefulSetRestartsOnReferencedSecretRotation confirms a new version of a
// user-supplied auth Secret changes the pod template (and so podTemplateHash), which is what
// rolls the pods onto the rotated credentials.
func TestBuildStatefulSetRestartsOnReferencedSecretRotation(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
		SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "my-own-login-secret"},
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "uid-1", ResourceVersion: "100"}}

	before, err := buildStatefulSet(cluster, nodeGroup, "hash", referencedAuthSecretHash(secret), nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}
	if got := before.Spec.Template.Annotations[authHashAnnotation]; got != referencedAuthSecretHash(secret) {
		t.Errorf("auth hash annotation = %q, want %q", got, referencedAuthSecretHash(secret))
	}

	secret.ResourceVersion = "101"
	after, err := buildStatefulSet(cluster, nodeGroup, "hash", referencedAuthSecretHash(secret), nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet(, nifiPodServiceAccount()) error = %v", err)
	}
	if before.Annotations[templateHashAnnotation] == after.Annotations[templateHashAnnotation] {
		t.Error("podTemplateHash unchanged after the referenced Secret was rotated - pods would keep the stale password")
	}
}

// TestBuildStatefulSetRestartsOnEntrypointChange is the regression test for PR #210's review: the
// embedded entrypoint script moved out of the ConfigMap into its own Secret, referenced by the
// pod template only by that Secret's *name* (which never changes) - so an operator release that
// changes the script's content used to leave podTemplateHash, and so the pods, completely
// unchanged.
func TestBuildStatefulSetRestartsOnEntrypointChange(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	cluster := minimalCluster()

	before, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}

	original := nifiEntrypoint
	nifiEntrypoint = original + "\n# a future release changed this script\n"
	defer func() { nifiEntrypoint = original }()

	after, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	if podTemplateHash(before.Spec.Template) == podTemplateHash(after.Spec.Template) {
		t.Error("podTemplateHash unchanged after the embedded entrypoint script changed - pods would keep running the stale script")
	}
	if before.Spec.Template.Annotations[entrypointHashAnnotation] == after.Spec.Template.Annotations[entrypointHashAnnotation] {
		t.Error("entrypointHashAnnotation unchanged after the embedded entrypoint script changed")
	}
}

// TestBuildStatefulSetRestartsOnBootstrapOverrideChange is the regression test for the entrypoint
// Secret's half of NIFI-248 B2 (PR #210 review): bootstrap.conf.overrides moved out of the
// ConfigMap into the entrypoint Secret, which is referenced by the pod template only by name - so
// without entrypointSecretHash, a spec.configOverrides.bootstrapConf change would silently leave
// already-running pods on the old JVM args.
func TestBuildStatefulSetRestartsOnBootstrapOverrideChange(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	cluster := minimalCluster()

	before, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}

	cluster = cluster.DeepCopy()
	cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{BootstrapConf: map[string]string{"java.arg.3": "-Xmx2g"}}
	after, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	if podTemplateHash(before.Spec.Template) == podTemplateHash(after.Spec.Template) {
		t.Error("podTemplateHash unchanged after spec.configOverrides.bootstrapConf changed - pods would keep the stale JVM args")
	}
}

// TestBuildStatefulSetNoPasswordDerivedAnnotationForLiteralCredentials is a regression test:
// literal mode used to put an unsalted sha256(username+password) on the pod
// template, which anyone who can read pods could brute-force offline. The annotation must now be
// the backing Secret's version hash, and no annotation may carry the password or that old hash.
func TestBuildStatefulSetNoPasswordDerivedAnnotationForLiteralCredentials(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	cluster := minimalCluster()
	auth := &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "first-password"}
	cluster.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{SingleUser: auth}
	secretHash := referencedAuthSecretHash(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "uid-1", ResourceVersion: "100"}})

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", secretHash, nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	if got := sts.Spec.Template.Annotations[authHashAnnotation]; got != secretHash {
		t.Errorf("auth hash annotation = %q, want Secret version hash %q", got, secretHash)
	}

	legacy := sha256.Sum256([]byte(auth.Username + "\x00" + auth.Password))
	legacyHash := hex.EncodeToString(legacy[:])
	for _, annotations := range []map[string]string{sts.Annotations, sts.Spec.Template.Annotations} {
		for key, value := range annotations {
			if value == legacyHash || strings.Contains(value, auth.Password) {
				t.Errorf("annotation %q = %q is derived from the password", key, value)
			}
		}
	}
}

func TestBuildStatefulSetPodOverridesNoOp(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	podSpec := sts.Spec.Template.Spec
	if len(podSpec.NodeSelector) != 0 || len(podSpec.Tolerations) != 0 || podSpec.PriorityClassName != "" {
		t.Error("nil PodOverrides should leave scheduling fields untouched")
	}
	// Only the operator's own: the nifi-init container, the config volume, the entrypoint
	// volume and the read-only-root emptyDirs.
	wantVolumes := 2 + len(readOnlyRootVolumes())
	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Name != InitContainerName ||
		len(podSpec.Containers) != 1 || len(podSpec.Volumes) != wantVolumes {
		t.Errorf("nil PodOverrides should leave containers/volumes untouched, got %d init, %d containers, %d volumes (want 1, 1, %d)",
			len(podSpec.InitContainers), len(podSpec.Containers), len(podSpec.Volumes), wantVolumes)
	}
}

func TestBuildStatefulSetPodOverridesApplied(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{
			Env:               []corev1.EnvVar{{Name: "MY_VAR", Value: "hello"}},
			NodeSelector:      map[string]string{"disktype": "ssd"},
			Tolerations:       []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
			PriorityClassName: "high-priority",
			InitContainers:    []corev1.Container{{Name: "wait-for-dns", Image: "busybox"}},
			Sidecars:          []corev1.Container{{Name: "log-shipper", Image: "fluent-bit"}},
			Volumes:           []corev1.Volume{{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	podSpec := sts.Spec.Template.Spec

	if podSpec.NodeSelector["disktype"] != "ssd" {
		t.Errorf("NodeSelector not applied: %v", podSpec.NodeSelector)
	}
	if len(podSpec.Tolerations) != 1 || podSpec.Tolerations[0].Key != "dedicated" {
		t.Errorf("Tolerations not applied: %v", podSpec.Tolerations)
	}
	if podSpec.PriorityClassName != "high-priority" {
		t.Errorf("PriorityClassName = %q, want %q", podSpec.PriorityClassName, "high-priority")
	}
	if len(podSpec.InitContainers) != 2 || podSpec.InitContainers[0].Name != InitContainerName ||
		podSpec.InitContainers[1].Name != "wait-for-dns" {
		t.Errorf("InitContainers not applied after %q: %v", InitContainerName, podSpec.InitContainers)
	}
	if len(podSpec.Containers) != 2 {
		t.Fatalf("expected nifi container + 1 sidecar, got %d containers", len(podSpec.Containers))
	}
	foundSidecar := false
	var nifiContainer *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "log-shipper" {
			foundSidecar = true
		}
		if podSpec.Containers[i].Name == NifiContainerName {
			nifiContainer = &podSpec.Containers[i]
		}
	}
	if !foundSidecar {
		t.Error("sidecar container not added")
	}
	if nifiContainer == nil {
		t.Fatal("nifi container missing")
	}
	foundEnv := false
	for _, e := range nifiContainer.Env {
		if e.Name == "MY_VAR" && e.Value == "hello" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("extra env var not applied to nifi container: %v", nifiContainer.Env)
	}
	foundVolume := false
	for _, v := range podSpec.Volumes {
		if v.Name == "extra" {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Errorf("extra volume not applied: %v", podSpec.Volumes)
	}
}

func TestBuildStatefulSetPodOverridesAffinityReplacesDefault(t *testing.T) {
	cluster := minimalCluster()
	customAffinity := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{Affinity: customAffinity},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	podSpec := sts.Spec.Template.Spec
	if podSpec.Affinity.PodAntiAffinity != nil {
		t.Error("custom Affinity should fully replace the operator's default pod anti-affinity")
	}
	if podSpec.Affinity.NodeAffinity == nil {
		t.Error("custom Affinity's NodeAffinity should be present")
	}
}

// user topology spread constraints replace the default zone one; one without a
// labelSelector inherits the node group's selector so it counts exactly its pods.
func TestBuildStatefulSetPodOverridesTopologySpreadReplacesDefault(t *testing.T) {
	cluster := minimalCluster()
	build := func(o *nifiv1alpha1.PodOverridesSpec) []corev1.TopologySpreadConstraint {
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 3, Storage: "1Gi", PodOverrides: o}
		sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
		if err != nil {
			t.Fatal(err)
		}
		return sts.Spec.Template.Spec.TopologySpreadConstraints
	}
	def := build(nil)
	if len(def) != 1 || def[0].TopologyKey != "topology.kubernetes.io/zone" || def[0].WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Fatalf("default constraints = %+v", def)
	}
	got := build(&nifiv1alpha1.PodOverridesSpec{TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
		{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.DoNotSchedule},
		{MaxSkew: 2, TopologyKey: "example.com/rack", WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}},
	}})
	if len(got) != 2 || got[0].WhenUnsatisfiable != corev1.DoNotSchedule || got[1].TopologyKey != "example.com/rack" {
		t.Fatalf("constraints = %+v, want the two user ones only", got)
	}
	if !reflect.DeepEqual(got[0].LabelSelector, def[0].LabelSelector) {
		t.Errorf("selector-less constraint got %v, want the node group's %v", got[0].LabelSelector, def[0].LabelSelector)
	}
	if got[1].LabelSelector.MatchLabels["x"] != "y" {
		t.Errorf("explicit selector overwritten: %v", got[1].LabelSelector)
	}
}

func TestBuildStatefulSetPodOverridesReservedNamesSkipped(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{
			Env:            []corev1.EnvVar{{Name: "POD_NAME", Value: "evil"}},
			InitContainers: []corev1.Container{{Name: NifiContainerName, Image: "evil"}},
			Sidecars:       []corev1.Container{{Name: NifiContainerName, Image: "evil"}},
			// configVolumeName already exists in podSpec.Volumes (unlike data/logs, which are
			// VolumeClaimTemplates, not podSpec.Volumes) - the only reserved name that lets this
			// test actually observe whether the override's conflicting source got applied.
			Volumes: []corev1.Volume{{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	podSpec := sts.Spec.Template.Spec

	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Name != InitContainerName {
		t.Errorf("init container named %q should have been skipped, leaving only %q, got %v",
			NifiContainerName, InitContainerName, podSpec.InitContainers)
	}
	if len(podSpec.Containers) != 1 {
		t.Errorf("sidecar named %q should have been skipped, got %d containers", NifiContainerName, len(podSpec.Containers))
	}
	for _, v := range podSpec.Volumes {
		if v.Name == configVolumeName && v.EmptyDir != nil {
			t.Error("reserved volume name should not have been overwritten by an override's EmptyDir source")
		}
	}
	for _, e := range podSpec.Containers[0].Env {
		if e.Name == "POD_NAME" && e.Value == "evil" {
			t.Error("reserved env var POD_NAME should not have been overridden")
		}
	}
}

func volumeClaimNames(sts *appsv1.StatefulSet) map[string]bool {
	names := make(map[string]bool, len(sts.Spec.VolumeClaimTemplates))
	for _, pvc := range sts.Spec.VolumeClaimTemplates {
		names[pvc.Name] = true
	}
	return names
}

func volumeMountNames(sts *appsv1.StatefulSet) map[string]bool {
	names := make(map[string]bool)
	for _, vm := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		names[vm.Name] = true
	}
	return names
}

func TestBuildStatefulSetSplitRepositoryStorageDisabledByDefault(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}

	claims := volumeClaimNames(sts)
	if len(claims) != 2 || !claims[dataVolumeName] || !claims[logsVolumeName] {
		t.Errorf("expected exactly data+logs PVCs when SplitRepositoryStorage is unset, got %v", claims)
	}
	for _, reserved := range []string{flowfileVolumeName, contentVolumeName, provenanceVolumeName, databaseVolumeName, extensionsVolumeName} {
		if claims[reserved] {
			t.Errorf("unexpected PVC %q when SplitRepositoryStorage is unset", reserved)
		}
	}
}

func TestBuildStatefulSetSplitRepositoryStorageEnabled(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}

	claims := volumeClaimNames(sts)
	wantClaims := []string{dataVolumeName, logsVolumeName, flowfileVolumeName, contentVolumeName, provenanceVolumeName, databaseVolumeName, extensionsVolumeName}
	for _, want := range wantClaims {
		if !claims[want] {
			t.Errorf("expected PVC %q when SplitRepositoryStorage is enabled, got %v", want, claims)
		}
	}
	if len(claims) != len(wantClaims) {
		t.Errorf("expected exactly %d PVCs, got %d: %v", len(wantClaims), len(claims), claims)
	}

	mounts := volumeMountNames(sts)
	for _, want := range []string{flowfileVolumeName, contentVolumeName, provenanceVolumeName, databaseVolumeName, extensionsVolumeName} {
		if !mounts[want] {
			t.Errorf("expected VolumeMount %q on the nifi container when SplitRepositoryStorage is enabled", want)
		}
	}

	// The extensions PVC should be mounted twice (NAR autoload dir + Python extensions dir),
	// via two different SubPaths.
	var extensionsMounts []corev1.VolumeMount
	for _, vm := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if vm.Name == extensionsVolumeName {
			extensionsMounts = append(extensionsMounts, vm)
		}
	}
	if len(extensionsMounts) != 2 {
		t.Fatalf("expected 2 mounts of the extensions volume (nars + python), got %d: %v", len(extensionsMounts), extensionsMounts)
	}
	subPaths := map[string]bool{extensionsMounts[0].SubPath: true, extensionsMounts[1].SubPath: true}
	if !subPaths[extensionsNarSubPath] || !subPaths[extensionsPythonSubPath] {
		t.Errorf("expected extensions mounts to use subPaths %q and %q, got %v", extensionsNarSubPath, extensionsPythonSubPath, subPaths)
	}
}

func claimSize(sts *appsv1.StatefulSet, name string) string {
	for _, pvc := range sts.Spec.VolumeClaimTemplates {
		if pvc.Name == name {
			return pvc.Spec.Resources.Requests.Storage().String()
		}
	}
	return ""
}

func TestBuildStatefulSetSplitRepositoryStorageDefaultSizes(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}

	wantDefaults := map[string]string{
		flowfileVolumeName:   defaultFlowFileRepoStorage,
		contentVolumeName:    defaultContentRepoStorage,
		provenanceVolumeName: defaultProvenanceRepoStorage,
		databaseVolumeName:   defaultDatabaseRepoStorage,
		extensionsVolumeName: defaultExtensionsStorage,
	}
	for name, want := range wantDefaults {
		if got := claimSize(sts, name); got != want {
			t.Errorf("PVC %q size = %q, want default %q", name, got, want)
		}
	}
}

func TestBuildStatefulSetSplitRepositoryStorageCustomSizes(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{
			Enabled:                     true,
			FlowFileRepositoryStorage:   "7Gi",
			ContentRepositoryStorage:    "50Gi",
			ProvenanceRepositoryStorage: "3Gi",
			DatabaseRepositoryStorage:   "4Gi",
			ExtensionsStorage:           "11Gi",
		},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}

	wantSizes := map[string]string{
		flowfileVolumeName:   "7Gi",
		contentVolumeName:    "50Gi",
		provenanceVolumeName: "3Gi",
		databaseVolumeName:   "4Gi",
		extensionsVolumeName: "11Gi",
	}
	for name, want := range wantSizes {
		if got := claimSize(sts, name); got != want {
			t.Errorf("PVC %q size = %q, want custom %q", name, got, want)
		}
	}
}

func TestBuildStatefulSetSplitRepositoryStorageInvalidSizePropagatesError(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{
			Enabled:                  true,
			ContentRepositoryStorage: "not-a-quantity",
		},
	}

	if _, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster)); err == nil {
		t.Error("expected buildStatefulSet() to return an error for an invalid repository storage size")
	}
}

func TestBuildStatefulSetPodOverridesImagePullPolicyNodeSASecretsHostAliases(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{
			ImagePullPolicy:    corev1.PullAlways,
			ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "my-registry-secret"}},
			ServiceAccountName: "custom-sa",
			HostAliases:        []corev1.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"internal.example.com"}}},
		},
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	podSpec := sts.Spec.Template.Spec

	var nifiContainer *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == NifiContainerName {
			nifiContainer = &podSpec.Containers[i]
		}
	}
	if nifiContainer == nil {
		t.Fatal("nifi container missing")
	}
	if nifiContainer.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("ImagePullPolicy = %q, want %q", nifiContainer.ImagePullPolicy, corev1.PullAlways)
	}
	if len(podSpec.ImagePullSecrets) != 1 || podSpec.ImagePullSecrets[0].Name != "my-registry-secret" {
		t.Errorf("ImagePullSecrets not applied: %v", podSpec.ImagePullSecrets)
	}
	if podSpec.ServiceAccountName != "custom-sa" {
		t.Errorf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, "custom-sa")
	}
	if len(podSpec.HostAliases) != 1 || podSpec.HostAliases[0].IP != "10.0.0.1" {
		t.Errorf("HostAliases not applied: %v", podSpec.HostAliases)
	}
}

func TestBuildStatefulSetPodOverridesDefaultsLeftUnsetWhenNotProvided(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.ServiceAccountName = "cluster-level-sa"
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatalf("buildStatefulSet() error = %v", err)
	}
	podSpec := sts.Spec.Template.Spec

	if podSpec.ServiceAccountName != "cluster-level-sa" {
		t.Errorf("ServiceAccountName should fall back to cluster-level value, got %q", podSpec.ServiceAccountName)
	}
	if len(podSpec.HostAliases) != 0 {
		t.Errorf("HostAliases should be empty by default, got %v", podSpec.HostAliases)
	}
	for _, c := range podSpec.Containers {
		if c.Name == NifiContainerName && c.ImagePullPolicy != "" {
			t.Errorf("ImagePullPolicy should be left unset by default (Kubernetes' own default applies), got %q", c.ImagePullPolicy)
		}
	}
}

// Spec labels/annotations are object metadata. They reach the StatefulSet and its
// PVC templates, but not the pod template, so editing them never rolls the NiFi nodes.
func TestBuildStatefulSetKeepsUserMetadataOffPodTemplate(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	build := func(owner string) *appsv1.StatefulSet {
		cluster := minimalCluster()
		cluster.Spec.Labels = map[string]string{"team": owner}
		cluster.Spec.Annotations = map[string]string{"owner": owner}
		sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
		if err != nil {
			t.Fatal(err)
		}
		return sts
	}
	a, b := build("team-a"), build("team-b")
	if a.Labels["team"] != "team-a" || a.Annotations["owner"] != "team-a" {
		t.Errorf("StatefulSet metadata = %v / %v, want the spec labels and annotations", a.Labels, a.Annotations)
	}
	for _, vct := range a.Spec.VolumeClaimTemplates {
		if vct.Labels["team"] != "team-a" {
			t.Errorf("PVC template %q labels = %v, want spec.labels", vct.Name, vct.Labels)
		}
	}
	if _, ok := a.Spec.Template.Labels["team"]; ok {
		t.Errorf("pod template labels = %v, must not carry spec.labels", a.Spec.Template.Labels)
	}
	if _, ok := a.Spec.Template.Annotations["owner"]; ok {
		t.Errorf("pod template annotations = %v, must not carry spec.annotations", a.Spec.Template.Annotations)
	}
	if podTemplateHash(a.Spec.Template) != podTemplateHash(b.Spec.Template) {
		t.Error("changing spec.labels/annotations changed the pod template - every node would roll")
	}
}

// the NiFi container never runs as root, escalates privileges or keeps capabilities.
func TestBuildStatefulSetNiFiContainerSecurityContext(t *testing.T) {
	cluster := minimalCluster()
	sts, err := buildStatefulSet(cluster, &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatal(err)
	}
	sc := sts.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("nifi container SecurityContext = %+v, want runAsNonRoot, no privilege escalation, drop ALL", sc)
	}
	// restricted Pod Security Standard.
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("nifi container SecurityContext = %+v, want readOnlyRootFilesystem and RuntimeDefault seccomp", sc)
	}
	podSC := sts.Spec.Template.Spec.SecurityContext
	if podSC == nil || podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		podSC.FSGroup == nil || *podSC.FSGroup != 0 ||
		podSC.FSGroupChangePolicy == nil || *podSC.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("pod SecurityContext = %+v, want RuntimeDefault seccomp, fsGroup 0, fsGroupChangePolicy OnRootMismatch", podSC)
	}
}

// pod FQDNs, Service SANs and the entrypoint's NiFi node address follow the
// configured cluster domain instead of a hard-coded cluster.local.
func TestClusterDomainIsConfigurable(t *testing.T) {
	saved := ClusterDomain
	ClusterDomain = "corp.internal"
	defer func() { ClusterDomain = saved }()

	if got := PodFQDN("c-default-0", "c", "default", "ns"); got != "c-default-0.c-default-headless.ns.svc.corp.internal" {
		t.Errorf("PodFQDN = %q", got)
	}
	cluster := minimalCluster()
	for _, name := range clusterServiceDNSNames(cluster) {
		if strings.Contains(name, "cluster.local") {
			t.Errorf("cluster Service SAN %q still uses cluster.local", name)
		}
	}
	sts, err := buildStatefulSet(cluster, &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatal(err)
	}
	var env string
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "NIFI_CLUSTER_DOMAIN" {
			env = e.Value
		}
	}
	if env != "corp.internal" {
		t.Errorf("NIFI_CLUSTER_DOMAIN = %q, want corp.internal", env)
	}
}

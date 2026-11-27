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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// on OpenShift without a configured SCC, fsGroup is left to restricted-v2.
func TestBuildStatefulSetNoFSGroup(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster), statefulSetOptions{NoFSGroup: true})
	if err != nil {
		t.Fatal(err)
	}
	sc := sts.Spec.Template.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod SecurityContext is nil")
	}
	if sc.FSGroup != nil || sc.FSGroupChangePolicy != nil {
		t.Errorf("fsGroup = %v, fsGroupChangePolicy = %v, want both unset with NoFSGroup", sc.FSGroup, sc.FSGroupChangePolicy)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccompProfile = %v, want RuntimeDefault with NoFSGroup too", sc.SeccompProfile)
	}
}

// Cluster and node group security contexts merge over the defaults field by field, the node
// group winning; unset fields keep the restricted defaults.
func TestBuildStatefulSetSecurityContextMergesFieldByField(t *testing.T) {
	cluster := minimalCluster()
	cluster.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(1001))}
	cluster.Spec.ContainerSecurityContext = &corev1.SecurityContext{
		ReadOnlyRootFilesystem: ptr.To(false),
		RunAsUser:              ptr.To(int64(1)),
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default", Replicas: 1, Storage: "1Gi",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{
			PodSecurityContext:       &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000))},
			ContainerSecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(2))},
		},
	}
	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatal(err)
	}

	podSC := sts.Spec.Template.Spec.SecurityContext
	if podSC.RunAsUser == nil || *podSC.RunAsUser != 1001 {
		t.Errorf("pod runAsUser = %v, want the cluster's 1001", podSC.RunAsUser)
	}
	if podSC.FSGroup == nil || *podSC.FSGroup != 2000 {
		t.Errorf("pod fsGroup = %v, want the node group's 2000", podSC.FSGroup)
	}
	if podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile = %v, want the default RuntimeDefault", podSC.SeccompProfile)
	}
	if podSC.FSGroupChangePolicy == nil || *podSC.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("pod fsGroupChangePolicy = %v, want the default OnRootMismatch", podSC.FSGroupChangePolicy)
	}

	sc := sts.Spec.Template.Spec.Containers[0].SecurityContext
	if sc.ReadOnlyRootFilesystem == nil || *sc.ReadOnlyRootFilesystem {
		t.Errorf("container readOnlyRootFilesystem = %v, want the cluster's explicit false", sc.ReadOnlyRootFilesystem)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 2 {
		t.Errorf("container runAsUser = %v, want the node group's 2 over the cluster's 1", sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container SecurityContext = %+v, want the unset fields left at their restricted defaults", sc)
	}
}

func TestEffectiveContainerSecurityContextNodeGroupOnly(t *testing.T) {
	nodeGroup := &nifiv1alpha1.NodeGroup{
		Name: "default",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{
			ContainerSecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(5))},
		},
	}
	sc := effectiveContainerSecurityContext(minimalCluster(), nodeGroup)
	want := defaultContainerSecurityContext()
	want.RunAsUser = ptr.To(int64(5))
	if !reflect.DeepEqual(sc, want) {
		t.Errorf("effectiveContainerSecurityContext() = %+v, want %+v", sc, want)
	}
}

// The operator's init container runs first, from the node image, under the nifi container's
// security context, and copies conf/ and /etc/passwd into their emptyDirs.
func TestBuildStatefulSetInitContainer(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatal(err)
	}
	podSpec := sts.Spec.Template.Spec
	if len(podSpec.InitContainers) == 0 || podSpec.InitContainers[0].Name != InitContainerName {
		t.Fatalf("InitContainers = %v, want %q first", podSpec.InitContainers, InitContainerName)
	}
	initC, nifi := podSpec.InitContainers[0], podSpec.Containers[0]
	if initC.Image != nifi.Image {
		t.Errorf("init image = %q, want the nifi container's %q", initC.Image, nifi.Image)
	}
	if !reflect.DeepEqual(initC.SecurityContext, nifi.SecurityContext) {
		t.Errorf("init SecurityContext = %+v, want the nifi container's %+v", initC.SecurityContext, nifi.SecurityContext)
	}
	mounts := map[string]string{}
	for _, m := range initC.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts[confVolumeName] != "/init/conf" || mounts[passwdVolumeName] != "/init/passwd" {
		t.Errorf("init mounts = %v, want %s at /init/conf and %s at /init/passwd", mounts, confVolumeName, passwdVolumeName)
	}
	if len(initC.Command) < 3 || !strings.Contains(initC.Command[2], `cp -R "$NIFI_HOME/conf/." /init/conf/`) {
		t.Errorf("init command = %q, want it to copy the image's conf/", initC.Command)
	}
}

// The nifi container's writable paths under a read-only root filesystem, with no mount path
// claimed twice (with and without split repository storage).
func TestBuildStatefulSetReadOnlyRootMounts(t *testing.T) {
	cluster := minimalCluster()
	for _, nodeGroup := range []*nifiv1alpha1.NodeGroup{
		{Name: "default", Replicas: 1, Storage: "1Gi"},
		{Name: "default", Replicas: 1, Storage: "1Gi", SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}},
	} {
		sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
		if err != nil {
			t.Fatal(err)
		}
		byPath := map[string]corev1.VolumeMount{}
		for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
			if _, dup := byPath[m.MountPath]; dup {
				t.Errorf("mount path %q mounted twice (split=%v)", m.MountPath, nodeGroup.SplitRepositoryStorage != nil)
			}
			byPath[m.MountPath] = m
		}

		want := make([]corev1.VolumeMount, 0, 11)
		want = append(want,
			corev1.VolumeMount{Name: confVolumeName, MountPath: nifiHomePath + "/conf"},
			corev1.VolumeMount{Name: passwdVolumeName, MountPath: "/etc/passwd", SubPath: "passwd", ReadOnly: true},
			corev1.VolumeMount{Name: scratchVolumeName, MountPath: "/tmp", SubPath: "tmp"},
			corev1.VolumeMount{Name: dataVolumeName, MountPath: nifiHomePath + "/state", SubPath: stateSubPath},
		)
		for _, dir := range []string{"work", "run", "nar_repository", "assets", "connector_assets", "nar_extensions", "python_extensions"} {
			want = append(want, corev1.VolumeMount{Name: scratchVolumeName, MountPath: nifiHomePath + "/" + dir, SubPath: dir})
		}
		for _, w := range want {
			if got, ok := byPath[w.MountPath]; !ok || !reflect.DeepEqual(got, w) {
				t.Errorf("mount at %q = %+v, want %+v (split=%v)", w.MountPath, got, w, nodeGroup.SplitRepositoryStorage != nil)
			}
		}
	}
}

func TestWithDefaultSecurityContext(t *testing.T) {
	own := &corev1.SecurityContext{RunAsUser: ptr.To(int64(7))}
	got := withDefaultSecurityContext([]corev1.Container{
		{Name: "bare"},
		{Name: "own", SecurityContext: own},
	})
	if !reflect.DeepEqual(got[0].SecurityContext, defaultContainerSecurityContext()) {
		t.Errorf("sidecar without SecurityContext got %+v, want the restricted default", got[0].SecurityContext)
	}
	if !reflect.DeepEqual(got[1].SecurityContext, &corev1.SecurityContext{RunAsUser: ptr.To(int64(7))}) {
		t.Errorf("sidecar with its own SecurityContext got %+v, want it kept as is", got[1].SecurityContext)
	}
}

func setSeccompProfile(t *testing.T, v string) {
	t.Helper()
	old := NiFiSeccompProfile
	NiFiSeccompProfile = v
	t.Cleanup(func() { NiFiSeccompProfile = old })
}

func buildSeccompTestStatefulSet(t *testing.T) (*corev1.PodSecurityContext, []corev1.Container) {
	t.Helper()
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster), statefulSetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := sts.Spec.Template.Spec
	return spec.SecurityContext, append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
}

func TestSeccompProfileDefaultRuntimeDefault(t *testing.T) {
	setSeccompProfile(t, string(corev1.SeccompProfileTypeRuntimeDefault))
	podSC, containers := buildSeccompTestStatefulSet(t)
	if podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile = %v, want RuntimeDefault", podSC.SeccompProfile)
	}
	for _, c := range containers {
		if c.SecurityContext == nil || c.SecurityContext.SeccompProfile == nil ||
			c.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("container %q seccompProfile not RuntimeDefault: %+v", c.Name, c.SecurityContext)
		}
	}
}

func TestSeccompProfileEmptyLeavesUnset(t *testing.T) {
	setSeccompProfile(t, "")
	podSC, containers := buildSeccompTestStatefulSet(t)
	if podSC.SeccompProfile != nil {
		t.Errorf("pod seccompProfile = %v, want unset", podSC.SeccompProfile)
	}
	for _, c := range containers {
		if c.SecurityContext == nil {
			t.Errorf("container %q has no SecurityContext", c.Name)
			continue
		}
		if c.SecurityContext.SeccompProfile != nil {
			t.Errorf("container %q seccompProfile = %v, want unset", c.Name, c.SecurityContext.SeccompProfile)
		}
	}
	if got := withDefaultSecurityContext([]corev1.Container{{Name: "bare"}}); got[0].SecurityContext.SeccompProfile != nil {
		t.Errorf("sidecar default seccompProfile = %v, want unset", got[0].SecurityContext.SeccompProfile)
	}
}

func TestSeccompProfileEmptyUserOverrideStillApplies(t *testing.T) {
	setSeccompProfile(t, "")
	cluster := minimalCluster()
	cluster.Spec.PodSecurityContext = &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	sc := effectivePodSecurityContext(cluster, nodeGroup, statefulSetOptions{})
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile = %v, want the cluster override", sc.SeccompProfile)
	}
}

func TestParseSeccompProfile(t *testing.T) {
	for _, ok := range []string{"RuntimeDefault", ""} {
		if got, err := ParseSeccompProfile(ok); err != nil || got != ok {
			t.Errorf("ParseSeccompProfile(%q) = %q, %v; want %q, nil", ok, got, err, ok)
		}
	}
	for _, bad := range []string{"Unconfined", "Localhost", "runtimedefault", "garbage", " RuntimeDefault"} {
		if _, err := ParseSeccompProfile(bad); err == nil {
			t.Errorf("ParseSeccompProfile(%q) = nil error, want rejection", bad)
		}
	}
}

func TestMergeNonZeroFieldsNilOverride(t *testing.T) {
	dst := defaultContainerSecurityContext()
	mergeNonZeroFields(dst, nil)
	if !reflect.DeepEqual(dst, defaultContainerSecurityContext()) {
		t.Errorf("mergeNonZeroFields(dst, nil) changed dst to %+v", dst)
	}
}

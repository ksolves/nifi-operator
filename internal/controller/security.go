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
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// NiFi pods meet the Kubernetes "restricted" Pod Security Standard and OpenShift's restricted-v2
// SCC by default: non-root, no privilege escalation, no capabilities, RuntimeDefault
// seccomp and a read-only root filesystem. NiFi's runtime writes go to emptyDirs (see
// scratchMounts) and the pod's PVCs.

// Names of the operator's own volumes and init container for a read-only root filesystem.
const (
	scratchVolumeName = "nifi-scratch"
	confVolumeName    = "nifi-conf"
	passwdVolumeName  = "nifi-passwd"
	// InitContainerName is the operator's init container, which prepares the emptyDirs.
	InitContainerName = "nifi-init"
	// stateSubPath is the directory on the data PVC that holds NiFi's local state (state/local),
	// which otherwise lived on the image's anonymous volume and was lost with the pod.
	stateSubPath = "state"
)

// The init container copies a few small files, but its first start on an existing cluster walks
// every volume once (initScript), which can take a while on a large content repository.
var (
	initContainerCPURequest    = resource.MustParse("100m")
	initContainerMemoryRequest = resource.MustParse("64Mi")
	initContainerCPULimit      = resource.MustParse("1")
	initContainerMemoryLimit   = resource.MustParse("256Mi")
)

// scratchMounts are the directories NiFi writes at runtime outside its PVCs (measured on a
// running node): NAR unpacking and Jetty (work), the PID file (run), installed NARs and assets,
// and the image's own extension directories. Each is a subPath of one emptyDir, so a restart of
// the pod starts them empty, as before.
var scratchMounts = []string{"work", "run", "nar_repository", "assets", "connector_assets", "nar_extensions", "python_extensions"}

// statefulSetOptions are reconciler-level settings buildStatefulSet needs on top of the
// NiFiCluster.
type statefulSetOptions struct {
	// NoFSGroup leaves fsGroup unset (--nifi-fs-group=""), for platforms that assign it
	// themselves and reject a fixed one, such as OpenShift's restricted-v2.
	NoFSGroup bool
	// FSGroup is the pod's fsGroup otherwise (--nifi-fs-group, 0 by default).
	FSGroup int64
	// RunAsUser is the pods' UID (--nifi-run-as-user, 1000 by default), with group 0; nil leaves
	// both unset, for platforms that assign the UID themselves, such as OpenShift's restricted-v2.
	RunAsUser *int64
}

// NiFiSeccompProfile is the seccomp profile type of NiFi pods and containers (operator-wide):
// "RuntimeDefault", or "" to leave seccompProfile unset, as OpenShift's anyuid SCC requires.
var NiFiSeccompProfile = string(corev1.SeccompProfileTypeRuntimeDefault)

// ParseSeccompProfile validates a NiFiSeccompProfile value.
func ParseSeccompProfile(s string) (string, error) {
	switch s {
	case string(corev1.SeccompProfileTypeRuntimeDefault), "":
		return s, nil
	}
	return "", fmt.Errorf("unsupported seccomp profile %q: want %q or empty", s, corev1.SeccompProfileTypeRuntimeDefault)
}

// defaultSeccompProfile is the SeccompProfile for NiFiSeccompProfile, nil when it is empty.
func defaultSeccompProfile() *corev1.SeccompProfile {
	if NiFiSeccompProfile == "" {
		return nil
	}
	return &corev1.SeccompProfile{Type: corev1.SeccompProfileType(NiFiSeccompProfile)}
}

// stockImageGroup is the group of the stock apache/nifi images' install tree ("nifi", 1000):
// lib/ is 770 and bin/nifi.sh 750, so a UID the platform assigns (OpenShift's arbitrary UIDs)
// can only start NiFi as a member.
const stockImageGroup int64 = 1000

// defaultPodSecurityContext is the NiFi pod's security context before any override.
func defaultPodSecurityContext(opts statefulSetOptions) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		SeccompProfile:     defaultSeccompProfile(),
		SupplementalGroups: []int64{stockImageGroup},
	}
	if !opts.NoFSGroup {
		// fsGroup (0, the root group, by default) makes a freshly provisioned PVC group-writable
		// whatever the provisioner's own permissions; the image's files are group 0 with g=u.
		// OnRootMismatch skips the recursive re-ownership on every later mount.
		sc.FSGroup = ptr.To(opts.FSGroup)
		sc.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch)
	}
	if opts.RunAsUser != nil {
		// Stock apache/nifi images run as the user "nifi", a name: runAsNonRoot needs a numeric
		// UID to verify. Group 0, as in the custom node images (USER 1000:0), so the
		// files NiFi writes stay usable by any later UID in group 0.
		sc.RunAsUser = ptr.To(*opts.RunAsUser)
		sc.RunAsGroup = ptr.To(int64(0))
	}
	return sc
}

// defaultContainerSecurityContext is the restricted security context of the "nifi" container,
// the init container, and of user init containers and sidecars that set none. Flows run user
// code (ExecuteScript, ExecuteStreamCommand), so no container gets root, privilege escalation or
// capabilities.
func defaultContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		ReadOnlyRootFilesystem:   ptr.To(true),
		SeccompProfile:           defaultSeccompProfile(),
	}
}

// effectivePodSecurityContext merges the cluster's and then the node group's podSecurityContext
// over the operator's defaults, field by field. supplementalGroups set there are added to
// stockImageGroup rather than replacing it: without that group NiFi cannot start under an
// assigned UID.
func effectivePodSecurityContext(
	cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup, opts statefulSetOptions,
) *corev1.PodSecurityContext {
	sc := defaultPodSecurityContext(opts)
	mergeNonZeroFields(sc, cluster.Spec.PodSecurityContext)
	if nodeGroup.PodOverrides != nil {
		mergeNonZeroFields(sc, nodeGroup.PodOverrides.PodSecurityContext)
	}
	if !slices.Contains(sc.SupplementalGroups, stockImageGroup) {
		sc.SupplementalGroups = append([]int64{stockImageGroup}, sc.SupplementalGroups...)
	}
	return sc
}

// effectiveContainerSecurityContext does the same for the "nifi" container.
func effectiveContainerSecurityContext(
	cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup,
) *corev1.SecurityContext {
	sc := defaultContainerSecurityContext()
	mergeNonZeroFields(sc, cluster.Spec.ContainerSecurityContext)
	if nodeGroup.PodOverrides != nil {
		mergeNonZeroFields(sc, nodeGroup.PodOverrides.ContainerSecurityContext)
	}
	return sc
}

// mergeNonZeroFields copies every field of override that is set (non-nil, non-empty) onto dst,
// a pointer to the same struct type. Fields are replaced whole: a capabilities or seccompProfile
// in override replaces the default one rather than merging into it.
func mergeNonZeroFields[T any](dst, override *T) {
	if override == nil {
		return
	}
	d := reflect.ValueOf(dst).Elem()
	o := reflect.ValueOf(override).Elem()
	for i := range o.NumField() {
		if f := o.Field(i); !f.IsZero() {
			d.Field(i).Set(f)
		}
	}
}

// withDefaultSecurityContext gives user init containers and sidecars (podOverrides) the
// restricted defaults when they set no securityContext of their own; one they did set is kept
// as it is.
func withDefaultSecurityContext(containers []corev1.Container) []corev1.Container {
	out := make([]corev1.Container, len(containers))
	for i, c := range containers {
		c = *c.DeepCopy()
		if c.SecurityContext == nil {
			c.SecurityContext = defaultContainerSecurityContext()
		}
		out[i] = c
	}
	return out
}

// initScript prepares the emptyDirs a read-only root filesystem needs: a copy of the image's
// conf/ for the entrypoint to patch, an /etc/passwd with an entry for the pod's UID (an arbitrary
// one on OpenShift), which NiFi and the JVM need, and the scratch and state directories, created
// by the pod's own UID so it can write them whatever fsGroup is (or isn't) set. Inline rather than
// a script in the image, so older node images start too. cp -R, not -a: preserving times on the
// emptyDir's root, which belongs to root, is not permitted.
//
// It also makes each of the pod's volumes group-writable for group 0, once per volume (marker
// file volumeGroupMarker, written only when the pass succeeds): the files this UID owns get group
// 0, g+rwX, and setgid directories. Node images from 0.13.0 on run NiFi with primary group 0 and
// umask 0002, so new files are group-writable too. Any later UID in group 0 (another runAsUser,
// OpenShift's arbitrary UIDs) can then use the data, on any storage, including storage that
// ignores fsGroup. Files of other UIDs, unreadable directories (lost+found) and symlinks, whose
// targets may lie outside the volume, are skipped.
var initScript = `set -eu
umask 0002
cp -R "$NIFI_HOME/conf/." /init/conf/
cp /etc/passwd /init/passwd/passwd
if ! grep -q "^[^:]*:[^:]*:$(id -u):" /etc/passwd; then
  echo "nifi:x:$(id -u):0:nifi user:$NIFI_HOME:/sbin/nologin" >>/init/passwd/passwd
fi
cd /init/scratch && mkdir -p tmp ` + strings.Join(scratchMounts, " ") + `
if [ -d /init/volumes/` + dataVolumeName + ` ]; then mkdir -p /init/volumes/` + dataVolumeName + `/` + stateSubPath + `; fi
uid=$(id -u)
for v in /init/volumes/*; do
  [ -d "$v" ] && [ -r "$v" ] && [ -w "$v" ] && [ ! -e "$v/` + volumeGroupMarker + `" ] || continue
  if find "$v" ! -readable -prune -o -user "$uid" ! -type l -exec chgrp 0 {} + &&
    find "$v" ! -readable -prune -o -user "$uid" ! -type l -exec chmod g+rwX {} + &&
    find "$v" ! -readable -prune -o -user "$uid" -type d -exec chmod g+s {} +; then
    touch "$v/` + volumeGroupMarker + `" || echo "nifi-init: could not write the marker in $v" >&2
  else
    echo "nifi-init: could not make $v group-writable for group 0; retrying on the next start" >&2
  fi
done`

// volumeGroupMarker marks a volume the init container has made group-writable.
const volumeGroupMarker = ".nifi-group-writable"

// initVolumesPath is where the init container mounts the pod's PVCs.
const initVolumesPath = "/init/volumes"

// buildInitContainer renders the operator's init container, from the node image itself (it
// copies that image's conf/), under the same security context as the "nifi" container.
func buildInitContainer(
	image string, pullPolicy corev1.PullPolicy, sc *corev1.SecurityContext, volumeMounts []corev1.VolumeMount,
) corev1.Container {
	return corev1.Container{
		Name:            InitContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"sh", "-c", initScript},
		Env:             []corev1.EnvVar{{Name: "NIFI_HOME", Value: nifiHomePath}},
		SecurityContext: sc.DeepCopy(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    initContainerCPURequest,
				corev1.ResourceMemory: initContainerMemoryRequest,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    initContainerCPULimit,
				corev1.ResourceMemory: initContainerMemoryLimit,
			},
		},
		VolumeMounts: append([]corev1.VolumeMount{
			{Name: confVolumeName, MountPath: "/init/conf"},
			{Name: passwdVolumeName, MountPath: "/init/passwd"},
			{Name: scratchVolumeName, MountPath: "/init/scratch"},
		}, initPVCMounts(volumeMounts)...),
	}
}

// initPVCMounts mounts every PVC-backed volume the "nifi" container uses, whole, under
// initVolumesPath for the group-writable pass of initScript.
func initPVCMounts(nifiMounts []corev1.VolumeMount) []corev1.VolumeMount {
	pvcVolumes := map[string]bool{
		dataVolumeName: true, logsVolumeName: true, flowfileVolumeName: true, contentVolumeName: true,
		provenanceVolumeName: true, databaseVolumeName: true, extensionsVolumeName: true,
	}
	var mounts []corev1.VolumeMount
	seen := map[string]bool{}
	for _, m := range nifiMounts {
		if pvcVolumes[m.Name] && !seen[m.Name] {
			seen[m.Name] = true
			mounts = append(mounts, corev1.VolumeMount{Name: m.Name, MountPath: initVolumesPath + "/" + m.Name})
		}
	}
	return mounts
}

// readOnlyRootVolumes are the emptyDirs behind scratchMounts, conf/ and /etc/passwd.
func readOnlyRootVolumes() []corev1.Volume {
	emptyDir := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	return []corev1.Volume{emptyDir(scratchVolumeName), emptyDir(confVolumeName), emptyDir(passwdVolumeName)}
}

// readOnlyRootMounts are the "nifi" container's mounts of those emptyDirs, plus NiFi's local
// state directory on the data PVC.
func readOnlyRootMounts() []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, 4+len(scratchMounts))
	mounts = append(mounts,
		corev1.VolumeMount{Name: confVolumeName, MountPath: nifiHomePath + "/conf"},
		corev1.VolumeMount{Name: passwdVolumeName, MountPath: "/etc/passwd", SubPath: "passwd", ReadOnly: true},
		corev1.VolumeMount{Name: scratchVolumeName, MountPath: "/tmp", SubPath: "tmp"},
		corev1.VolumeMount{Name: dataVolumeName, MountPath: nifiHomePath + "/state", SubPath: stateSubPath},
	)
	for _, dir := range scratchMounts {
		mounts = append(mounts, corev1.VolumeMount{Name: scratchVolumeName, MountPath: nifiHomePath + "/" + dir, SubPath: dir})
	}
	return mounts
}

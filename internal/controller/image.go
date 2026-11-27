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
	"regexp"
	"strconv"
	"strings"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// BuiltinNiFiImage is the node image a NiFiCluster without spec.image runs when the operator is
// started without --default-nifi-image: the stock Apache NiFi image.
const BuiltinNiFiImage = "apache/nifi:2.12.0"

// DefaultNiFiImage is the node image for a NiFiCluster without spec.image. The webhook writes it
// into the spec; buildStatefulSet falls back to it when the webhook is disabled. Set once from
// --default-nifi-image at startup, before the manager starts.
var DefaultNiFiImage = mustParseImage(BuiltinNiFiImage)

// MinimumNiFiVersion is the oldest NiFi the operator supports with Kubernetes coordination:
// Kubernetes-native clustering (Lease election, ConfigMap state) works from 2.11.0 on (NIFI-16120).
var MinimumNiFiVersion = [3]int{2, 11, 0}

// MinimumZooKeeperNiFiVersion is the oldest NiFi the operator supports with ZooKeeper
// coordination: 1.14.0 brought the single-user login and nifi.sh set-single-user-credentials
// and decommission, which the default login and safe scale-down rely on.
var MinimumZooKeeperNiFiVersion = [3]int{1, 14, 0}

// nifi2 is the first NiFi 2 release: NiFi 1.x loads its flow from flow.xml.gz.
var nifi2 = [3]int{2, 0, 0}

// clusterProtocolIsSecureRemoved is the NiFi version that removed nifi.cluster.protocol.is.secure
// (NIFI-15140); older NiFi defaults it to false.
var clusterProtocolIsSecureRemoved = [3]int{2, 7, 0}

// MinimumNiFiVersionFor returns the oldest NiFi supported with the cluster's coordination backend.
func MinimumNiFiVersionFor(cluster *nifiv1alpha1.NiFiCluster) [3]int {
	if coordinationBackend(cluster) == nifiv1alpha1.CoordinationBackendZooKeeper {
		return MinimumZooKeeperNiFiVersion
	}
	return MinimumNiFiVersion
}

// NiFiVersion returns the NiFi version of the cluster's node image, read from its tag, and false
// when the tag names none.
func NiFiVersion(cluster *nifiv1alpha1.NiFiCluster) ([3]int, bool) {
	image := cluster.Spec.Image
	if image.Repository == "" && image.Tag == "" {
		image = DefaultNiFiImage
	}
	return NiFiVersionFromTag(image.Tag)
}

// ParseImage splits "repository:tag" into an ImageSpec. The tag is after the last ":" that
// follows the last "/", so a registry port ("host:5000/nifi:tag") stays in the repository.
func ParseImage(ref string) (nifiv1alpha1.ImageSpec, error) {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash || colon == len(ref)-1 || colon == 0 {
		return nifiv1alpha1.ImageSpec{}, fmt.Errorf("image %q is not repository:tag", ref)
	}
	return nifiv1alpha1.ImageSpec{Repository: ref[:colon], Tag: ref[colon+1:]}, nil
}

func mustParseImage(ref string) nifiv1alpha1.ImageSpec {
	image, err := ParseImage(ref)
	if err != nil {
		panic(err)
	}
	return image
}

// nifiImage returns the node image reference: spec.image, or DefaultNiFiImage when it is unset.
func nifiImage(cluster *nifiv1alpha1.NiFiCluster) string {
	image := cluster.Spec.Image
	if image.Repository == "" && image.Tag == "" {
		image = DefaultNiFiImage
	}
	return fmt.Sprintf("%s:%s", image.Repository, image.Tag)
}

var versionInTag = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// NiFiVersionFromTag returns the first X.Y.Z in an image tag ("2.12.0", "nifi-node-2.12.0-0.12.0"),
// and false when the tag holds none ("latest", a custom name).
func NiFiVersionFromTag(tag string) ([3]int, bool) {
	m := versionInTag.FindStringSubmatch(tag)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

// VersionLess reports whether a is older than b.
func VersionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

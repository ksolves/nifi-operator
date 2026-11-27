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

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// sarValidator returns a validator whose SubjectAccessReviews answer allowed for the listed
// resource/subresource pairs only, and records every review it was asked.
func sarValidator(t *testing.T, allowed ...string) (*NiFiClusterCustomValidator, *[]authorizationv1.ResourceAttributes) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var asked []authorizationv1.ResourceAttributes
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			sar, ok := obj.(*authorizationv1.SubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			attrs := *sar.Spec.ResourceAttributes
			asked = append(asked, attrs)
			if sar.Spec.User != "alice" {
				t.Errorf("SubjectAccessReview asked for user %q, want the requesting user alice", sar.Spec.User)
			}
			for _, a := range allowed {
				if a == attrs.Resource+"/"+attrs.Subresource {
					sar.Status.Allowed = true
				}
			}
			return nil
		},
	}).Build()
	return &NiFiClusterCustomValidator{Client: c}, &asked
}

func requestContext() context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UserInfo: authenticationv1.UserInfo{Username: "alice", Groups: []string{"team-a"}},
	}})
}

// the operator reads a ConfigOverrides Secret and hands its values to NiFi, so the
// requesting user must be allowed to read that Secret themselves.
func TestConfigOverrideSecretRequiresGet(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi",
			ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{SecretName: "app-creds"}}},
	}}
	cluster.Namespace = "team-a"

	v, _ := sarValidator(t)
	_, err := v.ValidateCreate(requestContext(), cluster)
	if err == nil || !strings.Contains(err.Error(), "spec.nodeGroups[0].configOverrides.secretName") ||
		!strings.Contains(err.Error(), `read Secret "app-creds"`) {
		t.Fatalf("err = %v, want a denial naming spec.nodeGroups[0].configOverrides.secretName", err)
	}

	v, asked := sarValidator(t, "secrets/")
	if _, err := v.ValidateCreate(requestContext(), cluster); err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0].Verb != "get" || (*asked)[0].Name != "app-creds" || (*asked)[0].Namespace != "team-a" {
		t.Errorf("reviews asked = %+v, want get on secret app-creds in team-a", *asked)
	}

	// An unchanged reference is not re-checked.
	v, asked = sarValidator(t)
	updated := cluster.DeepCopy()
	updated.Spec.NodeGroups[0].Replicas = 2
	if _, err := v.ValidateUpdate(requestContext(), cluster, updated); err != nil || len(*asked) != 0 {
		t.Errorf("unchanged secretName re-checked: err = %v, asked = %+v", err, *asked)
	}
}

// The operator copies the keystore password Secret into the CA Secret, so naming it needs get.
func TestKeystorePasswordSecretRequiresGet(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi",
			TLS: &nifiv1alpha1.TLSSpec{Enabled: true, KeystorePasswordSecretName: "ks-pass"}}},
	}}
	cluster.Namespace = "team-a"

	v, _ := sarValidator(t)
	_, err := v.ValidateCreate(requestContext(), cluster)
	if err == nil || !strings.Contains(err.Error(), "spec.nodeGroups[0].tls.keystorePasswordSecretName") ||
		!strings.Contains(err.Error(), `read Secret "ks-pass"`) {
		t.Fatalf("err = %v, want a denial naming spec.nodeGroups[0].tls.keystorePasswordSecretName", err)
	}

	v, asked := sarValidator(t, "secrets/")
	if _, err := v.ValidateCreate(requestContext(), cluster); err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0].Verb != "get" || (*asked)[0].Name != "ks-pass" || (*asked)[0].Namespace != "team-a" {
		t.Errorf("reviews asked = %+v, want get on secret ks-pass in team-a", *asked)
	}

	v, asked = sarValidator(t)
	updated := cluster.DeepCopy()
	updated.Spec.NodeGroups[0].Replicas = 2
	if _, err := v.ValidateUpdate(requestContext(), cluster, updated); err != nil || len(*asked) != 0 {
		t.Errorf("unchanged keystorePasswordSecretName re-checked: err = %v, asked = %+v", err, *asked)
	}
}

// a requester who can't read the auth Secret is denied, and learns nothing about its
// content from the webhook.
func TestAuthSecretContentNotLeakedWithoutGet(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups:     []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
		Authentication: &nifiv1alpha1.AuthenticationSpec{SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "someone-elses"}},
	}}
	cluster.Name, cluster.Namespace = "c", "team-a"
	v, asked := sarValidator(t) // every review denied; the Secret doesn't exist either
	_, err := v.ValidateCreate(requestContext(), cluster)
	if err == nil || !strings.Contains(err.Error(), `read Secret "someone-elses"`) {
		t.Fatalf("err = %v, want a denial to read the Secret", err)
	}
	if strings.Contains(err.Error(), "username") || strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v leaks the Secret's state", err)
	}
	if len(*asked) == 0 {
		t.Error("no SubjectAccessReview was made")
	}
}

// podOverrides decide what pods run, so setting them needs `create pods`.
func TestPodOverridesRequireCreatePods(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{
		Name: "default", Replicas: 1, Storage: "1Gi",
		PodOverrides: &nifiv1alpha1.PodOverridesSpec{Sidecars: []corev1.Container{{Name: "shell", Image: "busybox"}}},
	}}}}
	cluster.Namespace = "team-a"

	v, _ := sarValidator(t)
	if _, err := v.ValidateCreate(requestContext(), cluster); err == nil ||
		!strings.Contains(err.Error(), "spec.nodeGroups[0].podOverrides") || !strings.Contains(err.Error(), "create pods") {
		t.Fatalf("err = %v, want a denial on podOverrides", err)
	}

	v, asked := sarValidator(t, "pods/")
	if _, err := v.ValidateCreate(requestContext(), cluster); err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0].Verb != "create" || (*asked)[0].Resource != "pods" {
		t.Errorf("reviews asked = %+v, want create pods", *asked)
	}

	v, asked = sarValidator(t)
	updated := cluster.DeepCopy()
	updated.Spec.NodeGroups[0].Replicas = 2
	if _, err := v.ValidateUpdate(requestContext(), cluster, updated); err != nil || len(*asked) != 0 {
		t.Errorf("unchanged podOverrides re-checked: err = %v, asked = %+v", err, *asked)
	}
}

// a new or changed cluster-level security context needs `create pods`, like
// podOverrides; an unchanged one is not re-checked.
func TestSecurityContextRequiresCreatePods(t *testing.T) {
	root := int64(0)
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
	}}
	withPodSC := cluster.DeepCopy()
	withPodSC.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: &root}
	withContainerSC := cluster.DeepCopy()
	withContainerSC.Spec.ContainerSecurityContext = &corev1.SecurityContext{RunAsUser: &root}

	for path, updated := range map[string]*nifiv1alpha1.NiFiCluster{
		"spec.podSecurityContext":       withPodSC,
		"spec.containerSecurityContext": withContainerSC,
	} {
		grants := podShapingGrants(cluster, updated)
		if len(grants) != 1 || grants[0].path.String() != path || grants[0].resource != "pods" {
			t.Errorf("grants = %+v, want one pods grant at %s", grants, path)
		}
		if grants := podShapingGrants(updated, updated.DeepCopy()); len(grants) != 0 {
			t.Errorf("unchanged %s re-checked: grants = %+v", path, grants)
		}
	}
}

// a literal password draws a warning.
func TestLiteralPasswordWarns(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
		Authentication: &nifiv1alpha1.AuthenticationSpec{SingleUser: &nifiv1alpha1.SingleUserAuthSpec{
			Username: "admin", Password: "a-long-enough-password",
		}},
	}}
	warnings, err := (&NiFiClusterCustomValidator{}).ValidateCreate(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "plain text") {
		t.Errorf("warnings = %v, want the plain-text password warning", warnings)
	}
}

// an image outside the trusted repositories runs arbitrary code in the NiFi pod, so it
// needs `create pods`; a trusted repository or a tag-only change does not.
func TestImageRepositoryRequiresCreatePodsUnlessTrusted(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		Image:      nifiv1alpha1.ImageSpec{Repository: "docker.io/acme/nifi", Tag: "2.12.0"},
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
	}}
	cluster.Namespace = "team-a"

	v, asked := sarValidator(t)
	v.TrustedImageRepositories = []string{"acme/nifi"}
	if _, err := v.ValidateCreate(requestContext(), cluster); err != nil || len(*asked) != 0 {
		t.Fatalf("trusted repository checked: err = %v, asked = %+v", err, *asked)
	}
	retagged := cluster.DeepCopy()
	retagged.Spec.Image.Tag = "2.13.0"
	if _, err := v.ValidateUpdate(requestContext(), cluster, retagged); err != nil || len(*asked) != 0 {
		t.Fatalf("tag change checked: err = %v, asked = %+v", err, *asked)
	}

	v, _ = sarValidator(t)
	v.TrustedImageRepositories = []string{"registry.example.com/nifi/"}
	untrusted := cluster.DeepCopy()
	untrusted.Spec.Image.Repository = "evil.example.com/nifi"
	if _, err := v.ValidateUpdate(requestContext(), cluster, untrusted); err == nil ||
		!strings.Contains(err.Error(), "spec.image.repository") || !strings.Contains(err.Error(), "create pods") {
		t.Fatalf("err = %v, want a denial on spec.image.repository", err)
	}
	underPrefix := cluster.DeepCopy()
	underPrefix.Spec.Image.Repository = "registry.example.com/nifi/node"
	if _, err := v.ValidateUpdate(requestContext(), cluster, underPrefix); err != nil {
		t.Fatalf("repository under a trusted prefix denied: %v", err)
	}

	v, asked = sarValidator(t, "pods/")
	if _, err := v.ValidateUpdate(requestContext(), cluster, untrusted); err != nil {
		t.Fatalf("user allowed to create pods denied: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0].Resource != "pods" || (*asked)[0].Verb != "create" {
		t.Errorf("reviews asked = %+v, want create pods", *asked)
	}
}

func TestTrustedImageRepository(t *testing.T) {
	for _, tc := range []struct {
		repo    string
		trusted []string
		want    bool
	}{
		{"nifi", []string{"docker.io/library/nifi"}, true},
		{"index.docker.io/apache/nifi", []string{"apache/nifi"}, true},
		{"apache/nifi-evil", []string{"apache/nifi"}, false},
		{"apache/nifi", []string{"apache/"}, true},
		{"apache/nifi", []string{" ", ""}, false},
		{"apache/nifi", nil, false},
		{"docker.io/quay.io/acme", []string{"quay.io/acme"}, false},
		{"docker.io/localhost/acme", []string{"localhost/acme"}, false},
		{"ghcr.io/library/nifi", []string{"ghcr.io/nifi"}, false},
	} {
		if got := trustedImageRepository(tc.repo, tc.trusted); got != tc.want {
			t.Errorf("trustedImageRepository(%q, %q) = %v, want %v", tc.repo, tc.trusted, got, tc.want)
		}
	}
}

// bootstrap.conf picks the java binary and its arguments, so changing it needs
// `create pods`, at the cluster and in a node group; an unchanged one is not re-checked.
func TestBootstrapConfRequiresCreatePods(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
	}}
	cluster.Namespace = "team-a"

	withClusterArg := cluster.DeepCopy()
	withClusterArg.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
		BootstrapConf: map[string]string{"java.arg.99": "-javaagent:/tmp/agent.jar"},
	}
	withGroupArg := cluster.DeepCopy()
	withGroupArg.Spec.NodeGroups[0].ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
		BootstrapConf: map[string]string{"java": "/tmp/java"},
	}
	for path, updated := range map[string]*nifiv1alpha1.NiFiCluster{
		"spec.configOverrides.bootstrapConf":               withClusterArg,
		"spec.nodeGroups[0].configOverrides.bootstrapConf": withGroupArg,
	} {
		v, _ := sarValidator(t)
		if _, err := v.ValidateUpdate(requestContext(), cluster, updated); err == nil ||
			!strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "create pods") {
			t.Errorf("err = %v, want a denial on %s", err, path)
		}
		v, _ = sarValidator(t, "pods/")
		if _, err := v.ValidateUpdate(requestContext(), cluster, updated); err != nil {
			t.Errorf("user allowed to create pods denied on %s: %v", path, err)
		}
	}

	v, asked := sarValidator(t)
	scaled := withClusterArg.DeepCopy()
	scaled.Spec.NodeGroups[0].Replicas = 2
	if _, err := v.ValidateUpdate(requestContext(), withClusterArg, scaled); err != nil || len(*asked) != 0 {
		t.Errorf("unchanged bootstrapConf re-checked: err = %v, asked = %+v", err, *asked)
	}
	empty := cluster.DeepCopy()
	empty.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{BootstrapConf: map[string]string{}}
	if _, err := v.ValidateUpdate(requestContext(), cluster, empty); err != nil || len(*asked) != 0 {
		t.Errorf("an empty bootstrapConf counted as a change: err = %v, asked = %+v", err, *asked)
	}
}

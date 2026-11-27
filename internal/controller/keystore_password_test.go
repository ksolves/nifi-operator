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
	"encoding/json"
	"strings"
	"testing"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// the keystore password reaches the pod only through a secretKeyRef, never the
// ConfigMap or the pod spec.
func TestKeystorePasswordStaysOutOfConfigMapAndPodSpec(t *testing.T) {
	singleUser := minimalCluster()
	singleUser.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}}
	for name, cluster := range map[string]*nifiv1alpha1.NiFiCluster{"single-user": singleUser} {
		t.Run(name, func(t *testing.T) {
			ng := &cluster.Spec.NodeGroups[0]
			cm, err := json.Marshal(buildConfigMap(cluster, ng).Data)
			if err != nil {
				t.Fatal(err)
			}
			sts, err := buildStatefulSet(cluster, ng, "hash", "", nifiPodServiceAccount(cluster))
			if err != nil {
				t.Fatal(err)
			}
			pod, err := json.Marshal(sts.Spec.Template)
			if err != nil {
				t.Fatal(err)
			}
			for what, text := range map[string]string{"ConfigMap": string(cm), "pod template": string(pod)} {
				for _, leak := range []string{"keystorePasswd", "truststorePasswd", "changeit", "keystore.p12:"} {
					if strings.Contains(text, leak) {
						t.Errorf("%s contains %q", what, leak)
					}
				}
			}

			var found bool
			for _, env := range sts.Spec.Template.Spec.Containers[0].Env {
				if env.Name != keystorePasswordEnv {
					continue
				}
				found = true
				ref := env.ValueFrom
				if env.Value != "" || ref == nil || ref.SecretKeyRef == nil ||
					ref.SecretKeyRef.Name != tlsCASecretName(cluster) || ref.SecretKeyRef.Key != TLSKeystorePasswordDataKey {
					t.Errorf("%s = %+v, want a secretKeyRef to %s/%s", keystorePasswordEnv, env, tlsCASecretName(cluster), TLSKeystorePasswordDataKey)
				}
			}
			if !found {
				t.Errorf("no %s env var on a TLS pod", keystorePasswordEnv)
			}
		})
	}

	// Plain HTTP: no keystore, no env var.
	cluster := minimalCluster()
	ng := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
	if env := tlsEnvVars(cluster, ng); env != nil {
		t.Errorf("tlsEnvVars() = %v on a plain-HTTP node group, want none", env)
	}
}

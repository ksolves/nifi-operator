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

// Package controller uses two distinct hashes to drive StatefulSet rollouts, each scoped
// differently: config-hash deliberately excludes resource fields, documented below rather than
// changed.
//
//   - configMapHash / configHashAnnotation: covers *only* the ConfigMap's Data - i.e. the
//     nifi.properties override content. Deliberately narrow: this is what
//     forces the pod template to visibly change (via the annotation's value) whenever the
//     rendered NiFi config itself changes, which is what actually triggers NiFi to pick up new
//     properties on restart.
//   - podTemplateHash / templateHashAnnotation: covers the *entire* built pod template - image,
//     env vars, volumes, and Resources (CPU/memory requests+limits) included, plus the
//     configHashAnnotation value itself (since that's baked into the pod template's own
//     annotations). This is what reconcileStatefulSet compares to decide whether the
//     StatefulSet needs updating at all (see its own doc comment for why comparing whole
//     objects instead causes an infinite update loop).
//
// A NodeGroup.Resources change is correctly picked up and rolled out - not because it's part of
// configMapHash (it isn't, and doesn't need to be), but because it changes podTemplateHash, which
// changes what reconcileStatefulSet writes to the StatefulSet's Spec.Template, which Kubernetes'
// own StatefulSet controller then rolls out natively - the same mechanism that would pick up any
// other pod template field, with or without a NiFi-specific "config hash" involved at all.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// configHashAnnotation is set on a StatefulSet's pod template to the hash of the ConfigMap its
// pods mount. Changing the ConfigMap changes this annotation, which changes the pod template,
// which makes the StatefulSet controller roll the pods on its own - no manual eviction needed.
const configHashAnnotation = "nifi.ksolves.com/config-hash"

// authHashAnnotation is set on a StatefulSet's pod template to referencedAuthSecretHash of the
// Secret backing the single-user login, for the exact same reason configHashAnnotation exists:
// authEnvVars only ever changes a pod's env vars by SecretKeyRef *name*, never by value, so a
// credential change would otherwise leave podTemplateHash unchanged (Kubernetes doesn't
// live-refresh env-var-sourced Secret values into a running container either) - silently
// leaving already-running pods authenticating with the stale password. The hash is derived from
// the Secret's UID and resourceVersion, never its contents, so nothing password-derived ever
// appears on the pod template. In literal Username/Password mode that Secret is the
// operator-owned authSecretName one (watched via Owns); in SecretName mode it's the
// user-managed one (watched via clustersReferencingSecret). Either way, a write to the
// Secret rolls the pods.
const authHashAnnotation = "nifi.ksolves.com/auth-hash"

// templateHashAnnotation is set on the StatefulSet itself (not the pod template) to a hash of
// the fully-built pod template we submitted. See podTemplateHash for why this exists.
const templateHashAnnotation = "nifi.ksolves.com/template-hash"

// podTemplateHash returns a stable hash of a pod template, computed purely from our own
// freshly-built Go struct before it's ever submitted to the API server.
//
// This exists to answer "does the StatefulSet actually need updating" without comparing our
// desired template against the server's stored one field-by-field: the API server fills in Pod
// spec defaults we never set ourselves (a readinessProbe's successThreshold, a container's
// terminationMessagePath, and dozens more), so a freshly-built template will always look
// "different" from the server's already-decorated version even when nothing meaningful changed.
// Comparing whole objects that way causes an infinite update loop - confirmed live, at 40-60+
// writes/sec, before this fix existed. Hashing only our own pre-submission struct and comparing
// hash-to-hash (previously-stored vs freshly-computed) sidesteps server defaulting entirely: the
// hash of what WE built is deterministic and depends only on what WE control.
func podTemplateHash(template corev1.PodTemplateSpec) string {
	// encoding/json is deterministic for identical input: struct fields serialize in a fixed
	// order (their declaration order) and map keys are sorted alphabetically, so two calls with
	// equal content always produce byte-identical output regardless of Go's randomized map
	// iteration order.
	data, err := json.Marshal(template)
	if err != nil {
		// template is built entirely from types that always marshal cleanly (no channels,
		// funcs, or cycles) - a marshal error here would mean a real bug in buildStatefulSet,
		// not a runtime condition to recover from gracefully.
		panic(fmt.Sprintf("marshaling pod template for hashing: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// referencedAuthSecretHash returns a stable hash identifying one specific version of the
// authentication Secret. resourceVersion changes on every write to the Secret, and
// the UID changes if it is deleted and recreated under the same name, so either kind of rotation
// yields a new value.
func referencedAuthSecretHash(secret *corev1.Secret) string {
	sum := sha256.Sum256([]byte(string(secret.UID) + "/" + secret.ResourceVersion))
	return hex.EncodeToString(sum[:])
}

// certSANsHashAnnotation records, on the pod template, a hash of the certificate SANs every pod
// of a TLS node group shares (see sharedCertificateSANs).
const certSANsHashAnnotation = "nifi.ksolves.com/cert-sans-hash"

// entrypointHashAnnotation records, on the pod template, a hash of everything the entrypoint
// Secret holds (see entrypointSecretHash): the embedded entrypoint script, bootstrap.conf.overrides
// and any logback.xml/authorizers.xml replacement. The volume references that Secret by name
// only, which never changes, so either a new operator release that changes the script, or a
// spec change to one of those overrides, would otherwise update the Secret without the pod
// template visibly changing - silently leaving already-running pods on the old content, the
// same class of gap authHashAnnotation exists to close for env-var-sourced Secret values
// (NIFI-248 PR #210 review).
const entrypointHashAnnotation = "nifi.ksolves.com/entrypoint-hash"

// stringsHash returns a stable hash of an ordered list of strings.
func stringsHash(values []string) string {
	h := sha256.New()
	for _, v := range values {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// configMapHash returns a stable hash of a ConfigMap's Data, independent of key iteration order.
func configMapHash(cm *corev1.ConfigMap) string {
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(withoutRestartExemptProperties(cm.Data[k])))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// restartExemptNifiProperties are nifi.properties keys left out of configMapHash. They only matter
// when a NiFi node starts and every node already reads the current ConfigMap at its own start, so
// a change to them must not roll the whole cluster. nifi.cluster.flow.election.max.candidates is
// derived from the total replica count: hashing it would restart every existing node on each
// scale up or down, taking the cluster's UI down for minutes for no benefit.
var restartExemptNifiProperties = []string{nifiPropClusterFlowElectionMaxCand}

// withoutRestartExemptProperties drops restartExemptNifiProperties lines from a rendered
// properties file.
func withoutRestartExemptProperties(content string) string {
	lines := strings.Split(content, "\n")
	kept := lines[:0]
	for _, line := range lines {
		exempt := false
		for _, key := range restartExemptNifiProperties {
			if strings.HasPrefix(line, key+"=") {
				exempt = true
				break
			}
		}
		if !exempt {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

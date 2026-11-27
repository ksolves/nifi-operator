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
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// sensitivePropsKeyDataKey is the Secret Data key holding the generated nifi.sensitive.props.key
// value.
const sensitivePropsKeyDataKey = "sensitive-props-key"

// generateSensitivePropsKey returns a cryptographically random hex string well above NiFi's
// 12-character minimum for nifi.sensitive.props.key.
func generateSensitivePropsKey() (string, error) {
	buf := make([]byte, 16) // 32 hex characters
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random sensitive props key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// buildSensitivePropsSecret renders the cluster's Secret holding nifi.sensitive.props.key. key is
// either freshly generated or adopted from a legacy per-node-group Secret. Callers must only use
// this to *create* the Secret, never to overwrite an existing one: rotating the key would break
// decryption of anything NiFi already encrypted with the old one.
func buildSensitivePropsSecret(cluster *nifiv1alpha1.NiFiCluster, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sensitivePropsSecretName(cluster),
			Namespace: cluster.Namespace,
			// Cluster-scoped, so no node-group label: pruneRemovedNodeGroups must never delete it.
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		StringData: map[string]string{
			sensitivePropsKeyDataKey: key,
		},
	}
}

// buildConfigSecret renders the operator-owned Secret holding a node group's Secret-sourced
// nifi.properties overrides, in the same overrides format as the ConfigMap's
// nifi.properties.overrides entry. Data (not StringData) so an unchanged Secret compares equal and
// isn't rewritten on every reconcile.
func buildConfigSecret(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup, props map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        configSecretName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      commonLabels(cluster, nodeGroup),
			Annotations: mergedAnnotations(cluster, nodeGroup),
		},
		Data: map[string][]byte{nifiPropertiesOverridesKey: []byte(renderProperties(props))},
	}
}

// buildEntrypointSecret renders the operator-owned Secret holding a node group's entrypoint
// script - see entrypointSecretName for why this is a Secret, not part of the node group's
// ConfigMap - plus bootstrap.conf.overrides and any logback.xml/authorizers.xml replacement,
// for the same reason (NIFI-248 B2): all three are things a compromised pod from another
// cluster could otherwise rewrite via the ConfigMap's broad Role to run code in, or change the
// access policy of, this cluster's pods.
func buildEntrypointSecret(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *corev1.Secret {
	data := map[string][]byte{
		entrypointKey:              []byte(nifiEntrypoint),
		"bootstrap.conf.overrides": []byte(buildBootstrapConfOverrides(cluster, nodeGroup)),
	}
	// logback.xml/authorizers.xml are full-file replacements, not key=value overrides (see
	// ConfigOverridesSpec.Logback/Authorizers' own doc comment) - the Secret key is only added
	// when a replacement was actually supplied, so the entrypoint's "does this file exist" check
	// correctly distinguishes "no replacement requested" from "replace with this content".
	if logback := effectiveLogback(cluster, nodeGroup); logback != nil {
		data["logback.xml.override"] = []byte(*logback)
	}
	if authorizers := effectiveAuthorizers(cluster, nodeGroup); authorizers != nil {
		data["authorizers.xml.override"] = []byte(*authorizers)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        entrypointSecretName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      commonLabels(cluster, nodeGroup),
			Annotations: mergedAnnotations(cluster, nodeGroup),
		},
		Data: data,
	}
}

// entrypointSecretHash returns a stable hash of everything buildEntrypointSecret puts in the
// Secret, for entrypointHashAnnotation: that Secret is referenced by the pod template only by
// name (which never changes), so without this a changed bootstrap.conf.overrides or
// logback.xml/authorizers.xml replacement would update the Secret without rolling the pods onto
// it - the same class of gap entrypointHashAnnotation exists to close for the entrypoint script
// itself (NIFI-248 PR #210 review). Presence-prefixed so toggling a replacement on or off changes
// the hash even when the new content happens to be empty.
func entrypointSecretHash(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	values := []string{nifiEntrypoint, buildBootstrapConfOverrides(cluster, nodeGroup)}
	if logback := effectiveLogback(cluster, nodeGroup); logback != nil {
		values = append(values, "logback:"+*logback)
	}
	if authorizers := effectiveAuthorizers(cluster, nodeGroup); authorizers != nil {
		values = append(values, "authorizers:"+*authorizers)
	}
	return stringsHash(values)
}

// authSecretUsernameKey/authSecretPasswordKey are the Secret Data keys this operator both reads
// (from a user-supplied spec.authentication.singleUser.secretName) and writes (into its own
// synthesized authSecretName Secret, built from literal Username/Password fields) - matching the
// key names a user's own hand-written Secret is expected to use.
const (
	authSecretUsernameKey = "username"
	authSecretPasswordKey = "password"
)

// defaultAuthUsername is the login username the operator assigns when spec.authentication is
// left unset entirely. Only the password needs to be unique/secret; a fixed, well-known username
// is one less thing an operator has to look up before logging in.
const defaultAuthUsername = "admin"

// generateRandomPassword returns a cryptographically random hex string well above NiFi's
// 12-character single-user-login minimum - same approach as generateSensitivePropsKey.
func generateRandomPassword() (string, error) {
	buf := make([]byte, 16) // 32 hex characters
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random auth password: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// effectiveAuthSecretName returns the name of whichever Secret actually backs the cluster's NiFi
// login right now - the user's own Secret when SecretName is set, or the operator's own
// synthesized one (authSecretName) otherwise. Mirrors authEnvVars' own secretName selection
// (internal/controller/statefulset.go) exactly, so NiFiCluster.Status.AuthSecretName always
// names the same Secret the running pods actually authenticate against.
func effectiveAuthSecretName(cluster *nifiv1alpha1.NiFiCluster) string {
	auth := cluster.Spec.Authentication
	if auth != nil && auth.SingleUser != nil && auth.SingleUser.SecretName != "" {
		return auth.SingleUser.SecretName
	}
	return authSecretName(cluster)
}

// buildDefaultAuthSecret renders the Secret the operator synthesizes when
// spec.authentication is left unset entirely, so every node in the cluster shares one
// operator-generated login instead of each node falling back to NiFi's own per-node random
// single-user credentials (which differ node to node, making a multi-node cluster's UI
// effectively impossible to log into consistently). Callers must only use this to *create* the
// Secret, never to overwrite an existing one - like buildSensitivePropsSecret, rotating the
// password here would log out whoever is already using it, with no CR field to tell them it
// changed.
func buildDefaultAuthSecret(cluster *nifiv1alpha1.NiFiCluster) (*corev1.Secret, error) {
	password, err := generateRandomPassword()
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        authSecretName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		StringData: map[string]string{
			authSecretUsernameKey: defaultAuthUsername,
			authSecretPasswordKey: password,
		},
	}, nil
}

// buildAuthSecret renders the Secret the operator synthesizes from
// spec.authentication.singleUser's literal Username/Password fields. Unlike
// buildSensitivePropsSecret/buildTLSCASecret, this is NOT create-once: the CR's literal fields
// are the source of truth, so this Secret must track them - if the user edits the password in
// the CR, the running cluster's actual login credentials should follow, not stay stuck on
// whatever was first generated.
func buildAuthSecret(cluster *nifiv1alpha1.NiFiCluster, auth *nifiv1alpha1.SingleUserAuthSpec) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authSecretName(cluster),
			Namespace: cluster.Namespace,
			// Cluster-scoped, not per-node-group (see authSecretName) - same reasoning as
			// buildTLSCASecret's label choice.
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		// Data, not StringData: the server stores only Data, so StringData never compares equal and
		// the Secret was rewritten on every reconcile.
		Data: map[string][]byte{
			authSecretUsernameKey: []byte(auth.Username),
			authSecretPasswordKey: []byte(auth.Password),
		},
	}
}

// ensureSecretMetadata adds or corrects labels and annotations on an existing create-once Secret
// (sensitive-props key, TLS CA, default login), which is otherwise never written again: an
// upgrade that adds a label, or a change to spec.labels/spec.annotations, would never reach it.
// Only the given keys are patched. Data, ownerReferences and anything else set on the
// Secret are left alone, and nothing is written when they already match. A merge patch carries
// only the changed metadata, so a cached copy whose Data was stripped (CacheByObject) is safe.
func (r *NiFiClusterReconciler) ensureSecretMetadata(
	ctx context.Context, secret *corev1.Secret, labels, annotations map[string]string,
) error {
	if subsetOf(labels, secret.Labels) && subsetOf(annotations, secret.Annotations) {
		return nil
	}
	base := secret.DeepCopy()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	maps.Copy(secret.Labels, labels)
	if len(annotations) > 0 {
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		maps.Copy(secret.Annotations, annotations)
	}
	if err := r.Patch(ctx, secret, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating labels of Secret %q: %w", secret.Name, err)
	}
	return nil
}

// subsetOf reports whether every key of want is in have with the same value.
func subsetOf(want, have map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

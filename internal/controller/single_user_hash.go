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
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// The single-user login's bcrypt hash. The entrypoint writes it straight into
// login-identity-providers.xml instead of running "nifi.sh set-single-user-credentials <user>
// <password>", which put the password in the argv of a process any user on the node could list.
const (
	// singleUserHashKey is the Data key holding the bcrypt hash of the login password.
	singleUserHashKey = "password-bcrypt"
	// singleUserHashSourceAnnotation records which version of the authentication Secret (see
	// referencedAuthSecretHash) the hash was made from, so an unchanged Secret costs no bcrypt
	// comparison on every reconcile.
	singleUserHashSourceAnnotation = "nifi.ksolves.com/hashed-from"
)

// singleUserHashCost matches the cost NiFi's own set-single-user-credentials uses. A variable only
// so the tests can lower it: at cost 12 each hash takes seconds under the race detector, and every
// test cluster with a single-user login hashes its password.
var singleUserHashCost = 12

// singleUserHashSecretName returns the name of the operator-owned Secret holding the login's
// bcrypt hash. One per cluster, like authSecretName.
func singleUserHashSecretName(cluster *nifiv1alpha1.NiFiCluster) string {
	return fmt.Sprintf("%s-single-user-hash", cluster.Name)
}

// buildSingleUserHashSecret renders the hash Secret for a login made from the authentication
// Secret version sourceVersion.
func buildSingleUserHashSecret(cluster *nifiv1alpha1.NiFiCluster, username, hash []byte, sourceVersion string) *corev1.Secret {
	annotations := maps.Clone(cluster.Spec.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[singleUserHashSourceAnnotation] = sourceVersion
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        singleUserHashSecretName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: annotations,
		},
		Data: map[string][]byte{
			authSecretUsernameKey: username,
			singleUserHashKey:     hash,
		},
	}
}

// singleUserHash returns the bcrypt hash to store for password: existing when it still matches
// (bcrypt is salted, so hashing again would change the Secret for nothing), a new one otherwise.
func singleUserHash(existing, password []byte) ([]byte, error) {
	if len(existing) > 0 && bcrypt.CompareHashAndPassword(existing, password) == nil {
		return existing, nil
	}
	hash, err := bcrypt.GenerateFromPassword(password, singleUserHashCost)
	if err != nil {
		return nil, fmt.Errorf("hashing the single-user password: %w", err)
	}
	// Go writes the "$2a$" prefix. NiFi's verifier only accepts the "$2b$" its own command writes
	// (checked against NiFi 2.12's BCryptPasswordEncoder: the same hash fails with "$2a$" and
	// matches with "$2b$"). The algorithm is the same, so only the prefix changes, and Go
	// still compares the result.
	return bytes.Replace(hash, []byte("$2a$"), []byte("$2b$"), 1), nil
}

// reconcileLoginSecrets reconciles the login Secret (reconcileAuthSecret) and then its hash
// (reconcileSingleUserHashSecret), with an event when a same-named Secret the operator didn't
// create is in the way.
func (r *NiFiClusterReconciler) reconcileLoginSecrets(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	for _, step := range []struct {
		name      string
		reconcile func(context.Context, *nifiv1alpha1.NiFiCluster) error
		what      string
	}{
		{authSecretName(cluster), r.reconcileAuthSecret, "auth Secret"},
		{singleUserHashSecretName(cluster), r.reconcileSingleUserHashSecret, "single-user hash Secret"},
	} {
		if err := step.reconcile(ctx, cluster); err != nil {
			if errors.Is(err, errNotOwned) {
				r.event(cluster, corev1.EventTypeWarning, "AuthSecretNotOwned", fmt.Sprintf(
					"Secret %q exists but was not created by the operator, so it is neither used nor overwritten; "+
						"rename or remove it", step.name))
			}
			return fmt.Errorf("reconciling %s: %w", step.what, err)
		}
	}
	return nil
}

// reconcileSingleUserHashSecret keeps the hash Secret in step with the authentication Secret
// (effectiveAuthSecretName). It runs after
// reconcileAuthSecret and before the StatefulSets, so pods restarted for a new password read the
// matching hash.
func (r *NiFiClusterReconciler) reconcileSingleUserHashSecret(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	key := client.ObjectKey{Name: singleUserHashSecretName(cluster), Namespace: cluster.Namespace}
	source := &corev1.Secret{}
	sourceName := effectiveAuthSecretName(cluster)
	// The cached copy: enough to tell from its UID and resourceVersion whether it changed.
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: sourceName}, source)
	if apierrors.IsNotFound(err) {
		// The pods can't start without it either; its creation triggers a reconcile.
		log.FromContext(ctx).Info("authentication Secret not found; not hashing the login yet", "secretName", sourceName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting authentication Secret %q: %w", sourceName, err)
	}
	sourceVersion := referencedAuthSecretHash(source)

	existing := &corev1.Secret{}
	found, err := r.getOwned(ctx, cluster, key, existing)
	if err != nil {
		return err
	}
	username, hash := existing.Data[authSecretUsernameKey], existing.Data[singleUserHashKey]
	if !found || existing.Annotations[singleUserHashSourceAnnotation] != sourceVersion {
		// The cache strips the data of Secrets the operator didn't create (CacheByObject), such as
		// the user's own singleUser.secretName one: read the credentials uncached, only when the
		// Secret changed.
		if err := r.secretReader().Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
			return fmt.Errorf("reading authentication Secret %q: %w", sourceName, err)
		}
		sourceVersion = referencedAuthSecretHash(source)
		password := source.Data[authSecretPasswordKey]
		if len(password) == 0 {
			// The pods can't start without the key either; the webhook reports it on admission.
			log.FromContext(ctx).Info("authentication Secret has no password; not hashing the login yet",
				"secretName", sourceName)
			return nil
		}
		username = source.Data[authSecretUsernameKey]
		if hash, err = singleUserHash(hash, password); errors.Is(err, bcrypt.ErrPasswordTooLong) {
			// Not a reconcile error, which would stop everything after it: the webhook rejects
			// such a password, so only a Secret edited after admission gets here.
			r.event(cluster, corev1.EventTypeWarning, "AuthPasswordTooLong", fmt.Sprintf(
				"The password in Secret %q is longer than 72 bytes, which NiFi cannot store; "+
					"the login keeps its previous password until it is shortened", sourceName))
			return nil
		} else if err != nil {
			return err
		}
	}

	secret := buildSingleUserHashSecret(cluster, username, hash, sourceVersion)
	return r.createOrUpdateOwned(ctx, cluster, secret, func(obj client.Object) {
		obj.SetLabels(secret.Labels)
		obj.SetAnnotations(secret.Annotations)
		obj.(*corev1.Secret).Data = secret.Data
	})
}

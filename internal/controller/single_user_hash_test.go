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
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// The cheapest bcrypt cost for the whole test binary, envtest included: cost 12 makes every test
// cluster with a single-user login wait seconds for its hash under -race.
func init() {
	singleUserHashCost = bcrypt.MinCost
}

func TestSingleUserHashUsesNiFiPrefixAndVerifies(t *testing.T) {
	password := []byte("correct-horse-battery")
	hash, err := singleUserHash(nil, password)
	if err != nil {
		t.Fatalf("singleUserHash() error = %v", err)
	}
	if !bytes.HasPrefix(hash, []byte("$2b$")) {
		t.Errorf("singleUserHash() = %q, want prefix %q", hash, "$2b$")
	}
	if err := bcrypt.CompareHashAndPassword(hash, password); err != nil {
		t.Errorf("CompareHashAndPassword() = %v, want the hash to verify the password", err)
	}
}

func TestSingleUserHashKeepsMatchingHash(t *testing.T) {
	password := []byte("correct-horse-battery")
	first, err := singleUserHash(nil, password)
	if err != nil {
		t.Fatalf("singleUserHash() error = %v", err)
	}
	second, err := singleUserHash(first, password)
	if err != nil {
		t.Fatalf("singleUserHash() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("singleUserHash() = %q, want the matching existing hash %q kept", second, first)
	}
}

func TestSingleUserHashRehashesOnPasswordChange(t *testing.T) {
	existing, err := singleUserHash(nil, []byte("password-a"))
	if err != nil {
		t.Fatalf("singleUserHash() error = %v", err)
	}
	newPassword := []byte("password-b")
	hash, err := singleUserHash(existing, newPassword)
	if err != nil {
		t.Fatalf("singleUserHash() error = %v", err)
	}
	if bytes.Equal(hash, existing) {
		t.Fatalf("singleUserHash() kept the old hash %q after a password change", existing)
	}
	if err := bcrypt.CompareHashAndPassword(hash, newPassword); err != nil {
		t.Errorf("CompareHashAndPassword() = %v, want the new hash to verify the new password", err)
	}
}

func TestAuthEnvVarsIncludePasswordHash(t *testing.T) {
	cluster := minimalCluster()

	env := authEnvVars(cluster)
	hash := findEnvVar(env, "NIFI_AUTH_PASSWORD_HASH")
	if hash == nil || hash.ValueFrom == nil || hash.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("authEnvVars() = %+v, want NIFI_AUTH_PASSWORD_HASH from a Secret", env)
	}
	wantSecret := singleUserHashSecretName(cluster)
	if hash.ValueFrom.SecretKeyRef.Name != wantSecret || hash.ValueFrom.SecretKeyRef.Key != singleUserHashKey {
		t.Errorf("NIFI_AUTH_PASSWORD_HASH secretKeyRef = %+v, want Secret %q key %q", hash.ValueFrom.SecretKeyRef, wantSecret, singleUserHashKey)
	}
}

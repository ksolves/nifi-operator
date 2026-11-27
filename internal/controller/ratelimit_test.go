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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A NiFiCluster that keeps failing backs off exponentially, never beyond maxReconcileBackoff, and
// starts again from the bottom once it succeeds.
func TestReconcileRateLimiterCapsBackoff(t *testing.T) {
	limiter := reconcileRateLimiter()
	failing := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "bad"}}
	other := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "good"}}

	var last time.Duration
	for i := range 40 {
		d := limiter.When(failing)
		if d > maxReconcileBackoff {
			t.Fatalf("failure %d: backoff %s exceeds the %s cap", i+1, d, maxReconcileBackoff)
		}
		if d < last {
			t.Fatalf("failure %d: backoff dropped from %s to %s", i+1, last, d)
		}
		last = d
	}
	if last != maxReconcileBackoff {
		t.Fatalf("backoff after 40 failures = %s, want the %s cap", last, maxReconcileBackoff)
	}

	if d := limiter.When(other); d > time.Second {
		t.Fatalf("another cluster inherited the backoff: %s", d)
	}

	limiter.Forget(failing)
	if d := limiter.When(failing); d > time.Second {
		t.Fatalf("backoff after Forget = %s, want it reset", d)
	}
}

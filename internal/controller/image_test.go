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

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func TestParseImage(t *testing.T) {
	tests := []struct {
		ref, repository, tag string
		wantErr              bool
	}{
		{ref: "repo/nifi:2.12.0", repository: "repo/nifi", tag: "2.12.0"},
		{ref: "host:5000/nifi:t", repository: "host:5000/nifi", tag: "t"},
		{ref: "repo/nifi", wantErr: true},
		{ref: "repo/nifi:", wantErr: true},
		{ref: ":tag", wantErr: true},
		{ref: "host:5000/nifi", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseImage(tt.ref)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseImage(%q) = %+v, want error", tt.ref, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseImage(%q) error: %v", tt.ref, err)
			continue
		}
		if got.Repository != tt.repository || got.Tag != tt.tag {
			t.Errorf("ParseImage(%q) = %q:%q, want %q:%q", tt.ref, got.Repository, got.Tag, tt.repository, tt.tag)
		}
	}
}

func TestNiFiVersionFromTag(t *testing.T) {
	tests := []struct {
		tag  string
		want [3]int
		ok   bool
	}{
		{tag: "2.12.0", want: [3]int{2, 12, 0}, ok: true},
		{tag: "nifi-node-2.12.0-0.12.0", want: [3]int{2, 12, 0}, ok: true},
		{tag: "2.9.10", want: [3]int{2, 9, 10}, ok: true},
		{tag: "latest"},
		{tag: ""},
	}
	for _, tt := range tests {
		got, ok := NiFiVersionFromTag(tt.tag)
		if ok != tt.ok || got != tt.want {
			t.Errorf("NiFiVersionFromTag(%q) = %v, %v, want %v, %v", tt.tag, got, ok, tt.want, tt.ok)
		}
	}
}

func TestVersionLess(t *testing.T) {
	tests := []struct {
		a, b [3]int
		want bool
	}{
		{a: [3]int{2, 11, 0}, b: [3]int{2, 11, 0}, want: false},
		{a: [3]int{2, 10, 9}, b: [3]int{2, 11, 0}, want: true},
		{a: [3]int{2, 12, 0}, b: [3]int{2, 11, 0}, want: false},
		{a: [3]int{3, 0, 0}, b: [3]int{2, 11, 0}, want: false},
		{a: [3]int{1, 99, 99}, b: [3]int{2, 0, 0}, want: true},
	}
	for _, tt := range tests {
		if got := VersionLess(tt.a, tt.b); got != tt.want {
			t.Errorf("VersionLess(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNiFiImage(t *testing.T) {
	tests := []struct {
		name  string
		image nifiv1alpha1.ImageSpec
		want  string
	}{
		{name: "unset uses the default", want: BuiltinNiFiImage},
		{name: "spec image", image: nifiv1alpha1.ImageSpec{Repository: "repo/nifi", Tag: "2.12.0"}, want: "repo/nifi:2.12.0"},
	}
	for _, tt := range tests {
		cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{Image: tt.image}}
		if got := nifiImage(cluster); got != tt.want {
			t.Errorf("%s: nifiImage() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

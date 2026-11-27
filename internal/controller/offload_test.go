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
)

func TestParseQueuedBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"0 / 0 bytes":       0,
		"12 / 512 bytes":    512,
		"120 / 4 KB":        4 << 10,
		"3 / 1.5 MB":        3 << 19,
		"1,024 / 2 GB":      2 << 30,
		"9 / 1,536.5 bytes": 1536,
		"5 / 1 TB":          1 << 40,
	} {
		if got, ok := parseQueuedBytes(in); !ok || got != want {
			t.Errorf("parseQueuedBytes(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "12", "12 / ", "12 / x MB", "12 / 4 PB", "12 / -1 KB"} {
		if _, ok := parseQueuedBytes(in); ok {
			t.Errorf("parseQueuedBytes(%q) ok, want not ok", in)
		}
	}
}

// estimatedOffloadSeconds is purely informational now (NIFI-248 B4: stepScaleDown waits for a
// confirmed OFFLOADED state rather than racing a termination grace period), used only to flesh
// out the OffloadStarted event with how long an in-progress offload is likely to take.
func TestEstimatedOffloadSeconds(t *testing.T) {
	for _, c := range []struct {
		name       string
		throughput int64
		queued     string
		wantOK     bool
		wantSecs   int64
	}{
		{"default throughput: 2 GiB at 10 MiB/s", 0, "9000 / 2 GB", true, 205},
		{"configured throughput: 100 MiB at 100 MiB/s", 100 << 20, "10 / 100 MB", true, 1},
		{"unreadable figure", 0, "?", false, 0},
	} {
		secs, ok := estimatedOffloadSeconds(c.throughput, c.queued)
		if ok != c.wantOK || (ok && secs != c.wantSecs) {
			t.Errorf("%s: estimatedOffloadSeconds(%d, %q) = %d, %v; want %d, %v", c.name, c.throughput, c.queued, secs, ok, c.wantSecs, c.wantOK)
		}
	}
}

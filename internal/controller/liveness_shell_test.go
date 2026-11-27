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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// the startup/liveness script passes on any HTTP answer and fails only when nothing
// answers, and never logs in. Run under a real sh with a stub curl, like the readiness script.
func TestWebServerProbeScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, c := range []struct {
		name  string
		tls   bool
		code  string // what the stub curl prints for -w '%{http_code}'
		alive bool
	}{
		{"ok", false, "200", true},
		{"unauthenticated under TLS", true, "401", true},
		{"still starting", true, "503", true},
		{"nothing listening", false, "000", false},
		{"curl printed nothing", true, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default"}
			if c.tls {
				nodeGroup.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			}
			curl := "#!/bin/sh\necho \"$*\" >> " + dir + "/calls\nprintf '%s' '" + c.code + "'\n"
			for name, b := range map[string]string{"curl": curl, "hostname": "#!/bin/sh\necho node\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(b), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(sh, "-c", webServerProbeHandler(nodeGroup).Exec.Command[2])
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "NIFI_AUTH_USERNAME=admin", "NIFI_AUTH_PASSWORD=secret"}
			if alive := cmd.Run() == nil; alive != c.alive {
				t.Errorf("alive = %v, want %v", alive, c.alive)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Count(string(calls), "\n") != 1 || strings.Contains(string(calls), "access/token") {
				t.Errorf("want exactly one unauthenticated call, got %q", calls)
			}
			wantScheme := "http://"
			if c.tls {
				wantScheme = "https://"
			}
			if !strings.Contains(string(calls), wantScheme) {
				t.Errorf("call %q does not use %s", calls, wantScheme)
			}
		})
	}
}

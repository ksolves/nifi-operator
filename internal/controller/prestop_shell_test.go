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

// TestPreStopScriptControlFlow runs the generated preStop script under a real POSIX shell with
// curl and nifi.sh stubbed, and checks which NiFi command it ends up running.
func TestPreStopScriptControlFlow(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	tls := &nifiv1alpha1.NodeGroup{Name: "ng", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}
	script := preStopHandler(tls).Exec.Command[2]
	script = strings.ReplaceAll(script, nifiHomePath+"/bin/nifi.sh", "nifish")

	nodes := func(n int) string {
		return `{"cluster":{"nodes":[` + strings.TrimSuffix(strings.Repeat(`{"status":"CONNECTED"},`, n), ",") + `]}}`
	}
	cases := []struct {
		name, clusterBody, decommissionExit, wantLog string
		creds                                        bool
	}{
		{"last connected node stops at once", nodes(1), "0", "stop", true},
		{"several nodes decommission", nodes(3), "0", "decommission", true},
		{"failing decommission falls back to stop", nodes(3), "1", "decommission decommission decommission decommission decommission stop", true},
		{"unreadable cluster view still decommissions, briefly", "", "1", "decommission decommission stop", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			script := strings.ReplaceAll(script, "/tmp/", dir+"/")
			logFile := filepath.Join(dir, "log")
			// curl: the token call answers a token only with credentials; the cluster call needs it.
			curl := `#!/bin/sh
args="$*"; prev=""
for a in "$@"; do if [ "$prev" = "-H" ]; then case "$a" in @*) args="$args $(cat "${a#@}")" ;; esac; fi; prev="$a"; done
case "$args" in
*access/token*) echo TOKEN ;;
*controller/cluster*) case "$args" in *"Bearer TOKEN"*) printf '%s' '` + c.clusterBody + `' ;; esac ;;
esac
`
			nifish := "#!/bin/sh\necho -n \"$1 \" >> " + logFile + "\n[ \"$1\" = decommission ] && exit " + c.decommissionExit + "\nexit 0\n"
			for name, body := range map[string]string{"curl": curl, "nifish": nifish, "hostname": "#!/bin/sh\necho node\n", "sleep": "#!/bin/sh\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(sh, "-c", script)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
			if c.creds {
				cmd.Env = append(cmd.Env, "NIFI_AUTH_USERNAME=admin", "NIFI_AUTH_PASSWORD=secret-password")
			}
			_ = cmd.Run()
			got, _ := os.ReadFile(logFile)
			if strings.TrimSpace(string(got)) != c.wantLog {
				t.Errorf("nifi.sh calls = %q, want %q", strings.TrimSpace(string(got)), c.wantLog)
			}
		})
	}
}

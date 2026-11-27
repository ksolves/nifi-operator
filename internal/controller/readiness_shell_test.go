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

// TestReadinessProbeScript runs the generated readiness probe under a real POSIX shell with curl
// stubbed. The stub answers the cluster summary only to the bearer token "TOKEN" on TLS, which
// is what NiFi does under the single-user authorizer.
func TestReadinessProbeScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	const connected, disconnected = `{"clusterSummary":{"connectedToCluster":true}}`, `{"clusterSummary":{"connectedToCluster":false}}`
	cases := []struct {
		name        string
		tls, creds  bool
		cachedToken string
		summary     string
		loginWorks  bool
		wantReady   bool
		wantLogins  int
	}{
		{name: "http connected", summary: connected, wantReady: true},
		{name: "http disconnected", summary: disconnected},
		{name: "tls logs in, then connected", tls: true, creds: true, summary: connected, loginWorks: true, wantReady: true, wantLogins: 1},
		{name: "tls cached token skips login", tls: true, creds: true, cachedToken: "TOKEN", summary: connected, loginWorks: true, wantReady: true},
		{name: "tls stale token re-logs in", tls: true, creds: true, cachedToken: "OLD", summary: connected, loginWorks: true, wantReady: true, wantLogins: 1},
		{name: "tls logged in but disconnected", tls: true, creds: true, summary: disconnected, loginWorks: true, wantLogins: 1},
		{name: "tls 401 without credentials is not ready", tls: true, summary: connected},
		{name: "tls failed login is not ready", tls: true, creds: true, summary: connected, wantLogins: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng"}
			if c.tls {
				nodeGroup.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			}
			script := strings.ReplaceAll(readinessProbeHandler(nodeGroup).Exec.Command[2], "/tmp/", dir+"/")
			if c.cachedToken != "" {
				if err := os.WriteFile(filepath.Join(dir, "readiness-probe-auth"), []byte("Authorization: Bearer "+c.cachedToken+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			token := ""
			if c.loginWorks {
				token = "TOKEN"
			}
			authorized := `*` // plain HTTP needs no token
			if c.tls {
				authorized = `*"Bearer TOKEN"*`
			}
			// The stub writes the response body to the -o file and prints the status code, like
			// curl -o FILE -w '%{http_code}'.
			curl := `#!/bin/sh
args="$*"; prev=""
for a in "$@"; do if [ "$prev" = "-H" ]; then case "$a" in @*) args="$args $(cat "${a#@}")" ;; esac; fi; prev="$a"; done
case "$args" in
*access/token*) echo login >> ` + dir + `/logins; printf '%s' '` + token + `'; exit 0 ;;
esac
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
case "$args" in
` + authorized + `) printf '%s' '` + c.summary + `' > "$out"; printf 200 ;;
*) : > "$out"; printf 401 ;;
esac
`
			for name, b := range map[string]string{"curl": curl, "hostname": "#!/bin/sh\necho node\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(b), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(sh, "-c", script)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
			if c.creds {
				cmd.Env = append(cmd.Env, "NIFI_AUTH_USERNAME=admin", "NIFI_AUTH_PASSWORD=secret-password")
			}
			ready := cmd.Run() == nil
			if ready != c.wantReady {
				t.Errorf("ready = %v, want %v", ready, c.wantReady)
			}
			logins, _ := os.ReadFile(filepath.Join(dir, "logins"))
			if got := strings.Count(string(logins), "login"); got != c.wantLogins {
				t.Errorf("logins = %d, want %d", got, c.wantLogins)
			}
		})
	}
}

// the bearer token reaches curl through a header file, never its arguments.
func TestProbeAndPreStopKeepTokenOffCommandLine(t *testing.T) {
	tls := &nifiv1alpha1.NodeGroup{Name: "ng", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}}
	for name, script := range map[string]string{
		"readiness": readinessProbeHandler(tls).Exec.Command[2],
		"preStop":   preStopHandler(tls).Exec.Command[2],
	} {
		if strings.Contains(script, `-H "Authorization`) || strings.Contains(script, `-H "$auth"`) {
			t.Errorf("%s passes the Authorization header as a curl argument", name)
		}
		if !strings.Contains(script, "-H @/tmp/") {
			t.Errorf("%s does not read the header from a file", name)
		}
	}
}

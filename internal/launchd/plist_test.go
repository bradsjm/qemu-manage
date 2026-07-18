package launchd

import (
	"bytes"
	"strings"
	"testing"

	"qemu-manage/internal/model"
)

func TestRenderPlistScopesPoliciesAndEscaping(t *testing.T) {
	for _, scope := range []model.AutostartScope{model.AutostartLogin, model.AutostartBoot} {
		for _, policy := range []model.RestartPolicy{model.RestartNever, model.RestartOnFailure} {
			t.Run(string(scope)+"/"+string(policy), func(t *testing.T) {
				cfg := launchdTestConfig()
				cfg.Autostart.Scope = scope
				cfg.RestartPolicy = policy
				got, err := Render(cfg, "/Applications/QEMU & Tools/qemu-manage", "/tmp/vm&dir", "/tmp/out<log", "/tmp/err>log", "alice&admin", "/Users/alice&co")
				if err != nil {
					t.Fatal(err)
				}
				again, err := Render(cfg, "/Applications/QEMU & Tools/qemu-manage", "/tmp/vm&dir", "/tmp/out<log", "/tmp/err>log", "alice&admin", "/Users/alice&co")
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, again) {
					t.Fatal("render is not deterministic")
				}
				s := string(got)
				for _, want := range []string{
					"<string>io.qemu-manage.vm.0123456789ab</string>", "<string>/Applications/QEMU &amp; Tools/qemu-manage</string>",
					"<string>vm</string>", "<string>/tmp/out&lt;log</string>", "<string>/tmp/err&gt;log</string>",
					"<key>RunAtLoad</key>\n  <true/>", "<key>ThrottleInterval</key>\n  <integer>30</integer>",
					"<key>ExitTimeOut</key>\n  <integer>195</integer>", "<key>Umask</key>\n  <integer>63</integer>",
					"<key>PATH</key>\n    <string>/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
				} {
					if !strings.Contains(s, want) {
						t.Errorf("missing %q", want)
					}
				}
				if strings.Contains(s, "Crashed") || strings.Contains(s, "<key>KeepAlive</key>\n  <true/>") {
					t.Fatal("unsafe KeepAlive policy rendered")
				}
				wantKeepAlive := policy == model.RestartOnFailure
				if strings.Contains(s, "<key>KeepAlive</key>") != wantKeepAlive {
					t.Errorf("KeepAlive presence mismatch")
				}
				if wantKeepAlive && !strings.Contains(s, "<key>SuccessfulExit</key>\n    <false/>") {
					t.Fatal("on-failure policy lacks SuccessfulExit=false")
				}
				wantUser := scope == model.AutostartBoot
				if strings.Contains(s, "<key>UserName</key>") != wantUser {
					t.Errorf("UserName presence mismatch")
				}
			})
		}
	}
}

func TestRenderRejectsUnsafeScopeAndBootUser(t *testing.T) {
	cfg := launchdTestConfig()
	for _, tc := range []struct {
		scope model.AutostartScope
		user  string
	}{{model.AutostartNone, "alice"}, {model.AutostartBoot, ""}, {model.AutostartBoot, "root"}} {
		cfg.Autostart.Scope = tc.scope
		if _, err := Render(cfg, "/bin/qemu-manage", "/tmp/vm", "/tmp/out", "/tmp/err", tc.user, "/Users/alice"); err == nil {
			t.Fatalf("scope %q user %q accepted", tc.scope, tc.user)
		}
	}
}

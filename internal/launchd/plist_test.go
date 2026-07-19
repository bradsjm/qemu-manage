package launchd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
)

func TestRenderPlistScopesPoliciesAndEscaping(t *testing.T) {
	for _, scope := range []model.AutostartScope{model.AutostartLogin, model.AutostartBoot} {
		for _, policy := range []model.RestartPolicy{model.RestartNever, model.RestartOnFailure} {
			t.Run(string(scope)+"/"+string(policy), func(t *testing.T) {
				cfg := launchdTestConfig()
				cfg.Autostart.Scope = scope
				cfg.RestartPolicy = policy
				dataRoot := "/tmp/data&root"
				runtimeRoot := "/tmp/runtime<root"
				logRoot := "/tmp/log>root"
				got, err := Render(cfg, "/Applications/QEMU & Tools/qemu-manage", "/tmp/vm&dir", "/tmp/out<log", "/tmp/err>log", "alice&admin", "/Users/alice&co", dataRoot, runtimeRoot, logRoot)
				if err != nil {
					t.Fatal(err)
				}
				again, err := Render(cfg, "/Applications/QEMU & Tools/qemu-manage", "/tmp/vm&dir", "/tmp/out<log", "/tmp/err>log", "alice&admin", "/Users/alice&co", dataRoot, runtimeRoot, logRoot)
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
					"<key>QEMU_MANAGE_DATA_ROOT</key>\n    <string>/tmp/data&amp;root</string>",
					"<key>QEMU_MANAGE_RUNTIME_ROOT</key>\n    <string>/tmp/runtime&lt;root</string>",
					"<key>QEMU_MANAGE_LOG_ROOT</key>\n    <string>/tmp/log&gt;root</string>",
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
		if _, err := Render(cfg, "/bin/qemu-manage", "/tmp/vm", "/tmp/out", "/tmp/err", tc.user, "/Users/alice", "/tmp/data", "/tmp/runtime", "/tmp/log"); err == nil {
			t.Fatalf("scope %q user %q accepted", tc.scope, tc.user)
		}
	}
}

func TestRenderRejectsRelativeRoots(t *testing.T) {
	cfg := launchdTestConfig()
	cfg.Autostart.Scope = model.AutostartLogin
	for _, tc := range []struct {
		name        string
		dataRoot    string
		runtimeRoot string
		logRoot     string
	}{
		{name: "data root", dataRoot: "data", runtimeRoot: "/tmp/runtime", logRoot: "/tmp/log"},
		{name: "runtime root", dataRoot: "/tmp/data", runtimeRoot: "runtime", logRoot: "/tmp/log"},
		{name: "log root", dataRoot: "/tmp/data", runtimeRoot: "/tmp/runtime", logRoot: "log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(cfg, "/bin/qemu-manage", "/tmp/vm", "/tmp/out", "/tmp/err", "alice", "/Users/alice", tc.dataRoot, tc.runtimeRoot, tc.logRoot)
			if err == nil || !strings.Contains(err.Error(), tc.name+" path must be absolute") {
				t.Fatalf("expected relative %s rejection, got %v", tc.name, err)
			}
		})
	}
}

func TestRenderWatchPathsOnlyForSocketVMNet(t *testing.T) {
	socketCfg := launchdTestConfig()
	socketCfg.Autostart.Scope = model.AutostartBoot
	socketCfg.Network.Mode = model.NetworkSocketVMNet
	socketCfg.Network.Forwards = nil
	socketCfg.Network.SocketVMNet = &model.SocketVMNetConfig{
		ClientPath: "/opt/socket_vmnet/bin/socket_vmnet_client",
		SocketPath: "/var/run/socket_vmnet.bridged.vlan0",
		Interface:  "vlan0",
	}
	got, err := Render(socketCfg, "/bin/qemu-manage", "/tmp/vm", "/tmp/out", "/tmp/err", "alice", "/Users/alice", "/tmp/data", "/tmp/runtime", "/tmp/log")
	if err != nil {
		t.Fatal(err)
	}
	watchSnippet := "<key>WatchPaths</key>\n  <array>\n    <string>/var/run/socket_vmnet.bridged.vlan0</string>\n  </array>"
	if !strings.Contains(string(got), watchSnippet) {
		t.Fatalf("socket_vmnet boot job missing WatchPaths:\n%s", got)
	}

	userCfg := launchdTestConfig()
	userCfg.Autostart.Scope = model.AutostartBoot
	withoutWatchPaths, err := Render(userCfg, "/bin/qemu-manage", "/tmp/vm", "/tmp/out", "/tmp/err", "alice", "/Users/alice", "/tmp/data", "/tmp/runtime", "/tmp/log")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutWatchPaths), "<key>WatchPaths</key>") {
		t.Fatalf("user-network boot job unexpectedly watches socket path:\n%s", withoutWatchPaths)
	}
}

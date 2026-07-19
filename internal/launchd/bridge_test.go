package launchd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProvisionSocketVMNetBridgeRejectsInvalidInterfaces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		iface     string
		wantError string
	}{
		{name: "shared", iface: "shared", wantError: "shared interface does not use bridged provisioning"},
		{name: "invalid chars", iface: "bridge/0", wantError: `invalid bridged interface "bridge/0"`},
		{name: "too long", iface: "abcdefghijklmnop", wantError: `invalid bridged interface "abcdefghijklmnop"`},
		{name: "leading punctuation", iface: "-bridge0", wantError: `invalid bridged interface "-bridge0"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, _ := launchdTestManager(t)
			m.WaitForSocketVMNet = func(context.Context, string) error {
				t.Fatal("readiness waiter called for invalid interface")
				return nil
			}
			if _, err := m.ProvisionSocketVMNetBridge(context.Background(), "/ignored", tc.iface); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("interface %q error=%v", tc.iface, err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("invalid interface invoked runner: %#v", r.calls)
			}
		})
	}
}

func TestRenderSocketVMNetBridgePlistDeterministic(t *testing.T) {
	m, _, _ := launchdTestManager(t)
	m.SocketVMNetInstallRoot = "/opt/socket_vmnet & bridge"
	socketPath := "/var/run/socket_vmnet.bridged.vlan0"
	got, err := m.renderSocketVMNetBridgePlist("vlan0", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.renderSocketVMNetBridgePlist("vlan0", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("bridge plist render is not deterministic")
	}
	s := string(got)
	for _, want := range []string{
		"<string>io.github.bradsjm.qemu-manage.socket_vmnet.bridged.vlan0</string>",
		"<key>Program</key>\n\t\t<string>/opt/socket_vmnet &amp; bridge/bin/socket_vmnet</string>",
		"<key>ProgramArguments</key>",
		"<string>--socket-group=staff</string>",
		"<string>--vmnet-mode=bridged</string>",
		"<string>--vmnet-interface=vlan0</string>",
		"<string>/var/run/socket_vmnet.bridged.vlan0</string>",
		"<key>RunAtLoad</key>\n\t\t<true />",
		"<key>KeepAlive</key>\n\t\t<true />",
		"<key>UserName</key>\n\t\t<string>root</string>",
		"<key>ProcessType</key>\n\t\t<string>Interactive</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestProvisionSocketVMNetBridgeRejectsUserOwnedExistingSystemPlistBeforeBootstrap(t *testing.T) {
	m, r, _ := launchdTestManager(t)
	root := t.TempDir()
	m.SystemDir = filepath.Join(root, "LaunchDaemons")
	m.SocketVMNetInstallRoot = filepath.Join(root, "socket_vmnet")
	m.SocketVMNetRunRoot = filepath.Join(root, "run")
	m.WaitForSocketVMNet = func(context.Context, string) error {
		t.Fatal("readiness waiter called before existing plist ownership rejection")
		return nil
	}
	clientPath, _ := createSocketVMNetSourcePair(t)
	interfaceName := "vlan0"
	plistPath := m.socketVMNetBridgePlistPath(interfaceName)
	want, err := m.renderSocketVMNetBridgePlist(interfaceName, m.socketVMNetBridgeSocketPath(interfaceName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, want, 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ProvisionSocketVMNetBridge(context.Background(), clientPath, interfaceName); err == nil || !strings.Contains(err.Error(), "must be root:wheel with mode 0644") {
		t.Fatalf("user-owned plist error=%v", err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("existing plist bytes changed on rejection")
	}
	if len(r.calls) != 0 {
		t.Fatalf("existing plist rejection invoked runner: %#v", r.calls)
	}
}

func TestProvisionSocketVMNetBridgeInstallsPrivilegedArtifactsAndRollsBackUserOwnedPlist(t *testing.T) {
	m, r, _ := launchdTestManager(t)
	root := t.TempDir()
	m.SystemDir = filepath.Join(root, "LaunchDaemons")
	m.SocketVMNetInstallRoot = filepath.Join(root, "socket_vmnet")
	m.SocketVMNetRunRoot = filepath.Join(root, "run")
	waiterCalled := false
	m.WaitForSocketVMNet = func(context.Context, string) error {
		waiterCalled = true
		return nil
	}
	clientPath, daemonPath := createSocketVMNetSourcePair(t)
	interfaceName := "vlan0"
	plistPath := m.socketVMNetBridgePlistPath(interfaceName)
	expectedPlist, err := m.renderSocketVMNetBridgePlist(interfaceName, m.socketVMNetBridgeSocketPath(interfaceName))
	if err != nil {
		t.Fatal(err)
	}
	var candidateChecked bool
	r.hook = func(c runnerCall) ([]byte, error) {
		switch {
		case c.path == "/usr/bin/plutil":
			if c.privileged || len(c.args) != 2 || c.args[0] != "-lint" {
				t.Fatalf("unexpected lint call: %#v", c)
			}
			return nil, nil
		case c.path == "/usr/bin/install" && reflect.DeepEqual(c.args, []string{"-d", "-o", "root", "-g", "wheel", "-m", "0755", m.socketVMNetBinDir()}):
			return nil, os.MkdirAll(m.socketVMNetBinDir(), 0755)
		case c.path == "/usr/bin/install" && reflect.DeepEqual(c.args, []string{"-o", "root", "-g", "wheel", "-m", "0755", daemonPath, m.socketVMNetDaemonPath()}):
			return nil, nil
		case c.path == "/usr/bin/install" && reflect.DeepEqual(c.args, []string{"-o", "root", "-g", "wheel", "-m", "0755", clientPath, m.socketVMNetClientPath()}):
			return nil, nil
		case c.path == "/usr/bin/install" && len(c.args) == 8 && c.args[7] == plistPath:
			if !c.privileged || !reflect.DeepEqual(c.args[:6], []string{"-o", "root", "-g", "wheel", "-m", "0644"}) {
				t.Fatalf("unsafe plist install argv: %#v", c)
			}
			candidateBytes, err := os.ReadFile(c.args[6])
			if err != nil {
				return nil, err
			}
			candidateChecked = true
			if !bytes.Equal(candidateBytes, expectedPlist) {
				t.Fatalf("bridge plist candidate mismatch\nwant:\n%s\n\ngot:\n%s", expectedPlist, candidateBytes)
			}
			if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(plistPath, append(candidateBytes, []byte("tampered")...), 0644)
		case c.path == "/bin/rm":
			if !c.privileged || !reflect.DeepEqual(c.args, []string{"-f", "--", plistPath}) {
				t.Fatalf("unsafe plist removal argv: %#v", c)
			}
			return nil, os.Remove(c.args[2])
		case c.path == launchctlPath:
			t.Fatalf("launchctl reached before plist verification: %#v", c)
		}
		return nil, nil
	}

	if _, err := m.ProvisionSocketVMNetBridge(context.Background(), clientPath, interfaceName); err == nil || !strings.Contains(err.Error(), "must be root:wheel with mode 0644") {
		t.Fatalf("installed user-owned plist error=%v", err)
	}
	if waiterCalled {
		t.Fatal("readiness waiter called after user-owned plist install")
	}
	if !candidateChecked {
		t.Fatal("plist install candidate was not validated")
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback did not remove rejected plist: %v", err)
	}

	wantCalls := []runnerCall{
		{privileged: false, path: "/usr/bin/plutil"},
		{privileged: true, path: "/usr/bin/install", args: []string{"-d", "-o", "root", "-g", "wheel", "-m", "0755", m.socketVMNetBinDir()}},
		{privileged: true, path: "/usr/bin/install", args: []string{"-o", "root", "-g", "wheel", "-m", "0755", daemonPath, m.socketVMNetDaemonPath()}},
		{privileged: true, path: "/usr/bin/install", args: []string{"-o", "root", "-g", "wheel", "-m", "0755", clientPath, m.socketVMNetClientPath()}},
		{privileged: true, path: "/bin/rm", args: []string{"-f", "--", plistPath}},
	}
	for _, wantCall := range wantCalls {
		found := false
		for _, got := range r.calls {
			if got.privileged == wantCall.privileged && got.path == wantCall.path {
				if wantCall.args == nil || reflect.DeepEqual(got.args, wantCall.args) {
					found = true
					break
				}
			}
		}
		if !found {
			t.Fatalf("missing runner call %#v in %#v", wantCall, r.calls)
		}
	}
}

func createSocketVMNetSourcePair(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	daemonPath := filepath.Join(dir, "socket_vmnet")
	clientPath := filepath.Join(dir, "socket_vmnet_client")
	for _, path := range []string{daemonPath, clientPath} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return clientPath, daemonPath
}

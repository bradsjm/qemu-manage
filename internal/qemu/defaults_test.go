package qemu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverSocketVMNetPairsSafestClientWithInstalledDaemon(t *testing.T) {
	root := t.TempDir()
	rootClient := filepath.Join(root, "opt", "socket_vmnet_client")
	homebrewClient := filepath.Join(root, "homebrew", "socket_vmnet_client")
	homebrewDaemon := filepath.Join(root, "homebrew", "socket_vmnet")
	for _, path := range []string{rootClient, homebrewClient, homebrewDaemon} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	got := discoverSocketVMNet([]socketVMNetInstallation{
		{clientPath: rootClient, daemonPath: filepath.Join(root, "opt", "missing-daemon"), socketPath: "/var/run/socket_vmnet"},
		{clientPath: homebrewClient, daemonPath: homebrewDaemon, socketPath: "/opt/homebrew/var/run/socket_vmnet"},
	})
	if got == nil {
		t.Fatal("DiscoverSocketVMNet returned nil")
	}
	if got.ClientPath != rootClient || got.SocketPath != "/opt/homebrew/var/run/socket_vmnet" || got.Interface != "shared" {
		t.Fatalf("unexpected discovered configuration: %+v", got)
	}
}

func TestDiscoverSocketVMNetRequiresInstalledClient(t *testing.T) {
	root := t.TempDir()
	daemon := filepath.Join(root, "socket_vmnet")
	if err := os.WriteFile(daemon, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := discoverSocketVMNet([]socketVMNetInstallation{{
		clientPath: filepath.Join(root, "missing-client"),
		daemonPath: daemon,
		socketPath: "/var/run/socket_vmnet",
	}})
	if got == nil || got.ClientPath != "" || got.SocketPath != "/var/run/socket_vmnet" || got.Interface != "shared" {
		t.Fatalf("unexpected daemon-only discovery: %+v", got)
	}
}

func TestDiscoverSocketVMNetDoesNotGuessSocketFromClient(t *testing.T) {
	client := filepath.Join(t.TempDir(), "socket_vmnet_client")
	if err := os.WriteFile(client, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := discoverSocketVMNet([]socketVMNetInstallation{{
		clientPath: client,
		daemonPath: filepath.Join(t.TempDir(), "missing-daemon"),
		socketPath: filepath.Join(t.TempDir(), "missing.sock"),
	}})
	if got == nil || got.ClientPath != client || got.SocketPath != "" || got.Interface != "shared" {
		t.Fatalf("unexpected client-only discovery: %+v", got)
	}
}

func TestDiscoverFirmwareRequiresReadablePairFromOneInstallation(t *testing.T) {
	root := t.TempDir()
	firstCode := filepath.Join(root, "first-code.fd")
	secondVars := filepath.Join(root, "second-vars.fd")
	for _, path := range []string{firstCode, secondVars} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	installations := []firmwareInstallation{
		{codePath: firstCode, variablesPath: []string{filepath.Join(root, "missing-first-vars.fd")}},
		{codePath: filepath.Join(root, "missing-second-code.fd"), variablesPath: []string{secondVars}},
	}
	if code, variables := discoverFirmware(installations); code != "" || variables != "" {
		t.Fatalf("discovered cross-installation firmware pair: code=%q variables=%q", code, variables)
	}
	codeCheck, variablesCheck := discoveredFirmwareChecks(installations)
	if codeCheck.Status != CheckFail || variablesCheck.Status != CheckFail {
		t.Fatalf("doctor accepted cross-installation firmware: code=%+v variables=%+v", codeCheck, variablesCheck)
	}

	secondCode := filepath.Join(root, "second-code.fd")
	if err := os.WriteFile(secondCode, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	installations[1].codePath = secondCode
	code, variables := discoverFirmware(installations)
	if code != secondCode || variables != secondVars {
		t.Fatalf("discovered firmware pair = %q, %q; want %q, %q", code, variables, secondCode, secondVars)
	}
	codeCheck, variablesCheck = discoveredFirmwareChecks(installations)
	if codeCheck.Status != CheckPass || variablesCheck.Status != CheckPass {
		t.Fatalf("doctor rejected coherent firmware: code=%+v variables=%+v", codeCheck, variablesCheck)
	}
}

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qemu-manage/internal/model"
)

func TestSetForwardsGuestAgentAndNetworkTransitions(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	code, _, stderr := runCLI(a, "set", "vm", "--guest-agent", "on", "--forward", "tcp:127.0.0.1:2222:22", "--forward", "udp:127.0.0.1:5353:53")
	if code != 0 {
		t.Fatalf("set failed: %s", stderr)
	}
	got, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if !got.GuestAgent.Enabled || len(got.Network.Forwards) != 2 {
		t.Fatalf("unexpected set result: %+v", got)
	}
	code, _, stderr = runCLI(a, "set", "vm", "--clear-forwards", "--forward", "tcp:127.0.0.1:8443:443")
	if code != 0 {
		t.Fatalf("replace failed: %s", stderr)
	}
	got, _ = a.Store.Load("vm")
	if len(got.Network.Forwards) != 1 || got.Network.Forwards[0].HostPort != 8443 {
		t.Fatalf("forwards were not replaced: %+v", got.Network.Forwards)
	}
	code, _, stderr = runCLI(a, "set", "vm", "--network", "socket_vmnet", "--socket-vmnet-client", "/opt/socket_vmnet/client", "--socket-vmnet-socket", "/var/run/socket_vmnet", "--socket-vmnet-interface", "vlan0")
	if code != 0 {
		t.Fatalf("bridge transition failed: %s", stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.Network.Mode != model.NetworkSocketVMNet || len(got.Network.Forwards) != 0 || got.Network.SocketVMNet.Interface != "vlan0" {
		t.Fatalf("unexpected bridge config: %+v", got.Network)
	}
	code, _, stderr = runCLI(a, "set", "vm", "--network", "user", "--guest-agent", "off", "--forward", "tcp:127.0.0.1:2022:22")
	if code != 0 {
		t.Fatalf("user transition failed: %s", stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.GuestAgent.Enabled || got.Network.SocketVMNet != nil || len(got.Network.Forwards) != 1 {
		t.Fatalf("unexpected user config: %+v", got)
	}
}

func TestConfigShowValidateAndStrictApply(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	code, shown, stderr := runCLI(a, "config", "show", "vm")
	if code != 0 {
		t.Fatalf("show failed: %s", stderr)
	}
	var decoded model.Config
	if err := json.Unmarshal([]byte(shown), &decoded); err != nil {
		t.Fatalf("show was not JSON: %v", err)
	}
	if decoded.ID != cfg.ID || !strings.HasSuffix(shown, "\n") {
		t.Fatalf("unexpected canonical output %q", shown)
	}

	replacement := *cfg
	replacement.CPUs = 6
	replacement.GuestAgent.Enabled = true
	data, err := model.CanonicalJSON(&replacement)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "replacement.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runCLI(a, "config", "validate", file); code != 0 {
		t.Fatalf("validate failed: %s", stderr)
	}
	if code, _, stderr = runCLI(a, "config", "apply", "vm", file); code != 0 {
		t.Fatalf("apply failed: %s", stderr)
	}
	got, _ := a.Store.Load("vm")
	if got.CPUs != 6 || !got.GuestAgent.Enabled {
		t.Fatalf("apply not persisted: %+v", got)
	}

	bad := strings.TrimSuffix(string(data), "\n")
	bad = strings.TrimSuffix(bad, "}") + `,"unknown":true}`
	badFile := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badFile, []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runCLI(a, "config", "validate", badFile); code != 1 || !strings.Contains(stderr, "unknown field") {
		t.Fatalf("strict validation code=%d stderr=%q", code, stderr)
	}

	other := replacement
	other.ID = "fedcba9876543210fedcba9876543210"
	otherData, _ := model.CanonicalJSON(&other)
	otherFile := filepath.Join(t.TempDir(), "other.json")
	_ = os.WriteFile(otherFile, otherData, 0600)
	if code, _, stderr = runCLI(a, "config", "apply", "vm", otherFile); code != 1 || !strings.Contains(stderr, "id") {
		t.Fatalf("immutable apply code=%d stderr=%q", code, stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.ID != cfg.ID || got.CPUs != 6 {
		t.Fatalf("failed apply mutated config: %+v", got)
	}
}

func TestSetRejectsInvalidForwardWithoutMutation(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	saveTestConfig(t, a, cfg)
	code, _, stderr := runCLI(a, "set", "vm", "--forward", "tcp:0.0.0.0:0:22")
	if code != 2 || !strings.Contains(stderr, "host port") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	got, _ := a.Store.Load("vm")
	if len(got.Network.Forwards) != 0 {
		t.Fatalf("invalid set mutated config: %+v", got.Network.Forwards)
	}
}

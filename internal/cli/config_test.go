package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
)

func TestSetForwardsGuestAgentAndNetworkTransitions(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	a.DiscoverSocketVMNet = func() *model.SocketVMNetConfig {
		return &model.SocketVMNetConfig{
			ClientPath: "/opt/socket_vmnet/client",
			SocketPath: "/var/run/socket_vmnet",
			Interface:  "shared",
		}
	}
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
	code, _, stderr = runCLI(a, "set", "vm", "--network", "socket_vmnet", "--socket-vmnet-interface", "vlan0")
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
func TestSetMetricsPortTransitions(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("vm"))

	for _, port := range []string{"1024", "65535", "8124"} {
		code, _, stderr := runCLI(a, "set", "vm", "--metrics-port", port)
		if code != 0 {
			t.Fatalf("set port %s failed: %s", port, stderr)
		}
		config, err := a.Store.Load("vm")
		if err != nil {
			t.Fatal(err)
		}
		want, _ := strconv.ParseUint(port, 10, 16)
		if config.Metrics == nil || config.Metrics.Port != uint16(want) {
			t.Fatalf("set port %s persisted %#v", port, config.Metrics)
		}
	}

	code, _, stderr := runCLI(a, "set", "vm", "--metrics-port", "off")
	if code != 0 {
		t.Fatalf("disable metrics failed: %s", stderr)
	}
	config, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if config.Metrics != nil {
		t.Fatalf("metrics remained enabled: %#v", config.Metrics)
	}

	for _, value := range []string{"0", "1023", "65536", "invalid", "on"} {
		code, _, stderr = runCLI(a, "set", "vm", "--metrics-port", value)
		if code != 2 || !strings.Contains(stderr, "1024 and 65535, or off") {
			t.Fatalf("value %q: code=%d stderr=%q", value, code, stderr)
		}
	}
}

func TestSetVNCTransitions(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("vm"))

	code, _, stderr := runCLI(a, "set", "vm", "--vnc", "on")
	if code != 2 || !strings.Contains(stderr, "--vnc-password is required") {
		t.Fatalf("enable without password code=%d stderr=%q", code, stderr)
	}
	got, _ := a.Store.Load("vm")
	if got.VNC != nil {
		t.Fatalf("failed enable mutated VNC config: %+v", got.VNC)
	}

	code, _, stderr = runCLI(a, "set", "vm", "--vnc-password", "secret")
	if code != 2 || !strings.Contains(stderr, "existing VNC or --vnc on") {
		t.Fatalf("detail without enable code=%d stderr=%q", code, stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.VNC != nil {
		t.Fatalf("detail-only update mutated disabled VNC config: %+v", got.VNC)
	}

	code, _, stderr = runCLI(a, "set", "vm", "--vnc", "on", "--vnc-password", "secret")
	if code != 0 {
		t.Fatalf("enable VNC failed: %s", stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.VNC == nil {
		t.Fatal("VNC config was not enabled")
	}
	if *got.VNC != (model.VNCConfig{
		Bind:           defaultVNCBind,
		Port:           defaultVNCPort,
		PortTo:         defaultVNCPortTo,
		Password:       "secret",
		KeyboardLayout: defaultKeyboardLayout,
	}) {
		t.Fatalf("unexpected enabled VNC config: %+v", *got.VNC)
	}

	code, _, stderr = runCLI(a, "set", "vm", "--vnc-bind", "0.0.0.0", "--vnc-port", "5905")
	if code != 0 {
		t.Fatalf("update VNC details failed: %s", stderr)
	}
	got, _ = a.Store.Load("vm")
	if *got.VNC != (model.VNCConfig{
		Bind:           "0.0.0.0",
		Port:           5905,
		PortTo:         defaultVNCPortTo,
		Password:       "secret",
		KeyboardLayout: defaultKeyboardLayout,
	}) {
		t.Fatalf("VNC update did not preserve omitted fields: %+v", *got.VNC)
	}

	code, _, stderr = runCLI(a, "set", "vm", "--vnc", "off", "--vnc-port-to", "5906")
	if code != 2 || !strings.Contains(stderr, "--vnc off is incompatible") {
		t.Fatalf("disable with details code=%d stderr=%q", code, stderr)
	}
	got, _ = a.Store.Load("vm")
	if *got.VNC != (model.VNCConfig{
		Bind:           "0.0.0.0",
		Port:           5905,
		PortTo:         defaultVNCPortTo,
		Password:       "secret",
		KeyboardLayout: defaultKeyboardLayout,
	}) {
		t.Fatalf("failed disable mutated enabled VNC config: %+v", *got.VNC)
	}

	code, _, stderr = runCLI(a, "set", "vm", "--vnc", "off")
	if code != 0 {
		t.Fatalf("disable VNC failed: %s", stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.VNC != nil {
		t.Fatalf("VNC config was not cleared: %+v", got.VNC)
	}

	code, _, stderr = runCLI(a, "set", "vm", "--vnc-port-to", "5906")
	if code != 2 || !strings.Contains(stderr, "existing VNC or --vnc on") {
		t.Fatalf("detail after disable code=%d stderr=%q", code, stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.VNC != nil {
		t.Fatalf("failed detail-only update mutated disabled VNC config: %+v", got.VNC)
	}
}

func TestConfigShowValidateAndStrictApply(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.Autostart.Scope = model.AutostartBoot
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
	replacement.RestartPolicy = model.RestartOnFailure
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
	assertStaleAutostartWarning(t, stderr, "vm", model.AutostartBoot)
	got, _ := a.Store.Load("vm")
	if got.CPUs != 6 || !got.GuestAgent.Enabled || got.RestartPolicy != model.RestartOnFailure {
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
	malformed := strings.TrimSuffix(string(data), "\n")
	malformed = strings.TrimSuffix(malformed, "}") + `,"vnc":{"bind":"127.0.0.1","port":5900,"port_to":5999,"password":"\udc00"}}`
	malformedFile := filepath.Join(t.TempDir(), "malformed-vnc.json")
	if err := os.WriteFile(malformedFile, []byte(malformed), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runCLI(a, "config", "validate", malformedFile); code != 1 || stderr == "" {
		t.Fatalf("malformed validate code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr = runCLI(a, "config", "apply", "vm", malformedFile); code != 1 || stderr == "" {
		t.Fatalf("malformed apply code=%d stderr=%q", code, stderr)
	}
	got, _ = a.Store.Load("vm")
	if got.ID != cfg.ID || got.CPUs != 6 || got.VNC != nil {
		t.Fatalf("malformed apply mutated config: %+v", got)
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

func TestSetWarnsWhenAutostartLaunchdConfigBecomesStale(t *testing.T) {
	a := testApp(t)
	cfg := testConfig("vm")
	cfg.Autostart.Scope = model.AutostartLogin
	saveTestConfig(t, a, cfg)

	code, stdout, stderr := runCLI(a, "set", "vm", "--restart-policy", "on-failure")
	if code != 0 {
		t.Fatalf("set failed: %s", stderr)
	}
	if stdout != "" {
		t.Fatalf("set wrote unexpected stdout: %q", stdout)
	}
	assertStaleAutostartWarning(t, stderr, "vm", model.AutostartLogin)

	got, _ := a.Store.Load("vm")
	if got.RestartPolicy != model.RestartOnFailure {
		t.Fatalf("set did not persist restart policy: %+v", got)
	}
}

func TestWarnStaleAutostartIgnoresWriteErrors(t *testing.T) {
	writer := &attemptingErrorWriter{}
	warnStaleAutostart(writer, false, "vm", model.AutostartBoot)
	if writer.attempts != 1 {
		t.Fatalf("expected one write attempt, got %d", writer.attempts)
	}
}

func assertStaleAutostartWarning(t *testing.T, stderr, vmName string, scope model.AutostartScope) {
	t.Helper()
	if stderr == "" {
		t.Fatal("expected stale launchd warning")
	}
	if strings.Contains(stderr, "\x1b") {
		t.Fatalf("redirected warning contained ANSI escape: %q", stderr)
	}
	for _, fragment := range []string{
		"launchd configuration",
		vmName,
		"qemu-manage stop " + vmName,
		"qemu-manage autostart disable " + vmName,
		"qemu-manage autostart enable " + vmName + " --scope " + string(scope),
	} {
		if !strings.Contains(stderr, fragment) {
			t.Fatalf("warning %q missing fragment %q", stderr, fragment)
		}
	}
}

type attemptingErrorWriter struct {
	attempts int
}

func (w *attemptingErrorWriter) Write(p []byte) (int, error) {
	w.attempts++
	return 0, io.ErrClosedPipe
}

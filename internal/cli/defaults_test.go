package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/model"
)

func createInputs(t *testing.T) (code, variables, qemu, qemuImg string) {
	t.Helper()
	root := t.TempDir()
	code = filepath.Join(root, "edk2-code.fd")
	variables = filepath.Join(root, "edk2-vars.fd")
	qemu = filepath.Join(root, "qemu-system-aarch64")
	qemuImg = filepath.Join(root, "qemu-img")
	for path, contents := range map[string]string{
		code:      "discovered firmware code",
		variables: "discovered firmware variables",
		qemu:      "qemu",
		qemuImg:   "qemu-img",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return code, variables, qemu, qemuImg
}

func fakeDiskCreation(t *testing.T, a *App) {
	t.Helper()
	a.RunExternal = func(_ context.Context, _ string, args []string) error {
		if len(args) < 4 {
			t.Fatalf("unexpected qemu-img arguments: %v", args)
		}
		return os.WriteFile(args[3], []byte("disk"), 0o600)
	}
}

func TestCreateUsesDiscoveredFirmwareWhenFlagsAreOmitted(t *testing.T) {
	a := testApp(t)
	codePath, variablesPath, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (code, variables string) {
		return codePath, variablesPath
	}
	fakeDiskCreation(t, a)

	exit, _, stderr := runCLI(a, "create", "vm", "--qemu", qemuPath, "--qemu-img", qemuImgPath, "--disk-size", "1GiB")
	if exit != 0 {
		t.Fatalf("create exited %d: %s", exit, stderr)
	}
	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Firmware != (model.FirmwareConfig{Code: "firmware-code.fd", Variables: "firmware-vars.fd"}) {
		t.Fatalf("unexpected persisted firmware config: %+v", cfg.Firmware)
	}
	hardware, err := net.ParseMAC(cfg.Network.MAC)
	if err != nil || len(hardware) != 6 || cfg.Network.MAC != strings.ToLower(cfg.Network.MAC) || len(cfg.Network.MAC) != 17 || hardware[0]&0x03 != 0x02 {
		t.Fatalf("generated MAC %q is not a canonical locally administered unicast MAC", cfg.Network.MAC)
	}
	vmDir := filepath.Join(a.Store.DataRoot, "vm")
	configData, err := os.ReadFile(filepath.Join(vmDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "{\n  \"$schema\": \"" + model.ConfigSchemaURL + "\",\n"
	if !strings.HasPrefix(string(configData), prefix) {
		t.Fatalf("config.json lacks schema annotation prefix: %s", configData)
	}
	for _, check := range []struct {
		name     string
		want     string
		wantPerm os.FileMode
	}{
		{"firmware-code.fd", "discovered firmware code", 0o400},
		{"firmware-vars.fd", "discovered firmware variables", 0o600},
	} {
		path := filepath.Join(vmDir, check.name)
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read copied %s: %v", check.name, err)
		}
		if string(contents) != check.want {
			t.Errorf("copied %s contents = %q, want %q", check.name, contents, check.want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != check.wantPerm {
			t.Errorf("copied %s mode = %04o, want %04o", check.name, info.Mode().Perm(), check.wantPerm)
		}
	}
}

func TestCreatePersistsExplicitMAC(t *testing.T) {
	a := testApp(t)
	codePath, variablesPath, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (code, variables string) {
		return codePath, variablesPath
	}
	fakeDiskCreation(t, a)

	const wantMAC = "02:12:34:56:78:9a"
	exit, _, stderr := runCLI(a, "create", "vm", "--qemu", qemuPath, "--qemu-img", qemuImgPath, "--disk-size", "1GiB", "--mac", wantMAC)
	if exit != 0 {
		t.Fatalf("create exited %d: %s", exit, stderr)
	}
	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.MAC != wantMAC {
		t.Fatalf("persisted MAC = %q, want %q", cfg.Network.MAC, wantMAC)
	}
}

func TestCreateRejectsInvalidExplicitMAC(t *testing.T) {
	for _, tc := range []struct {
		name string
		mac  string
	}{
		{name: "uppercase", mac: "02:12:34:56:78:9A"},
		{name: "compact", mac: "02123456789a"},
		{name: "multicast", mac: "03:12:34:56:78:9a"},
		{name: "globally administered", mac: "00:12:34:56:78:9a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			codePath, variablesPath, qemuPath, qemuImgPath := createInputs(t)
			a.DiscoverFirmware = func() (code, variables string) {
				return codePath, variablesPath
			}
			fakeDiskCreation(t, a)

			exit, _, stderr := runCLI(a, "create", "vm", "--qemu", qemuPath, "--qemu-img", qemuImgPath, "--disk-size", "1GiB", "--mac", tc.mac)
			if exit == 0 || !strings.Contains(stderr, "network mac must") {
				t.Fatalf("create exited %d, stderr=%q; want MAC validation failure", exit, stderr)
			}
			if _, err := os.Stat(filepath.Join(a.Store.DataRoot, "vm")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid MAC left VM directory: %v", err)
			}
		})
	}
}

func TestCreateExplicitFirmwareOverridesDiscovery(t *testing.T) {
	a := testApp(t)
	discoveredCode, discoveredVariables, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (code, variables string) {
		return discoveredCode, discoveredVariables
	}
	explicitRoot := t.TempDir()
	explicitCode := filepath.Join(explicitRoot, "code.fd")
	explicitVariables := filepath.Join(explicitRoot, "vars.fd")
	if err := os.WriteFile(explicitCode, []byte("explicit code"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicitVariables, []byte("explicit variables"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeDiskCreation(t, a)

	exit, _, stderr := runCLI(a, "create", "vm", "--qemu", qemuPath, "--qemu-img", qemuImgPath, "--firmware-code", explicitCode, "--firmware-vars", explicitVariables, "--disk-size", "1GiB")
	if exit != 0 {
		t.Fatalf("create exited %d: %s", exit, stderr)
	}
	vmDir := filepath.Join(a.Store.DataRoot, "vm")
	for name, want := range map[string]string{
		"firmware-code.fd": "explicit code",
		"firmware-vars.fd": "explicit variables",
	} {
		contents, err := os.ReadFile(filepath.Join(vmDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != want {
			t.Errorf("%s contents = %q, want explicit source %q", name, contents, want)
		}
	}
}

func TestCreateDoesNotMixExplicitAndDiscoveredFirmware(t *testing.T) {
	for _, option := range []string{"--firmware-code", "--firmware-vars"} {
		t.Run(strings.TrimPrefix(option, "--"), func(t *testing.T) {
			a := testApp(t)
			discoveredCode, discoveredVariables, qemuPath, qemuImgPath := createInputs(t)
			a.DiscoverFirmware = func() (code, variables string) {
				return discoveredCode, discoveredVariables
			}
			exit, _, stderr := runCLI(
				a,
				"create", "vm",
				"--qemu", qemuPath,
				"--qemu-img", qemuImgPath,
				option, map[string]string{
					"--firmware-code": discoveredCode,
					"--firmware-vars": discoveredVariables,
				}[option],
			)
			if exit != 2 || !strings.Contains(stderr, "must be provided together") {
				t.Fatalf("create with only %s exited %d, stderr=%q", option, exit, stderr)
			}
			if _, err := a.Store.Load("vm"); err == nil {
				t.Fatal("invalid partial firmware override created a VM")
			}
		})
	}
}
func TestCreateVNCDefaultsAndFlagConsistency(t *testing.T) {
	newCreateApp := func(t *testing.T) (*App, string, string) {
		t.Helper()
		a := testApp(t)
		codePath, variablesPath, qemuPath, qemuImgPath := createInputs(t)
		a.DiscoverFirmware = func() (code, variables string) {
			return codePath, variablesPath
		}
		fakeDiskCreation(t, a)
		return a, qemuPath, qemuImgPath
	}

	t.Run("enabled persists defaults", func(t *testing.T) {
		a, qemuPath, qemuImgPath := newCreateApp(t)
		exit, _, stderr := runCLI(
			a,
			"create", "vm",
			"--qemu", qemuPath,
			"--qemu-img", qemuImgPath,
			"--disk-size", "1GiB",
			"--vnc",
			"--vnc-password", "secret",
		)
		if exit != 0 {
			t.Fatalf("create exited %d: %s", exit, stderr)
		}
		cfg, err := a.Store.Load("vm")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.VNC == nil {
			t.Fatal("VNC config was not persisted")
		}
		if *cfg.VNC != (model.VNCConfig{
			Bind:           defaultVNCBind,
			Port:           defaultVNCPort,
			PortTo:         defaultVNCPortTo,
			Password:       "secret",
			KeyboardLayout: defaultKeyboardLayout,
		}) {
			t.Fatalf("unexpected persisted VNC config: %+v", *cfg.VNC)
		}
	})

	t.Run("enabled requires password", func(t *testing.T) {
		a, qemuPath, qemuImgPath := newCreateApp(t)
		exit, _, stderr := runCLI(
			a,
			"create", "vm",
			"--qemu", qemuPath,
			"--qemu-img", qemuImgPath,
			"--disk-size", "1GiB",
			"--vnc",
		)
		if exit != 2 || !strings.Contains(stderr, "--vnc-password is required") {
			t.Fatalf("create exited %d, stderr=%q", exit, stderr)
		}
		if _, err := a.Store.Load("vm"); err == nil {
			t.Fatal("missing VNC password still created a VM")
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "password", args: []string{"--vnc-password", "secret"}},
		{name: "bind", args: []string{"--vnc-bind", "0.0.0.0"}},
		{name: "port", args: []string{"--vnc-port", "5901"}},
		{name: "port-to", args: []string{"--vnc-port-to", "5902"}},
	} {
		t.Run("detail without enable/"+tc.name, func(t *testing.T) {
			a, qemuPath, qemuImgPath := newCreateApp(t)
			args := []string{"create", "vm", "--qemu", qemuPath, "--qemu-img", qemuImgPath, "--disk-size", "1GiB"}
			args = append(args, tc.args...)
			exit, _, stderr := runCLI(a, args...)
			if exit != 2 || !strings.Contains(stderr, "require --vnc") {
				t.Fatalf("create exited %d, stderr=%q", exit, stderr)
			}
			if _, err := a.Store.Load("vm"); err == nil {
				t.Fatalf("detail flag %q created a VM without --vnc", tc.name)
			}
		})
	}
}

func TestCreateSocketVMNetProvisioningBehavior(t *testing.T) {
	newCreateApp := func(t *testing.T) (*App, string, string) {
		t.Helper()
		a := testApp(t)
		codePath, variablesPath, qemuPath, qemuImgPath := createInputs(t)
		a.DiscoverFirmware = func() (code, variables string) {
			return codePath, variablesPath
		}
		fakeDiskCreation(t, a)
		return a, qemuPath, qemuImgPath
	}

	t.Run("bridged interface provisions before storage and persists provisioned paths", func(t *testing.T) {
		a, qemuPath, qemuImgPath := newCreateApp(t)
		discovered := &model.SocketVMNetConfig{
			ClientPath: "/opt/homebrew/opt/socket_vmnet/bin/socket_vmnet_client",
			SocketPath: "/opt/homebrew/var/run/socket_vmnet",
			Interface:  defaultSocketVMNetInterface,
		}
		provisioned := &model.SocketVMNetConfig{
			ClientPath: "/opt/socket_vmnet/bin/socket_vmnet_client",
			SocketPath: "/var/run/socket_vmnet.bridged.en0",
			Interface:  "en0",
		}
		a.DiscoverSocketVMNet = func() *model.SocketVMNetConfig { return discovered }
		a.ProvisionSocketVMNetBridge = func(_ context.Context, clientPath, interfaceName string) (*model.SocketVMNetConfig, error) {
			if clientPath != discovered.ClientPath || interfaceName != provisioned.Interface {
				t.Fatalf("provisioner inputs = (%q, %q), want (%q, %q)", clientPath, interfaceName, discovered.ClientPath, provisioned.Interface)
			}
			if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "vm")); !os.IsNotExist(err) {
				t.Fatalf("bridge provisioning ran after VM storage began: %v", err)
			}
			if _, err := a.Store.Load("vm"); err == nil {
				t.Fatal("bridge provisioning ran after VM config storage")
			}
			return provisioned, nil
		}

		exit, _, stderr := runCLI(
			a,
			"create", "vm",
			"--qemu", qemuPath,
			"--qemu-img", qemuImgPath,
			"--disk-size", "1GiB",
			"--network", "socket_vmnet",
			"--socket-vmnet-interface", "en0",
		)
		if exit != 0 {
			t.Fatalf("create exited %d: %s", exit, stderr)
		}
		cfg, err := a.Store.Load("vm")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Network.Mode != model.NetworkSocketVMNet || cfg.Network.SocketVMNet == nil || *cfg.Network.SocketVMNet != *provisioned {
			t.Fatalf("unexpected persisted socket_vmnet config: %+v", cfg.Network)
		}
		if len(cfg.Network.Forwards) != 0 {
			t.Fatalf("socket_vmnet create retained forwards: %+v", cfg.Network.Forwards)
		}
	})

	t.Run("provisioning failure leaves no VM", func(t *testing.T) {
		a, qemuPath, qemuImgPath := newCreateApp(t)
		a.DiscoverSocketVMNet = func() *model.SocketVMNetConfig {
			return &model.SocketVMNetConfig{
				ClientPath: "/opt/homebrew/opt/socket_vmnet/bin/socket_vmnet_client",
				SocketPath: "/opt/homebrew/var/run/socket_vmnet",
				Interface:  defaultSocketVMNetInterface,
			}
		}
		a.ProvisionSocketVMNetBridge = func(_ context.Context, _ string, _ string) (*model.SocketVMNetConfig, error) {
			return nil, errors.New("bridge provisioning failed")
		}

		exit, _, stderr := runCLI(
			a,
			"create", "vm",
			"--qemu", qemuPath,
			"--qemu-img", qemuImgPath,
			"--disk-size", "1GiB",
			"--network", "socket_vmnet",
			"--socket-vmnet-interface", "en0",
		)
		if exit != 1 || !strings.Contains(stderr, "bridge provisioning failed") {
			t.Fatalf("create exited %d, stderr=%q", exit, stderr)
		}
		if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "vm")); !os.IsNotExist(err) {
			t.Fatalf("failed provisioning left VM directory: %v", err)
		}
		if _, err := a.Store.Load("vm"); err == nil {
			t.Fatal("failed provisioning still created a VM")
		}
	})

	t.Run("shared interface bypasses provisioning", func(t *testing.T) {
		a, qemuPath, qemuImgPath := newCreateApp(t)
		shared := &model.SocketVMNetConfig{
			ClientPath: "/opt/socket_vmnet/bin/socket_vmnet_client",
			SocketPath: "/var/run/socket_vmnet",
			Interface:  defaultSocketVMNetInterface,
		}
		a.DiscoverSocketVMNet = func() *model.SocketVMNetConfig { return shared }
		a.ProvisionSocketVMNetBridge = func(context.Context, string, string) (*model.SocketVMNetConfig, error) {
			t.Fatal("shared socket_vmnet should not provision a bridged daemon")
			return nil, nil
		}

		exit, _, stderr := runCLI(
			a,
			"create", "vm",
			"--qemu", qemuPath,
			"--qemu-img", qemuImgPath,
			"--disk-size", "1GiB",
			"--network", "socket_vmnet",
		)
		if exit != 0 {
			t.Fatalf("create exited %d: %s", exit, stderr)
		}
		cfg, err := a.Store.Load("vm")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Network.Mode != model.NetworkSocketVMNet || cfg.Network.SocketVMNet == nil || *cfg.Network.SocketVMNet != *shared {
			t.Fatalf("shared socket_vmnet defaults were not persisted: %+v", cfg.Network)
		}
	})
}

func TestSetSocketVMNetUsesCompleteDiscoveredDefaults(t *testing.T) {
	a := testApp(t)
	saveTestConfig(t, a, testConfig("vm"))
	want := &model.SocketVMNetConfig{
		ClientPath: "/opt/socket_vmnet/bin/socket_vmnet_client",
		SocketPath: "/var/run/socket_vmnet.bridged.vlan0",
		Interface:  "vlan0",
	}
	a.DiscoverSocketVMNet = func() *model.SocketVMNetConfig { return want }

	exit, _, stderr := runCLI(a, "set", "vm", "--network", "socket_vmnet")
	if exit != 0 {
		t.Fatalf("set exited %d: %s", exit, stderr)
	}
	cfg, err := a.Store.Load("vm")
	if err != nil {
		t.Fatal(err)
	}
	want.Interface = "shared"
	if cfg.Network.Mode != model.NetworkSocketVMNet || cfg.Network.SocketVMNet == nil || *cfg.Network.SocketVMNet != *want {
		t.Fatalf("discovered socket_vmnet defaults were not persisted: %+v", cfg.Network)
	}
	if len(cfg.Network.Forwards) != 0 {
		t.Fatalf("socket_vmnet transition retained forwards: %+v", cfg.Network.Forwards)
	}
}

func TestSetRejectsRemovedSocketVMNetPathFlags(t *testing.T) {
	for _, flag := range []string{"--socket-vmnet-client", "--socket-vmnet-socket"} {
		t.Run(flag, func(t *testing.T) {
			a := testApp(t)
			saveTestConfig(t, a, testConfig("vm"))

			exit, _, stderr := runCLI(a, "set", "vm", flag, "/explicit/path")
			if exit != 2 || !strings.Contains(stderr, "flag provided but not defined") || !strings.Contains(stderr, strings.TrimPrefix(flag, "-")) {
				t.Fatalf("removed flag exit=%d stderr=%q", exit, stderr)
			}
			cfg, err := a.Store.Load("vm")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Network.Mode != model.NetworkUser || cfg.Network.SocketVMNet != nil {
				t.Fatalf("rejected flag mutated persisted network: %+v", cfg.Network)
			}
		})
	}
}

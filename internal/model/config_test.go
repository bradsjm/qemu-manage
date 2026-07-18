package model

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
)

func validTestConfig() Config {
	return Config{
		SchemaVersion:          SchemaVersion,
		ID:                     "0123456789abcdef0123456789abcdef",
		Name:                   "ha.test-1",
		Backend:                BackendQEMU,
		Architecture:           "aarch64",
		UUID:                   "123e4567-e89b-42d3-a456-426614174000",
		CPUs:                   2,
		MemoryMiB:              2048,
		RestartPolicy:          RestartNever,
		ShutdownTimeoutSeconds: 180,
		Firmware:               FirmwareConfig{Code: "firmware/code,uefi.fd", Variables: "firmware/vars.fd"},
		Installer:              &InstallerConfig{Path: "install/media,arm64.iso", BootIndex: 0},
		Disks: []DiskConfig{
			{Path: "disks/system,primary.qcow2", Format: "qcow2", Serial: "system-1", BootIndex: 1},
			{Path: "../shared/data.raw", Format: "raw", Serial: "data_2", BootIndex: 2, ReadOnly: true},
		},
		Network: NetworkConfig{
			Mode: NetworkUser,
			MAC:  "02:12:34:56:78:9a",
			Forwards: []PortForward{
				{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 8123, GuestPort: 8123},
				{Protocol: "udp", HostAddress: "192.0.2.1", HostPort: 5353, GuestPort: 53},
			},
		},
		GuestAgent: GuestAgentConfig{Enabled: true},
		QEMU:       QEMUConfig{Binary: "/opt/homebrew/bin/qemu-system-aarch64", ImageTool: "/opt/homebrew/bin/qemu-img", Machine: "virt", ExtraArgs: []string{"-d", "guest_errors", "-trace=help"}},
		Autostart:  AutostartConfig{Scope: AutostartNone},
	}
}

func cloneTestConfig(t *testing.T, source Config) Config {
	t.Helper()
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone Config
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func requireInvalid(t *testing.T, config Config) {
	t.Helper()
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() succeeded for invalid config")
	}
}

func TestCanonicalRoundTripAndHashStability(t *testing.T) {
	config := validTestConfig()
	first, err := CanonicalJSON(&config)
	if err != nil {
		t.Fatal(err)
	}
	if first[len(first)-1] != '\n' {
		t.Fatal("canonical JSON lacks final newline")
	}
	decoded, err := Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*decoded, config) {
		t.Fatalf("round trip changed config\n got: %#v\nwant: %#v", *decoded, config)
	}
	second, err := CanonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical encoding changed after round trip")
	}
	h1, err := Hash(&config)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Hash(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("unstable hash: %q != %q", h1, h2)
	}
}

func TestGeneratedIdentityShapesAndBits(t *testing.T) {
	for range 64 {
		id, err := GenerateID()
		if err != nil {
			t.Fatal(err)
		}
		idBytes, err := hex.DecodeString(id)
		if err != nil || len(idBytes) != 16 || id != strings.ToLower(id) {
			t.Fatalf("invalid generated ID %q", id)
		}

		uuid, err := GenerateUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		compact := strings.ReplaceAll(uuid, "-", "")
		uuidBytes, decodeErr := hex.DecodeString(compact)
		if decodeErr != nil || len(uuid) != 36 || len(uuidBytes) != 16 || uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' || uuidBytes[6]>>4 != 4 || uuidBytes[8]>>6 != 2 {
			t.Fatalf("invalid generated UUIDv4 %q", uuid)
		}

		mac, err := GenerateMAC()
		if err != nil {
			t.Fatal(err)
		}
		hardware, parseErr := net.ParseMAC(mac)
		if parseErr != nil || len(hardware) != 6 || hardware[0]&1 != 0 || hardware[0]&2 == 0 || mac != strings.ToLower(mac) {
			t.Fatalf("invalid generated MAC %q", mac)
		}
	}
}

func TestDecodeRejectsUnknownAndTrailingJSON(t *testing.T) {
	canonical, err := CanonicalJSON(new(Config))
	if err == nil || canonical != nil {
		t.Fatal("invalid zero config unexpectedly encoded")
	}
	valid, err := json.Marshal(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(nil), valid[:len(valid)-1]...)
	unknown = append(unknown, []byte(`,"surprise":true}`)...)
	nestedUnknown := bytes.Replace(valid, []byte(`"firmware":{`), []byte(`"firmware":{"surprise":true,`), 1)
	for name, input := range map[string][]byte{
		"unknown top-level field": unknown,
		"unknown nested field":    nestedUnknown,
		"second object":           append(append([]byte(nil), valid...), []byte(` {}`)...),
		"trailing scalar":         append(append([]byte(nil), valid...), []byte(` true`)...),
		"null":                    []byte(`null`),
		"array":                   []byte(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeBytes(input); err == nil {
				t.Fatal("Decode succeeded")
			}
		})
	}
}

func TestValidationEnumsRangesAndArchitecture(t *testing.T) {
	mutations := map[string]func(*Config){
		"schema below":       func(c *Config) { c.SchemaVersion = 0 },
		"schema above":       func(c *Config) { c.SchemaVersion = 2 },
		"ID uppercase":       func(c *Config) { c.ID = "0123456789ABCDEF0123456789ABCDEF" },
		"ID short":           func(c *Config) { c.ID = c.ID[:31] },
		"name empty":         func(c *Config) { c.Name = "" },
		"name leading dash":  func(c *Config) { c.Name = "-vm" },
		"name too long":      func(c *Config) { c.Name = strings.Repeat("a", 64) },
		"backend":            func(c *Config) { c.Backend = "other" },
		"architecture":       func(c *Config) { c.Architecture = "x86_64" },
		"UUID version":       func(c *Config) { c.UUID = "123e4567-e89b-12d3-a456-426614174000" },
		"UUID variant":       func(c *Config) { c.UUID = "123e4567-e89b-42d3-7456-426614174000" },
		"CPUs zero":          func(c *Config) { c.CPUs = 0 },
		"CPUs above":         func(c *Config) { c.CPUs = 65 },
		"memory below":       func(c *Config) { c.MemoryMiB = 255 },
		"restart":            func(c *Config) { c.RestartPolicy = "always" },
		"timeout zero":       func(c *Config) { c.ShutdownTimeoutSeconds = 0 },
		"timeout above":      func(c *Config) { c.ShutdownTimeoutSeconds = 3601 },
		"firmware code":      func(c *Config) { c.Firmware.Code = "" },
		"firmware variables": func(c *Config) { c.Firmware.Variables = "" },
		"machine":            func(c *Config) { c.QEMU.Machine = "q35" },
		"autostart":          func(c *Config) { c.Autostart.Scope = "system" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { c := validTestConfig(); mutate(&c); requireInvalid(t, c) })
	}

	for _, backend := range []Backend{BackendQEMU, BackendVZ} {
		for _, restart := range []RestartPolicy{RestartNever, RestartOnFailure} {
			for _, scope := range []AutostartScope{AutostartNone, AutostartBoot, AutostartLogin} {
				c := validTestConfig()
				c.Backend, c.RestartPolicy, c.Autostart.Scope = backend, restart, scope
				if err := c.Validate(); err != nil {
					t.Fatalf("valid enum combination rejected: %v", err)
				}
			}
		}
	}
	for _, cpus := range []int{1, 64} {
		c := validTestConfig()
		c.CPUs = cpus
		if err := c.Validate(); err != nil {
			t.Fatalf("CPUs %d: %v", cpus, err)
		}
	}
	for _, timeout := range []int{1, 3600} {
		c := validTestConfig()
		c.ShutdownTimeoutSeconds = timeout
		if err := c.Validate(); err != nil {
			t.Fatalf("timeout %d: %v", timeout, err)
		}
	}
	for _, machine := range []string{"", "virt"} {
		c := validTestConfig()
		c.QEMU.Machine = machine
		if err := c.Validate(); err != nil {
			t.Fatalf("machine %q: %v", machine, err)
		}
	}
	boundary := validTestConfig()
	boundary.Name = strings.Repeat("a", 63)
	boundary.MemoryMiB = 256
	if err := boundary.Validate(); err != nil {
		t.Fatalf("valid name/memory boundaries rejected: %v", err)
	}
}

func TestStorageValidation(t *testing.T) {
	mutations := map[string]func(*Config){
		"installer empty path":     func(c *Config) { c.Installer.Path = "" },
		"installer negative index": func(c *Config) { c.Installer.BootIndex = -1 },
		"installer disk collision": func(c *Config) { c.Disks[0].BootIndex = c.Installer.BootIndex },
		"disk collision":           func(c *Config) { c.Disks[1].BootIndex = c.Disks[0].BootIndex },
		"disk empty path":          func(c *Config) { c.Disks[0].Path = "" },
		"disk negative index":      func(c *Config) { c.Disks[0].BootIndex = -1 },
		"format uppercase":         func(c *Config) { c.Disks[0].Format = "QCOW2" },
		"format other":             func(c *Config) { c.Disks[0].Format = "vmdk" },
		"serial empty":             func(c *Config) { c.Disks[0].Serial = "" },
		"serial unsafe":            func(c *Config) { c.Disks[0].Serial = "bad,serial" },
		"serial too long":          func(c *Config) { c.Disks[0].Serial = strings.Repeat("a", 65) },
		"serial duplicate":         func(c *Config) { c.Disks[1].Serial = c.Disks[0].Serial },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { c := validTestConfig(); mutate(&c); requireInvalid(t, c) })
	}
	for _, format := range []string{"qcow2", "raw"} {
		c := validTestConfig()
		c.Disks[0].Format = format
		c.Disks[0].Serial = strings.Repeat("Z", 64)
		if err := c.Validate(); err != nil {
			t.Fatalf("valid storage rejected: %v", err)
		}
	}
}

func TestNetworkValidation(t *testing.T) {
	mutations := map[string]func(*Config){
		"network enum":       func(c *Config) { c.Network.Mode = "bridge" },
		"MAC uppercase":      func(c *Config) { c.Network.MAC = "02:12:34:56:78:9A" },
		"MAC compact":        func(c *Config) { c.Network.MAC = "02123456789a" },
		"MAC multicast":      func(c *Config) { c.Network.MAC = "03:12:34:56:78:9a" },
		"MAC global":         func(c *Config) { c.Network.MAC = "00:12:34:56:78:9a" },
		"protocol":           func(c *Config) { c.Network.Forwards[0].Protocol = "sctp" },
		"hostname":           func(c *Config) { c.Network.Forwards[0].HostAddress = "localhost" },
		"IPv6":               func(c *Config) { c.Network.Forwards[0].HostAddress = "::1" },
		"host port zero":     func(c *Config) { c.Network.Forwards[0].HostPort = 0 },
		"guest port zero":    func(c *Config) { c.Network.Forwards[0].GuestPort = 0 },
		"duplicate listener": func(c *Config) { c.Network.Forwards[1] = c.Network.Forwards[0]; c.Network.Forwards[1].GuestPort++ },
		"user with socket config": func(c *Config) {
			c.Network.SocketVMNet = &SocketVMNetConfig{ClientPath: "/client", SocketPath: "/socket", Interface: "vlan0"}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { c := validTestConfig(); mutate(&c); requireInvalid(t, c) })
	}
	distinctListeners := validTestConfig()
	distinctListeners.Network.Forwards = []PortForward{
		{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 65535, GuestPort: 65535},
		{Protocol: "udp", HostAddress: "127.0.0.1", HostPort: 65535, GuestPort: 1},
		{Protocol: "tcp", HostAddress: "192.0.2.2", HostPort: 65535, GuestPort: 1},
	}
	if err := distinctListeners.Validate(); err != nil {
		t.Fatalf("distinct valid listeners rejected: %v", err)
	}

	bridge := func() Config {
		c := validTestConfig()
		c.Network.Mode = NetworkSocketVMNet
		c.Network.Forwards = nil
		c.Network.SocketVMNet = &SocketVMNetConfig{ClientPath: "/opt/socket_vmnet/bin/socket_vmnet_client", SocketPath: "/var/run/socket_vmnet.bridged.vlan0", Interface: "vlan0"}
		return c
	}
	validBridge := bridge()
	if err := validBridge.Validate(); err != nil {
		t.Fatalf("valid socket_vmnet rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"missing config": func(c *Config) { c.Network.SocketVMNet = nil },
		"forwards": func(c *Config) {
			c.Network.Forwards = []PortForward{{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 1, GuestPort: 1}}
		},
		"relative client": func(c *Config) { c.Network.SocketVMNet.ClientPath = "client" },
		"relative socket": func(c *Config) { c.Network.SocketVMNet.SocketPath = "socket" },
		"empty interface": func(c *Config) { c.Network.SocketVMNet.Interface = "" },
	} {
		t.Run("socket_vmnet "+name, func(t *testing.T) { c := bridge(); mutate(&c); requireInvalid(t, c) })
	}
}

func TestValidateApplyImmutableFieldsAndScope(t *testing.T) {
	current := validTestConfig()
	mutable := cloneTestConfig(t, current)
	mutable.CPUs++
	mutable.MemoryMiB++
	mutable.GuestAgent.Enabled = false
	if err := ValidateApply(&current, &mutable); err != nil {
		t.Fatalf("mutable update rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"schema":       func(c *Config) { c.SchemaVersion++ },
		"ID":           func(c *Config) { c.ID = "abcdef0123456789abcdef0123456789" },
		"name":         func(c *Config) { c.Name = "renamed" },
		"backend":      func(c *Config) { c.Backend = BackendVZ },
		"architecture": func(c *Config) { c.Architecture = "x86_64" },
		"scope":        func(c *Config) { c.Autostart.Scope = AutostartBoot },
	} {
		t.Run(name, func(t *testing.T) {
			replacement := cloneTestConfig(t, current)
			mutate(&replacement)
			if err := ValidateApply(&current, &replacement); err == nil {
				t.Fatal("immutable change accepted")
			}
		})
	}
}

func TestRuntimeVZError(t *testing.T) {
	c := validTestConfig()
	c.Backend = BackendVZ
	if err := c.Validate(); err != nil {
		t.Fatalf("VZ must be schema-valid: %v", err)
	}
	if err := c.ValidateRuntime(); err == nil || err.Error() != `backend "vz" is unavailable in this build` {
		t.Fatalf("unexpected VZ runtime error: %v", err)
	}
}

func TestManagerOwnedQEMUOptionsRejected(t *testing.T) {
	options := []string{"qmp", "monitor", "chardev", "serial", "daemonize", "pidfile", "run-with", "accel", "machine", "M", "cpu", "smp", "m", "drive", "blockdev", "device", "hda", "hdb", "hdc", "hdd", "fda", "fdb", "cdrom", "netdev", "nic", "net", "display", "nographic", "vga", "nodefaults", "name", "uuid", "boot", "bios", "readconfig", "writeconfig", "set", "global", "incoming", "snapshot", "S", "preconfig", "no-shutdown", "action"}
	for _, option := range options {
		for _, arg := range []string{"-" + option, "-" + option + "=value", "--" + option, "--" + option + "=value"} {
			t.Run(strings.ReplaceAll(arg, "/", "_"), func(t *testing.T) { c := validTestConfig(); c.QEMU.ExtraArgs = []string{arg}; requireInvalid(t, c) })
		}
	}
	for _, arg := range []string{"-Mnone", "-Mvirt", "-m4G", "-m256", "-m4096M"} {
		t.Run(arg, func(t *testing.T) { c := validTestConfig(); c.QEMU.ExtraArgs = []string{arg}; requireInvalid(t, c) })
	}
	c := validTestConfig()
	c.QEMU.ExtraArgs = []string{"-d", "guest_errors", "-trace=help", "--migrate-helper", "-msg", "timestamp=on", "plain-value"}
	if err := c.Validate(); err != nil {
		t.Fatalf("benign extras rejected: %v", err)
	}
}

package model

import (
	"bytes"
	"crypto/sha256"
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

func validTestVNC() *VNCConfig {
	return &VNCConfig{
		Bind:     "127.0.0.1",
		Port:     5900,
		PortTo:   5999,
		Password: "secret",
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
	const schemaPrefix = "{\n  \"$schema\": \"" + ConfigSchemaURL + "\""
	if !bytes.HasPrefix(first, []byte(schemaPrefix)) {
		t.Fatalf("canonical JSON lacks schema annotation prefix: %s", first)
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

func TestMetricsConfigContract(t *testing.T) {
	disabled := validTestConfig()
	disabledJSON, err := CanonicalJSON(&disabled)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(disabledJSON, []byte(`"metrics"`)) {
		t.Fatalf("disabled metrics unexpectedly persisted: %s", disabledJSON)
	}

	for _, port := range []uint16{1024, 65535} {
		config := validTestConfig()
		config.Metrics = &MetricsConfig{Port: port}
		data, err := CanonicalJSON(&config)
		if err != nil {
			t.Fatalf("port %d rejected: %v", port, err)
		}
		decoded, err := DecodeBytes(data)
		if err != nil {
			t.Fatalf("port %d round trip failed: %v", port, err)
		}
		if decoded.Metrics == nil || decoded.Metrics.Port != port {
			t.Fatalf("port %d round trip = %#v", port, decoded.Metrics)
		}
		withoutMetrics := config
		withoutMetrics.Metrics = nil
		enabledHash, err := Hash(&config)
		if err != nil {
			t.Fatal(err)
		}
		disabledHash, err := Hash(&withoutMetrics)
		if err != nil {
			t.Fatal(err)
		}
		if enabledHash == disabledHash {
			t.Fatalf("metrics port %d did not change configuration hash", port)
		}
	}

	for _, port := range []uint16{0, 1023} {
		config := validTestConfig()
		config.Metrics = &MetricsConfig{Port: port}
		requireInvalid(t, config)
	}

	valid, err := json.Marshal(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	for name, member := range map[string]string{
		"overflow":      `"metrics":{"port":65536}`,
		"unknown field": `"metrics":{"port":1024,"bind":"0.0.0.0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			document := append([]byte(`{"$schema":"`+ConfigSchemaURL+`",`), valid[1:len(valid)-1]...)
			document = append(document, ',')
			document = append(document, member...)
			document = append(document, '}')
			if _, err := DecodeBytes(document); err == nil {
				t.Fatal("DecodeBytes() accepted invalid metrics configuration")
			}
		})
	}
}

func TestCanonicalCompatibilityOmitsUSBAndDiskOptions(t *testing.T) {
	config := Config{
		SchemaVersion:          SchemaVersion,
		ID:                     "0123456789abcdef0123456789abcdef",
		Name:                   "compat",
		Backend:                BackendQEMU,
		Architecture:           "aarch64",
		UUID:                   "123e4567-e89b-42d3-a456-426614174000",
		CPUs:                   2,
		MemoryMiB:              2048,
		RestartPolicy:          RestartNever,
		ShutdownTimeoutSeconds: 180,
		Firmware:               FirmwareConfig{Code: "firmware-code.fd", Variables: "firmware-vars.fd"},
		Disks:                  []DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk-0123456789abcdef", BootIndex: 0}},
		Network:                NetworkConfig{Mode: NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []PortForward{}},
		GuestAgent:             GuestAgentConfig{},
		QEMU:                   QEMUConfig{Binary: "/usr/bin/qemu-system-aarch64", ImageTool: "/usr/bin/qemu-img", Machine: "virt", ExtraArgs: []string{}},
		Autostart:              AutostartConfig{Scope: AutostartNone},
	}
	data, err := CanonicalJSON(&config)
	if err != nil {
		t.Fatal(err)
	}
	const wantConfig = "{\n" +
		"  \"schema_version\": 1,\n" +
		"  \"id\": \"0123456789abcdef0123456789abcdef\",\n" +
		"  \"name\": \"compat\",\n" +
		"  \"backend\": \"qemu\",\n" +
		"  \"architecture\": \"aarch64\",\n" +
		"  \"uuid\": \"123e4567-e89b-42d3-a456-426614174000\",\n" +
		"  \"cpus\": 2,\n" +
		"  \"memory_mib\": 2048,\n" +
		"  \"restart_policy\": \"never\",\n" +
		"  \"shutdown_timeout_seconds\": 180,\n" +
		"  \"firmware\": {\n" +
		"    \"code\": \"firmware-code.fd\",\n" +
		"    \"variables\": \"firmware-vars.fd\"\n" +
		"  },\n" +
		"  \"disks\": [\n" +
		"    {\n" +
		"      \"path\": \"disk.qcow2\",\n" +
		"      \"format\": \"qcow2\",\n" +
		"      \"serial\": \"disk-0123456789abcdef\",\n" +
		"      \"boot_index\": 0,\n" +
		"      \"read_only\": false\n" +
		"    }\n" +
		"  ],\n" +
		"  \"network\": {\n" +
		"    \"mode\": \"user\",\n" +
		"    \"mac\": \"02:00:00:00:00:01\",\n" +
		"    \"forwards\": []\n" +
		"  },\n" +
		"  \"guest_agent\": {\n" +
		"    \"enabled\": false\n" +
		"  },\n" +
		"  \"qemu\": {\n" +
		"    \"binary\": \"/usr/bin/qemu-system-aarch64\",\n" +
		"    \"image_tool\": \"/usr/bin/qemu-img\",\n" +
		"    \"machine\": \"virt\",\n" +
		"    \"extra_args\": []\n" +
		"  },\n" +
		"  \"autostart\": {\n" +
		"    \"scope\": \"none\"\n" +
		"  }\n" +
		"}\n"
	wantDocument := strings.Replace(wantConfig, "{\n", "{\n  \"$schema\": \""+ConfigSchemaURL+"\",\n", 1)
	if string(data) != wantDocument {
		t.Fatalf("canonical JSON changed\n got:\n%s\nwant:\n%s", data, wantDocument)
	}
	if strings.Contains(wantConfig, "\"usb\"") || strings.Contains(wantConfig, "\"cache\"") {
		t.Fatal("expected fixture unexpectedly contains omitted fields")
	}
	hash, err := Hash(&config)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(wantConfig))
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash=%q want=%q", hash, hex.EncodeToString(sum[:]))
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
	config := validTestConfig()
	config.VNC = validTestVNC()
	valid, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(nil), valid[:len(valid)-1]...)
	unknown = append(unknown, []byte(`,"surprise":true}`)...)
	nestedUnknown := bytes.Replace(valid, []byte(`"firmware":{`), []byte(`"firmware":{"surprise":true,`), 1)
	vncUnknown := bytes.Replace(valid, []byte(`"vnc":{`), []byte(`"vnc":{"surprise":true,`), 1)
	for name, input := range map[string][]byte{
		"unknown top-level field": unknown,
		"unknown nested field":    nestedUnknown,
		"unknown VNC field":       vncUnknown,
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

func TestDecodeSchemaAnnotationCompatibility(t *testing.T) {
	legacy, err := json.Marshal(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig()
	annotated, err := CanonicalJSON(&config)
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{"legacy": legacy, "annotated": annotated} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeBytes(input); err != nil {
				t.Fatalf("Decode failed: %v", err)
			}
		})
	}
	for name, value := range map[string]string{
		"different URL": `"https://example.com/schema.json"`,
		"empty string":  `""`,
		"null":          `null`,
		"number":        `42`,
		"object":        `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := bytes.Replace(legacy, []byte("{"), []byte(`{"$schema":`+value+`,`), 1)
			_, err := DecodeBytes(input)
			want := `config: $schema must be "` + ConfigSchemaURL + `"`
			if err == nil || err.Error() != want {
				t.Fatalf("Decode error = %v, want %q", err, want)
			}
		})
	}
}

func TestDecodeCompatibilityAllowsMissingOptionalKeyboardAndRTC(t *testing.T) {
	config := validTestConfig()
	config.VNC = validTestVNC()
	data, err := CanonicalJSON(&config)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"keyboard_layout"`)) {
		t.Fatalf("canonical JSON unexpectedly included keyboard_layout: %s", data)
	}
	if bytes.Contains(data, []byte(`"rtc_base"`)) {
		t.Fatalf("canonical JSON unexpectedly included rtc_base: %s", data)
	}
	decoded, err := DecodeBytes(data)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	roundTrip, err := CanonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, data) {
		t.Fatalf("canonical round trip mismatch\n got: %s\nwant: %s", roundTrip, data)
	}
}

func TestVNCValidationAndHashStability(t *testing.T) {
	disabled := validTestConfig()
	if err := disabled.Validate(); err != nil {
		t.Fatalf("nil VNC should be valid: %v", err)
	}
	disabledHash, err := Hash(&disabled)
	if err != nil {
		t.Fatal(err)
	}

	enabled := cloneTestConfig(t, disabled)
	enabled.VNC = validTestVNC()
	if err := enabled.Validate(); err != nil {
		t.Fatalf("valid VNC rejected: %v", err)
	}
	enabledHash, err := Hash(&enabled)
	if err != nil {
		t.Fatal(err)
	}
	if enabledHash == disabledHash {
		t.Fatal("VNC did not affect config hash")
	}

	for _, password := range []string{"a", "éééé"} {
		t.Run("valid password "+password, func(t *testing.T) {
			c := cloneTestConfig(t, disabled)
			c.VNC = validTestVNC()
			c.VNC.Password = password
			if err := c.Validate(); err != nil {
				t.Fatalf("valid password %q rejected: %v", password, err)
			}
		})
	}
	for _, layout := range []string{"", "en-us", "fr-ca"} {
		t.Run("valid layout "+layout, func(t *testing.T) {
			c := cloneTestConfig(t, disabled)
			c.VNC = validTestVNC()
			c.VNC.KeyboardLayout = layout
			if err := c.Validate(); err != nil {
				t.Fatalf("valid keyboard layout %q rejected: %v", layout, err)
			}
		})
	}

	for name, mutate := range map[string]func(*Config){
		"bind hostname":    func(c *Config) { c.VNC.Bind = "localhost" },
		"bind IPv6":        func(c *Config) { c.VNC.Bind = "::1" },
		"port below":       func(c *Config) { c.VNC.Port = 5899 },
		"port_to below":    func(c *Config) { c.VNC.Port = 5901; c.VNC.PortTo = 5900 },
		"password empty":   func(c *Config) { c.VNC.Password = "" },
		"password 9 bytes": func(c *Config) { c.VNC.Password = "123456789" },
		"password NUL":     func(c *Config) { c.VNC.Password = "abc\x00def" },
		"password invalid UTF-8": func(c *Config) {
			c.VNC.Password = string([]byte{0xff})
		},
		"layout invalid": func(c *Config) { c.VNC.KeyboardLayout = "en_US" },
	} {
		t.Run(name, func(t *testing.T) {
			c := cloneTestConfig(t, disabled)
			c.VNC = validTestVNC()
			mutate(&c)
			requireInvalid(t, c)
		})
	}

	boundary := cloneTestConfig(t, disabled)
	boundary.VNC = validTestVNC()
	boundary.VNC.Bind = "0.0.0.0"
	boundary.VNC.Port = 65535
	boundary.VNC.PortTo = 65535
	boundary.VNC.Password = "12345678"
	if err := boundary.Validate(); err != nil {
		t.Fatalf("valid VNC boundaries rejected: %v", err)
	}
}

func TestDecodeRejectsInvalidRawUnicodeInVNCPassword(t *testing.T) {
	config := validTestConfig()
	config.VNC = validTestVNC()
	valid, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		raw     []byte
		wantErr string
	}{
		{name: "raw invalid UTF-8", raw: []byte{'"', 0xff, '"'}, wantErr: "raw JSON must be valid UTF-8"},
		{name: "lone high surrogate", raw: []byte(`"\uD800"`), wantErr: "unpaired surrogate escapes"},
		{name: "lone low surrogate", raw: []byte(`"\uDC00"`), wantErr: "unpaired surrogate escapes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := bytes.Replace(valid, []byte(`"password":"secret"`), append([]byte(`"password":`), tc.raw...), 1)
			if bytes.Equal(input, valid) {
				t.Fatal("password replacement failed")
			}
			if _, err := DecodeBytes(input); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecodeBytes() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}

	input := bytes.Replace(valid, []byte(`"password":"secret"`), []byte(`"password":"\uD83D\uDE00"`), 1)
	decoded, err := DecodeBytes(input)
	if err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
	if decoded.VNC == nil || decoded.VNC.Password != "😀" {
		t.Fatalf("decoded VNC password = %#v, want %#v", decoded.VNC, "😀")
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
		"rtc base":           func(c *Config) { c.QEMU.RTCBase = "gmt" },
		"autostart":          func(c *Config) { c.Autostart.Scope = "system" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { c := validTestConfig(); mutate(&c); requireInvalid(t, c) })
	}

	for _, restart := range []RestartPolicy{RestartNever, RestartOnFailure} {
		for _, scope := range []AutostartScope{AutostartNone, AutostartBoot, AutostartLogin} {
			c := validTestConfig()
			c.RestartPolicy, c.Autostart.Scope = restart, scope
			if err := c.Validate(); err != nil {
				t.Fatalf("valid enum combination rejected: %v", err)
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
	for _, machine := range []string{"", "virt", "virt-11.0", "virt-123.45"} {
		c := validTestConfig()
		c.QEMU.Machine = machine
		if err := c.Validate(); err != nil {
			t.Fatalf("machine %q: %v", machine, err)
		}
	}
	for _, machine := range []string{"virt-0.1", "virt-11", "virt-11.x"} {
		c := validTestConfig()
		c.QEMU.Machine = machine
		requireInvalid(t, c)
	}
	for _, base := range []string{"", "utc", "localtime"} {
		c := validTestConfig()
		c.QEMU.RTCBase = base
		if err := c.Validate(); err != nil {
			t.Fatalf("rtc_base %q: %v", base, err)
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
		"cache uppercase":          func(c *Config) { c.Disks[0].Cache = "Writeback" },
		"cache other":              func(c *Config) { c.Disks[0].Cache = "passthrough" },
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
	for _, cache := range []string{"", "none", "writeback", "writethrough", "directsync", "unsafe"} {
		c := validTestConfig()
		c.Disks[0].Cache = cache
		if err := c.Validate(); err != nil {
			t.Fatalf("valid cache %q rejected: %v", cache, err)
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
		"smb relative": func(c *Config) {
			c.Network.SMBFolder = "relative/share"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) { c := validTestConfig(); mutate(&c); requireInvalid(t, c) })
	}

	userShare := validTestConfig()
	userShare.Network.SMBFolder = "/absolute/share"
	if err := userShare.Validate(); err != nil {
		t.Fatalf("valid user smb_folder rejected: %v", err)
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
		"smb folder":      func(c *Config) { c.Network.SMBFolder = "/absolute/share" },
		"relative client": func(c *Config) { c.Network.SocketVMNet.ClientPath = "client" },
		"relative socket": func(c *Config) { c.Network.SocketVMNet.SocketPath = "socket" },
		"empty interface": func(c *Config) { c.Network.SocketVMNet.Interface = "" },
	} {
		t.Run("socket_vmnet "+name, func(t *testing.T) { c := bridge(); mutate(&c); requireInvalid(t, c) })
	}
}

func TestValidateHostPortConflicts(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		wantErr   string
	}{
		{
			name: "tcp wildcard after concrete conflicts",
			configure: func(c *Config) {
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 6000, GuestPort: 22},
					{Protocol: "tcp", HostAddress: "0.0.0.0", HostPort: 6000, GuestPort: 23},
				}
			},
			wantErr: "config: network forward 1 on 0.0.0.0 conflicts with network forward 0 on 127.0.0.1 for tcp port 6000",
		},
		{
			name: "tcp concrete after wildcard conflicts",
			configure: func(c *Config) {
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "0.0.0.0", HostPort: 6000, GuestPort: 22},
					{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 6000, GuestPort: 23},
				}
			},
			wantErr: "config: network forward 1 on 127.0.0.1 conflicts with network forward 0 on 0.0.0.0 for tcp port 6000",
		},
		{
			name: "metrics conflicts with tcp forward",
			configure: func(c *Config) {
				c.Metrics = &MetricsConfig{Port: 6001}
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "0.0.0.0", HostPort: 6001, GuestPort: 22},
				}
			},
			wantErr: "config: metrics port 6001 conflicts with network forward 0 on 0.0.0.0",
		},
		{
			name: "metrics conflicts with single-port vnc",
			configure: func(c *Config) {
				c.Metrics = &MetricsConfig{Port: 5905}
				c.VNC = validTestVNC()
				c.VNC.Bind = "0.0.0.0"
				c.VNC.Port = 5905
				c.VNC.PortTo = 5905
			},
			wantErr: "config: metrics port 5905 conflicts with vnc listener on 0.0.0.0",
		},
		{
			name: "single-port vnc conflicts with tcp forward",
			configure: func(c *Config) {
				c.VNC = validTestVNC()
				c.VNC.Bind = "127.0.0.1"
				c.VNC.Port = 5906
				c.VNC.PortTo = 5906
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "0.0.0.0", HostPort: 5906, GuestPort: 22},
				}
			},
			wantErr: "config: vnc port 5906 on 127.0.0.1 conflicts with network forward 0 on 0.0.0.0",
		},
		{
			name: "tcp and udp may share host port",
			configure: func(c *Config) {
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 6002, GuestPort: 22},
					{Protocol: "udp", HostAddress: "127.0.0.1", HostPort: 6002, GuestPort: 53},
				}
			},
		},
		{
			name: "distinct concrete tcp binds may share host port",
			configure: func(c *Config) {
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 6003, GuestPort: 22},
					{Protocol: "tcp", HostAddress: "192.0.2.1", HostPort: 6003, GuestPort: 23},
				}
			},
		},
		{
			name: "udp forward may match metrics port",
			configure: func(c *Config) {
				c.Metrics = &MetricsConfig{Port: 6004}
				c.Network.Forwards = []PortForward{
					{Protocol: "udp", HostAddress: "127.0.0.1", HostPort: 6004, GuestPort: 53},
				}
			},
		},
		{
			name: "udp forward may match vnc port",
			configure: func(c *Config) {
				c.VNC = validTestVNC()
				c.VNC.Port = 5907
				c.VNC.PortTo = 5907
				c.Network.Forwards = []PortForward{
					{Protocol: "udp", HostAddress: "127.0.0.1", HostPort: 5907, GuestPort: 53},
				}
			},
		},
		{
			name: "metrics may use port inside multi-port vnc range",
			configure: func(c *Config) {
				c.Metrics = &MetricsConfig{Port: 5909}
				c.VNC = validTestVNC()
				c.VNC.Port = 5908
				c.VNC.PortTo = 5910
			},
		},
		{
			name: "tcp forward may use first port of multi-port vnc range",
			configure: func(c *Config) {
				c.VNC = validTestVNC()
				c.VNC.Port = 5911
				c.VNC.PortTo = 5912
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 5911, GuestPort: 22},
				}
			},
		},
		{
			name: "disjoint listeners remain valid",
			configure: func(c *Config) {
				c.Metrics = &MetricsConfig{Port: 6005}
				c.VNC = validTestVNC()
				c.VNC.Bind = "192.0.2.2"
				c.VNC.Port = 5913
				c.VNC.PortTo = 5913
				c.Network.Forwards = []PortForward{
					{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 6006, GuestPort: 22},
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := validTestConfig()
			config.Network.Forwards = nil
			config.Metrics = nil
			config.VNC = nil
			tc.configure(&config)

			err := config.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestUSBValidationSelectorsDuplicatesAndCapacity(t *testing.T) {
	valid := validTestConfig()
	valid.USB = []USBDeviceConfig{
		{VendorID: "0001", ProductID: "ffff"},
		{HostBus: 255, HostAddress: 127},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid usb selectors rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"empty":            func(c *Config) { c.USB = []USBDeviceConfig{{}} },
		"vendor only":      func(c *Config) { c.USB = []USBDeviceConfig{{VendorID: "0001"}} },
		"product zero":     func(c *Config) { c.USB = []USBDeviceConfig{{VendorID: "0001", ProductID: "0000"}} },
		"vendor zero":      func(c *Config) { c.USB = []USBDeviceConfig{{VendorID: "0000", ProductID: "0001"}} },
		"vendor uppercase": func(c *Config) { c.USB = []USBDeviceConfig{{VendorID: "ABCD", ProductID: "0001"}} },
		"mixed selectors": func(c *Config) {
			c.USB = []USBDeviceConfig{{VendorID: "0001", ProductID: "0002", HostBus: 1, HostAddress: 2}}
		},
		"bus only":         func(c *Config) { c.USB = []USBDeviceConfig{{HostBus: 1}} },
		"address too high": func(c *Config) { c.USB = []USBDeviceConfig{{HostBus: 1, HostAddress: 128}} },
		"duplicate vendor pair": func(c *Config) {
			c.USB = []USBDeviceConfig{{VendorID: "0001", ProductID: "0002"}, {VendorID: "0001", ProductID: "0002"}}
		},
		"duplicate bus address": func(c *Config) { c.USB = []USBDeviceConfig{{HostBus: 2, HostAddress: 3}, {HostBus: 2, HostAddress: 3}} },
	} {
		t.Run(name, func(t *testing.T) { c := validTestConfig(); mutate(&c); requireInvalid(t, c) })
	}
	withoutVNC := validTestConfig()
	withoutVNC.USB = []USBDeviceConfig{
		{VendorID: "0001", ProductID: "0001"},
		{VendorID: "0001", ProductID: "0002"},
		{HostBus: 1, HostAddress: 1},
		{HostBus: 1, HostAddress: 2},
	}
	if err := withoutVNC.Validate(); err != nil {
		t.Fatalf("four usb devices without VNC rejected: %v", err)
	}
	withoutVNC.USB = append(withoutVNC.USB, USBDeviceConfig{HostBus: 1, HostAddress: 3})
	requireInvalid(t, withoutVNC)

	withVNC := validTestConfig()
	withVNC.VNC = validTestVNC()
	withVNC.USB = []USBDeviceConfig{
		{VendorID: "0001", ProductID: "0001"},
		{HostBus: 1, HostAddress: 1},
	}
	if err := withVNC.Validate(); err != nil {
		t.Fatalf("two usb devices with VNC rejected: %v", err)
	}
	withVNC.USB = append(withVNC.USB, USBDeviceConfig{HostBus: 1, HostAddress: 2})
	requireInvalid(t, withVNC)
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

func TestManagerOwnedQEMUOptionsRejected(t *testing.T) {
	options := []string{"qmp", "monitor", "mon", "chardev", "serial", "daemonize", "pidfile", "run-with", "accel", "machine", "M", "cpu", "smp", "m", "drive", "blockdev", "device", "hda", "hdb", "hdc", "hdd", "fda", "fdb", "cdrom", "netdev", "nic", "net", "display", "nographic", "vga", "nodefaults", "name", "uuid", "boot", "bios", "readconfig", "writeconfig", "set", "global", "incoming", "snapshot", "S", "preconfig", "no-shutdown", "action", "vnc", "object", "k", "rtc"}
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

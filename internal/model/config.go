// Package model defines the strict, versioned durable VM configuration format,
// its validation rules, and the canonical encodings and hashes shared across
// persistence and runtime coordination.

package model

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const SchemaVersion = 1
const ConfigSchemaURL = "https://raw.githubusercontent.com/bradsjm/qemu-manage/main/schema.json"

var encodedConfigSchemaURL = json.RawMessage(`"` + ConfigSchemaURL + `"`)

type configDocument struct {
	Schema json.RawMessage `json:"$schema,omitempty"`
	Config
}

// Backend identifies the durable VM backend selected in Config. Validation
// currently accepts only BackendQEMU.
type Backend string

const (
	BackendQEMU Backend = "qemu"
)

// RestartPolicy controls whether a stopped VM should be restarted
// automatically. Valid values are RestartNever and RestartOnFailure.
type RestartPolicy string

const (
	RestartNever     RestartPolicy = "never"
	RestartOnFailure RestartPolicy = "on-failure"
)

// NetworkMode selects the guest networking implementation for the desired
// state. Valid values are NetworkUser and NetworkSocketVMNet.
type NetworkMode string

const (
	NetworkUser        NetworkMode = "user"
	NetworkSocketVMNet NetworkMode = "socket_vmnet"
)

// AutostartScope selects which host lifecycle may auto-start a VM. Valid
// values are AutostartNone, AutostartBoot, and AutostartLogin.
type AutostartScope string

const (
	AutostartNone  AutostartScope = "none"
	AutostartBoot  AutostartScope = "boot"
	AutostartLogin AutostartScope = "login"
)

// RunState names the coarse runtime states reported by supervisor metadata and
// status APIs. Valid values are RunStateStarting, RunStateRunning,
// RunStatePaused, RunStateStopping, RunStateStopped, and RunStateFailed.
type RunState string

const (
	RunStateStarting RunState = "starting"
	RunStateRunning  RunState = "running"
	RunStatePaused   RunState = "paused"
	RunStateStopping RunState = "stopping"
	RunStateStopped  RunState = "stopped"
	RunStateFailed   RunState = "failed"
)

// Config is the strict, versioned durable desired state for one VM. It
// captures validated hardware, storage, networking, autostart, and optional
// listener settings for persistence and hashing; authenticated live state,
// readiness, and exit metadata are tracked outside the config document.
type Config struct {
	SchemaVersion          int               `json:"schema_version"`
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	Backend                Backend           `json:"backend"`
	Architecture           string            `json:"architecture"`
	UUID                   string            `json:"uuid"`
	CPUs                   int               `json:"cpus"`
	MemoryMiB              int               `json:"memory_mib"`
	RestartPolicy          RestartPolicy     `json:"restart_policy"`
	ShutdownTimeoutSeconds int               `json:"shutdown_timeout_seconds"`
	Firmware               FirmwareConfig    `json:"firmware"`
	Installer              *InstallerConfig  `json:"installer,omitempty"`
	Disks                  []DiskConfig      `json:"disks"`
	Network                NetworkConfig     `json:"network"`
	GuestAgent             GuestAgentConfig  `json:"guest_agent"`
	VNC                    *VNCConfig        `json:"vnc,omitempty"`
	Metrics                *MetricsConfig    `json:"metrics,omitempty"`
	USB                    []USBDeviceConfig `json:"usb,omitempty"`
	QEMU                   QEMUConfig        `json:"qemu"`
	Autostart              AutostartConfig   `json:"autostart"`
}

type FirmwareConfig struct {
	Code      string `json:"code"`
	Variables string `json:"variables"`
}

// MetricsConfig enables the per-VM monitoring HTTP endpoint on 127.0.0.1. Port
// must be in the inclusive range 1024-65535 because the listener is loopback-
// only and never binds wildcard or external addresses.
type MetricsConfig struct {
	Port uint16 `json:"port"`
}

type InstallerConfig struct {
	Path      string `json:"path"`
	BootIndex int    `json:"boot_index"`
}

type DiskConfig struct {
	Path      string `json:"path"`
	Format    string `json:"format"`
	Serial    string `json:"serial"`
	BootIndex int    `json:"boot_index"`
	ReadOnly  bool   `json:"read_only"`
	Cache     string `json:"cache,omitempty"`
}

type USBDeviceConfig struct {
	VendorID    string `json:"vendor_id,omitempty"`
	ProductID   string `json:"product_id,omitempty"`
	HostBus     int    `json:"host_bus,omitempty"`
	HostAddress int    `json:"host_address,omitempty"`
}

type NetworkConfig struct {
	Mode        NetworkMode        `json:"mode"`
	MAC         string             `json:"mac"`
	Forwards    []PortForward      `json:"forwards"`
	SocketVMNet *SocketVMNetConfig `json:"socket_vmnet,omitempty"`
	SMBFolder   string             `json:"smb_folder,omitempty"`
}

type PortForward struct {
	Protocol    string `json:"protocol"`
	HostAddress string `json:"host_address"`
	HostPort    uint16 `json:"host_port"`
	GuestPort   uint16 `json:"guest_port"`
}

type SocketVMNetConfig struct {
	ClientPath string `json:"client_path"`
	SocketPath string `json:"socket_path"`
	Interface  string `json:"interface"`
}

type GuestAgentConfig struct {
	Enabled bool `json:"enabled"`
}

// VNCConfig enables an optional VNC listener on a validated IPv4 bind address.
// A bind of 127.0.0.1 keeps access local, 0.0.0.0 overlaps every concrete
// IPv4 bind for conflict checks, PortTo permits QEMU's inclusive fallback
// scan, and Password must be valid UTF-8, contain no NUL bytes, and occupy
// 1-8 bytes even when that differs from rune count.
type VNCConfig struct {
	Bind           string `json:"bind"`
	Port           uint16 `json:"port"`
	PortTo         uint16 `json:"port_to"`
	Password       string `json:"password"`
	KeyboardLayout string `json:"keyboard_layout,omitempty"`
}

type QEMUConfig struct {
	Binary    string   `json:"binary"`
	ImageTool string   `json:"image_tool"`
	Machine   string   `json:"machine"`
	RTCBase   string   `json:"rtc_base,omitempty"`
	ExtraArgs []string `json:"extra_args"`
}

type AutostartConfig struct {
	Scope AutostartScope `json:"scope"`
}

var (
	idPattern      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	namePattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	serialPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	macPattern     = regexp.MustCompile(`^(?:[0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	usbIDPattern   = regexp.MustCompile(`^[0-9a-f]{4}$`)
	machinePattern = regexp.MustCompile(`^virt-[1-9][0-9]*\.[0-9]+$`)
)

var keyboardLayouts = map[string]struct{}{
	"ar": {}, "da": {}, "de": {}, "de-ch": {}, "en-gb": {}, "en-us": {}, "es": {}, "et": {},
	"fi": {}, "fo": {}, "fr": {}, "fr-be": {}, "fr-ca": {}, "fr-ch": {}, "hr": {}, "hu": {},
	"is": {}, "it": {}, "ja": {}, "lt": {}, "lv": {}, "mk": {}, "nl": {}, "nl-be": {},
	"no": {}, "pl": {}, "pt": {}, "pt-br": {}, "ru": {}, "sl": {}, "sv": {}, "th": {},
	"tr": {},
}

func validateRawJSONUnicode(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("config: decode: raw JSON must be valid UTF-8")
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		next, err := validateJSONStringUnicode(data, i+1)
		if err != nil {
			return err
		}
		i = next
	}
	return nil
}

func validateJSONStringUnicode(data []byte, start int) (int, error) {
	for i := start; i < len(data); {
		switch data[i] {
		case '"':
			return i, nil
		case '\\':
			if i+1 >= len(data) {
				return len(data), nil
			}
			if data[i+1] != 'u' {
				i += 2
				continue
			}
			code, ok := parseJSONHexEscape(data, i+2)
			if !ok {
				return len(data), nil
			}
			if code >= 0xDC00 && code <= 0xDFFF {
				return 0, errors.New("config: decode: JSON strings must not contain unpaired surrogate escapes")
			}
			if code >= 0xD800 && code <= 0xDBFF {
				if i+12 > len(data) {
					return len(data), nil
				}
				if data[i+6] != '\\' || data[i+7] != 'u' {
					return 0, errors.New("config: decode: JSON strings must not contain unpaired surrogate escapes")
				}
				next, ok := parseJSONHexEscape(data, i+8)
				if !ok {
					return len(data), nil
				}
				if next < 0xDC00 || next > 0xDFFF {
					return 0, errors.New("config: decode: JSON strings must not contain unpaired surrogate escapes")
				}
				i += 12
				continue
			}
			i += 6
		default:
			i++
		}
	}
	return len(data), nil
}

func parseJSONHexEscape(data []byte, start int) (uint16, bool) {
	if start+4 > len(data) {
		return 0, false
	}
	var value uint16
	for _, b := range data[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// Decode strictly decodes exactly one non-null JSON object and validates it.
func Decode(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if err := validateRawJSONUnicode(data); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("config: trailing data")
		}
		return nil, fmt.Errorf("config: trailing data: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("config: top-level value must be an object")
	}
	var document configDocument
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&document); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if document.Schema != nil {
		var schemaURL *string
		if err := json.Unmarshal(document.Schema, &schemaURL); err != nil || schemaURL == nil || *schemaURL != ConfigSchemaURL {
			return nil, configError("$schema must be %q", ConfigSchemaURL)
		}
	}
	if err := document.Config.Validate(); err != nil {
		return nil, err
	}
	return &document.Config, nil
}

func DecodeBytes(data []byte) (*Config, error) { return Decode(bytes.NewReader(data)) }

func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return configError("schema_version must be %d", SchemaVersion)
	}
	if !idPattern.MatchString(c.ID) {
		return configError("id must be exactly 32 lowercase hexadecimal characters")
	}
	if !namePattern.MatchString(c.Name) {
		return configError("invalid name %q", c.Name)
	}
	if c.Backend != BackendQEMU {
		return configError("invalid backend %q; valid values: qemu", c.Backend)
	}
	if c.Architecture != "aarch64" {
		return configError("architecture must be %q", "aarch64")
	}
	if !validUUIDv4(c.UUID) {
		return configError("uuid must be an RFC 4122 version 4 UUID")
	}
	if c.CPUs < 1 || c.CPUs > 64 {
		return configError("cpus must be between 1 and 64")
	}
	if c.MemoryMiB < 256 {
		return configError("memory_mib must be at least 256")
	}
	if c.RestartPolicy != RestartNever && c.RestartPolicy != RestartOnFailure {
		return configError("invalid restart_policy %q; valid values: never, on-failure", c.RestartPolicy)
	}
	if c.ShutdownTimeoutSeconds < 1 || c.ShutdownTimeoutSeconds > 3600 {
		return configError("shutdown_timeout_seconds must be between 1 and 3600")
	}
	if c.Metrics != nil && c.Metrics.Port < 1024 {
		return configError("metrics port must be between 1024 and 65535")
	}
	if c.Firmware.Code == "" || c.Firmware.Variables == "" {
		return configError("firmware code and variables paths are required")
	}
	if !validMachineType(c.QEMU.Machine) {
		return configError("qemu machine must be empty, %q, or match %q", "virt", machinePattern.String())
	}
	if !validRTCBase(c.QEMU.RTCBase) {
		return configError("qemu rtc_base must be empty, %q, or %q", "utc", "localtime")
	}
	for _, arg := range c.QEMU.ExtraArgs {
		if forbiddenQEMUArg(arg) {
			return configError("qemu extra argument %q is manager-owned", arg)
		}
	}
	if c.Autostart.Scope != AutostartNone && c.Autostart.Scope != AutostartBoot && c.Autostart.Scope != AutostartLogin {
		return configError("invalid autostart scope %q; valid values: none, boot, login", c.Autostart.Scope)
	}
	if err := validateStorage(c); err != nil {
		return err
	}
	if err := validateNetwork(c.Network); err != nil {
		return err
	}
	if err := validateVNC(c.VNC); err != nil {
		return err
	}
	if err := validateHostPortConflicts(c); err != nil {
		return err
	}
	if err := validateUSB(c); err != nil {
		return err
	}
	return nil
}

func validateStorage(c *Config) error {
	indexes := make(map[int]string, len(c.Disks)+1)
	if c.Installer != nil {
		if c.Installer.Path == "" {
			return configError("installer path is required")
		}
		if c.Installer.BootIndex < 0 {
			return configError("installer boot_index must be nonnegative")
		}
		indexes[c.Installer.BootIndex] = "installer"
	}
	serials := make(map[string]struct{}, len(c.Disks))
	for i, disk := range c.Disks {
		if disk.Path == "" {
			return configError("disk %d path is required", i)
		}
		if disk.Format != "qcow2" && disk.Format != "raw" {
			return configError("disk %d has invalid format %q; valid values: qcow2, raw", i, disk.Format)
		}
		switch disk.Cache {
		case "", "none", "writeback", "writethrough", "directsync", "unsafe":
		default:
			return configError(
				"disk %d has invalid cache %q; valid values: none, writeback, writethrough, directsync, unsafe",
				i,
				disk.Cache,
			)
		}
		if disk.BootIndex < 0 {
			return configError("disk %d boot_index must be nonnegative", i)
		}
		if owner, exists := indexes[disk.BootIndex]; exists {
			return configError("boot_index %d is used by both %s and disk %d", disk.BootIndex, owner, i)
		}
		indexes[disk.BootIndex] = fmt.Sprintf("disk %d", i)
		if !serialPattern.MatchString(disk.Serial) {
			return configError("disk %d serial is invalid", i)
		}
		if _, exists := serials[disk.Serial]; exists {
			return configError("disk serial %q is duplicated", disk.Serial)
		}
		serials[disk.Serial] = struct{}{}
	}
	return nil
}

func validateUSB(c *Config) error {
	limit := 4
	if c.VNC != nil {
		limit = 2
	}
	if len(c.USB) > limit {
		return configError("usb supports at most %d devices with current VNC settings", limit)
	}
	selectors := make(map[string]struct{}, len(c.USB))
	for i, usb := range c.USB {
		vendorSet := usb.VendorID != "" || usb.ProductID != ""
		busSet := usb.HostBus != 0 || usb.HostAddress != 0
		var selector string
		switch {
		case vendorSet && busSet:
			return configError("usb %d must use either vendor/product or bus/address, not both", i)
		case vendorSet:
			if usb.VendorID == "" || usb.ProductID == "" {
				return configError("usb %d vendor/product selector requires both fields", i)
			}
			if !usbIDPattern.MatchString(usb.VendorID) || usb.VendorID == "0000" {
				return configError("usb %d vendor_id must be a lowercase four-digit hexadecimal value between 0001 and ffff", i)
			}
			if !usbIDPattern.MatchString(usb.ProductID) || usb.ProductID == "0000" {
				return configError("usb %d product_id must be a lowercase four-digit hexadecimal value between 0001 and ffff", i)
			}
			selector = "vp:" + usb.VendorID + ":" + usb.ProductID
		case busSet:
			if usb.HostBus < 1 || usb.HostBus > 255 {
				return configError("usb %d host_bus must be between 1 and 255", i)
			}
			if usb.HostAddress < 1 || usb.HostAddress > 127 {
				return configError("usb %d host_address must be between 1 and 127", i)
			}
			if usb.VendorID != "" || usb.ProductID != "" {
				return configError("usb %d bus/address selector must not include vendor/product", i)
			}
			selector = fmt.Sprintf("ba:%d:%d", usb.HostBus, usb.HostAddress)
		default:
			return configError("usb %d selector is required", i)
		}
		if _, exists := selectors[selector]; exists {
			return configError("usb %d duplicates an earlier selector", i)
		}
		selectors[selector] = struct{}{}
	}
	return nil
}

func validateNetwork(network NetworkConfig) error {
	if !macPattern.MatchString(network.MAC) {
		return configError("network mac must use canonical six-byte colon-separated hexadecimal syntax")
	}
	hardware, err := net.ParseMAC(network.MAC)
	if err != nil || hardware[0]&0x01 != 0 || hardware[0]&0x02 == 0 {
		return configError("network mac must be a locally administered unicast MAC")
	}
	switch network.Mode {
	case NetworkUser:
		if network.SocketVMNet != nil {
			return configError("socket_vmnet configuration is forbidden in user network mode")
		}
		if network.SMBFolder != "" && !filepath.IsAbs(network.SMBFolder) {
			return configError("smb_folder must be an absolute path")
		}
	case NetworkSocketVMNet:
		if len(network.Forwards) != 0 {
			return configError("forwards are forbidden in socket_vmnet network mode")
		}
		if network.SMBFolder != "" {
			return configError("smb_folder is forbidden in socket_vmnet network mode")
		}
		if network.SocketVMNet == nil {
			return configError("socket_vmnet configuration is required")
		}
		if !filepath.IsAbs(network.SocketVMNet.ClientPath) || !filepath.IsAbs(network.SocketVMNet.SocketPath) || network.SocketVMNet.Interface == "" {
			return configError("socket_vmnet client_path and socket_path must be absolute and interface must be nonempty")
		}
	default:
		return configError("invalid network mode %q; valid values: user, socket_vmnet", network.Mode)
	}

	tuples := make(map[string]struct{}, len(network.Forwards))
	for i, forward := range network.Forwards {
		if forward.Protocol != "tcp" && forward.Protocol != "udp" {
			return configError("forward %d has invalid protocol %q; valid values: tcp, udp", i, forward.Protocol)
		}
		address := net.ParseIP(forward.HostAddress)
		if address == nil || address.To4() == nil || strings.Contains(forward.HostAddress, ":") {
			return configError("forward %d host_address must be an IPv4 literal", i)
		}
		if forward.HostPort == 0 || forward.GuestPort == 0 {
			return configError("forward %d ports must be nonzero", i)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", strings.ToLower(forward.Protocol), address.To4().String(), forward.HostPort)
		if _, exists := tuples[key]; exists {
			return configError("forward %d duplicates an earlier tuple", i)
		}
		tuples[key] = struct{}{}
	}
	return nil
}

func validateVNC(vnc *VNCConfig) error {
	if vnc == nil {
		return nil
	}
	address := net.ParseIP(vnc.Bind)
	if address == nil || address.To4() == nil || strings.Contains(vnc.Bind, ":") {
		return configError("vnc bind must be an IPv4 literal")
	}
	if vnc.Port < 5900 {
		return configError("vnc port must be between 5900 and 65535")
	}
	if vnc.PortTo < vnc.Port {
		return configError("vnc port_to must be between port and 65535")
	}
	if !utf8.ValidString(vnc.Password) {
		return configError("vnc password must be valid UTF-8")
	}
	if len(vnc.Password) < 1 || len(vnc.Password) > 8 {
		return configError("vnc password must be between 1 and 8 bytes")
	}
	if strings.IndexByte(vnc.Password, 0) >= 0 {
		return configError("vnc password must not contain NUL")
	}
	if vnc.KeyboardLayout != "" && !validKeyboardLayout(vnc.KeyboardLayout) {
		return configError("vnc keyboard_layout %q is invalid", vnc.KeyboardLayout)
	}
	return nil
}

func tcpBindAddressesOverlap(first, second string) bool {
	return first == second || first == "0.0.0.0" || second == "0.0.0.0"
}

func validateHostPortConflicts(c *Config) error {
	for i, forward := range c.Network.Forwards {
		if forward.Protocol != "tcp" {
			continue
		}
		for earlier := range i {
			previous := c.Network.Forwards[earlier]
			if previous.Protocol != "tcp" {
				continue
			}
			if forward.HostPort == previous.HostPort && tcpBindAddressesOverlap(forward.HostAddress, previous.HostAddress) {
				return configError(
					"network forward %d on %s conflicts with network forward %d on %s for tcp port %d",
					i,
					forward.HostAddress,
					earlier,
					previous.HostAddress,
					forward.HostPort,
				)
			}
		}
	}
	if c.Metrics != nil {
		for i, forward := range c.Network.Forwards {
			if forward.Protocol != "tcp" {
				continue
			}
			if c.Metrics.Port == forward.HostPort && tcpBindAddressesOverlap("127.0.0.1", forward.HostAddress) {
				return configError("metrics port %d conflicts with network forward %d on %s", c.Metrics.Port, i, forward.HostAddress)
			}
		}
		if c.VNC != nil && c.VNC.Port == c.VNC.PortTo && c.Metrics.Port == c.VNC.Port && tcpBindAddressesOverlap("127.0.0.1", c.VNC.Bind) {
			return configError("metrics port %d conflicts with vnc listener on %s", c.Metrics.Port, c.VNC.Bind)
		}
	}
	if c.VNC == nil || c.VNC.Port != c.VNC.PortTo {
		return nil
	}
	for i, forward := range c.Network.Forwards {
		if forward.Protocol != "tcp" {
			continue
		}
		if c.VNC.Port == forward.HostPort && tcpBindAddressesOverlap(c.VNC.Bind, forward.HostAddress) {
			return configError("vnc port %d on %s conflicts with network forward %d on %s", c.VNC.Port, c.VNC.Bind, i, forward.HostAddress)
		}
	}
	return nil
}

func validKeyboardLayout(layout string) bool {
	_, ok := keyboardLayouts[layout]
	return ok
}

func validMachineType(machine string) bool {
	return machine == "" || machine == "virt" || machinePattern.MatchString(machine)
}

func validRTCBase(base string) bool {
	return base == "" || base == "utc" || base == "localtime"
}

var managerOwnedQEMUOptions = map[string]struct{}{
	"qmp": {}, "monitor": {}, "mon": {}, "chardev": {}, "serial": {}, "daemonize": {}, "pidfile": {}, "run-with": {},
	"accel": {}, "machine": {}, "M": {}, "cpu": {}, "smp": {}, "m": {}, "drive": {}, "blockdev": {},
	"device": {}, "hda": {}, "hdb": {}, "hdc": {}, "hdd": {}, "fda": {}, "fdb": {}, "cdrom": {},
	"netdev": {}, "nic": {}, "net": {}, "display": {}, "nographic": {}, "vga": {}, "nodefaults": {},
	"name": {}, "uuid": {}, "boot": {}, "bios": {}, "readconfig": {}, "writeconfig": {}, "set": {},
	"global": {}, "incoming": {}, "snapshot": {}, "S": {}, "preconfig": {}, "no-shutdown": {}, "action": {},
	"vnc": {}, "object": {}, "k": {}, "rtc": {},
}

func forbiddenQEMUArg(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' {
		return false
	}
	option := strings.TrimPrefix(arg, "--")
	if option == arg {
		option = arg[1:]
	}
	if before, _, found := strings.Cut(option, "="); found {
		option = before
	}
	if _, forbidden := managerOwnedQEMUOptions[option]; forbidden {
		return true
	}
	// QEMU documents attached values for -M and numeric -m (for example,
	// -Mnone and -m4G). Restrict this to single-dash spellings so unrelated
	// long options beginning with m are not rejected.
	if strings.HasPrefix(arg, "-M") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
		return true
	}
	return strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "--") && len(arg) > 2 && arg[2] >= '0' && arg[2] <= '9'
}

func GenerateID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("config: generate id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func GenerateUUIDv4() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("config: generate uuid: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func GenerateMAC() (string, error) {
	var value [6]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("config: generate mac: %w", err)
	}
	value[0] = value[0]&0xfe | 0x02
	return net.HardwareAddr(value[:]).String(), nil
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16 && decoded[6]>>4 == 4 && decoded[8]>>6 == 2
}

// CanonicalJSON returns the stable, indented representation used on disk, including a final newline.
func CanonicalJSON(config *Config) ([]byte, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(configDocument{Schema: encodedConfigSchemaURL, Config: *config}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return append(data, '\n'), nil
}

func CanonicalBytes(config *Config) ([]byte, error) { return CanonicalJSON(config) }

// ConfigSHA256 returns the SHA-256 of the validated running configuration
// payload. Unlike CanonicalJSON, it hashes the config without the on-disk
// $schema annotation so the durable schema URL does not affect running-config
// identity.
func ConfigSHA256(config *Config) ([sha256.Size]byte, error) {
	if err := config.Validate(); err != nil {
		return [sha256.Size]byte{}, err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("config: encode: %w", err)
	}
	return sha256.Sum256(append(data, '\n')), nil
}

func Hash(config *Config) (string, error) {
	hash, err := ConfigSHA256(config)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash[:]), nil
}

func ConfigSHA256Hex(config *Config) (string, error) { return Hash(config) }

// ValidateApply validates a replacement and preserves all immutable fields.
func ValidateApply(current, replacement *Config) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if err := replacement.Validate(); err != nil {
		return err
	}
	if replacement.SchemaVersion != current.SchemaVersion {
		return configError("schema_version is immutable")
	}
	if replacement.ID != current.ID {
		return configError("id is immutable")
	}
	if replacement.Name != current.Name {
		return configError("name is immutable")
	}
	if replacement.Backend != current.Backend {
		return configError("backend is immutable")
	}
	if replacement.Architecture != current.Architecture {
		return configError("architecture is immutable")
	}
	if replacement.Autostart.Scope != current.Autostart.Scope {
		return configError("autostart scope is immutable during config apply")
	}
	return nil
}

func configError(format string, args ...interface{}) error {
	return fmt.Errorf("config: "+format, args...)
}

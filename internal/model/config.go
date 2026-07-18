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
)

const SchemaVersion = 1

type Backend string

const (
	BackendQEMU Backend = "qemu"
	BackendVZ   Backend = "vz"
)

type RestartPolicy string

const (
	RestartNever     RestartPolicy = "never"
	RestartOnFailure RestartPolicy = "on-failure"
)

type NetworkMode string

const (
	NetworkUser        NetworkMode = "user"
	NetworkSocketVMNet NetworkMode = "socket_vmnet"
)

type AutostartScope string

const (
	AutostartNone  AutostartScope = "none"
	AutostartBoot  AutostartScope = "boot"
	AutostartLogin AutostartScope = "login"
)

type RunState string

const (
	RunStateStarting RunState = "starting"
	RunStateRunning  RunState = "running"
	RunStatePaused   RunState = "paused"
	RunStateStopping RunState = "stopping"
	RunStateStopped  RunState = "stopped"
	RunStateFailed   RunState = "failed"
)

type Config struct {
	SchemaVersion          int              `json:"schema_version"`
	ID                     string           `json:"id"`
	Name                   string           `json:"name"`
	Backend                Backend          `json:"backend"`
	Architecture           string           `json:"architecture"`
	UUID                   string           `json:"uuid"`
	CPUs                   int              `json:"cpus"`
	MemoryMiB              int              `json:"memory_mib"`
	RestartPolicy          RestartPolicy    `json:"restart_policy"`
	ShutdownTimeoutSeconds int              `json:"shutdown_timeout_seconds"`
	Firmware               FirmwareConfig   `json:"firmware"`
	Installer              *InstallerConfig `json:"installer,omitempty"`
	Disks                  []DiskConfig     `json:"disks"`
	Network                NetworkConfig    `json:"network"`
	GuestAgent             GuestAgentConfig `json:"guest_agent"`
	QEMU                   QEMUConfig       `json:"qemu"`
	Autostart              AutostartConfig  `json:"autostart"`
}

type FirmwareConfig struct {
	Code      string `json:"code"`
	Variables string `json:"variables"`
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
}

type NetworkConfig struct {
	Mode        NetworkMode        `json:"mode"`
	MAC         string             `json:"mac"`
	Forwards    []PortForward      `json:"forwards"`
	SocketVMNet *SocketVMNetConfig `json:"socket_vmnet,omitempty"`
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

type QEMUConfig struct {
	Binary    string   `json:"binary"`
	ImageTool string   `json:"image_tool"`
	Machine   string   `json:"machine"`
	ExtraArgs []string `json:"extra_args"`
}

type AutostartConfig struct {
	Scope AutostartScope `json:"scope"`
}

var (
	idPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	namePattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	serialPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	macPattern    = regexp.MustCompile(`^(?:[0-9a-f]{2}:){5}[0-9a-f]{2}$`)
)

// Decode strictly decodes exactly one non-null JSON object and validates it.
func Decode(r io.Reader) (*Config, error) {
	var raw json.RawMessage
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("config: top-level value must be an object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("config: trailing data")
		}
		return nil, fmt.Errorf("config: trailing data: %w", err)
	}

	var config Config
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&config); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
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
	if c.Backend != BackendQEMU && c.Backend != BackendVZ {
		return configError("invalid backend %q", c.Backend)
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
		return configError("invalid restart_policy %q", c.RestartPolicy)
	}
	if c.ShutdownTimeoutSeconds < 1 || c.ShutdownTimeoutSeconds > 3600 {
		return configError("shutdown_timeout_seconds must be between 1 and 3600")
	}
	if c.Firmware.Code == "" || c.Firmware.Variables == "" {
		return configError("firmware code and variables paths are required")
	}
	if c.QEMU.Machine != "" && c.QEMU.Machine != "virt" {
		return configError("qemu machine must be empty or %q", "virt")
	}
	for _, arg := range c.QEMU.ExtraArgs {
		if forbiddenQEMUArg(arg) {
			return configError("qemu extra argument %q is manager-owned", arg)
		}
	}
	if c.Autostart.Scope != AutostartNone && c.Autostart.Scope != AutostartBoot && c.Autostart.Scope != AutostartLogin {
		return configError("invalid autostart scope %q", c.Autostart.Scope)
	}
	if err := validateStorage(c); err != nil {
		return err
	}
	if err := validateNetwork(c.Network); err != nil {
		return err
	}
	return nil
}

// ValidateRuntime rejects schema-valid backends unavailable in this release.
func (c *Config) ValidateRuntime() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Backend == BackendVZ {
		return errors.New(`backend "vz" is unavailable in this build`)
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
			return configError("disk %d has invalid format %q", i, disk.Format)
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
	case NetworkSocketVMNet:
		if len(network.Forwards) != 0 {
			return configError("forwards are forbidden in socket_vmnet network mode")
		}
		if network.SocketVMNet == nil {
			return configError("socket_vmnet configuration is required")
		}
		if !filepath.IsAbs(network.SocketVMNet.ClientPath) || !filepath.IsAbs(network.SocketVMNet.SocketPath) || network.SocketVMNet.Interface == "" {
			return configError("socket_vmnet client_path and socket_path must be absolute and interface must be nonempty")
		}
	default:
		return configError("invalid network mode %q", network.Mode)
	}

	tuples := make(map[string]struct{}, len(network.Forwards))
	for i, forward := range network.Forwards {
		if forward.Protocol != "tcp" && forward.Protocol != "udp" {
			return configError("forward %d has invalid protocol %q", i, forward.Protocol)
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

var managerOwnedQEMUOptions = map[string]struct{}{
	"qmp": {}, "monitor": {}, "chardev": {}, "serial": {}, "daemonize": {}, "pidfile": {}, "run-with": {},
	"accel": {}, "machine": {}, "M": {}, "cpu": {}, "smp": {}, "m": {}, "drive": {}, "blockdev": {},
	"device": {}, "hda": {}, "hdb": {}, "hdc": {}, "hdd": {}, "fda": {}, "fdb": {}, "cdrom": {},
	"netdev": {}, "nic": {}, "net": {}, "display": {}, "nographic": {}, "vga": {}, "nodefaults": {},
	"name": {}, "uuid": {}, "boot": {}, "bios": {}, "readconfig": {}, "writeconfig": {}, "set": {},
	"global": {}, "incoming": {}, "snapshot": {}, "S": {}, "preconfig": {}, "no-shutdown": {}, "action": {},
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
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return append(data, '\n'), nil
}

func CanonicalBytes(config *Config) ([]byte, error) { return CanonicalJSON(config) }

func ConfigSHA256(config *Config) ([sha256.Size]byte, error) {
	data, err := CanonicalJSON(config)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
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

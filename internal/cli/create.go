package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"github.com/jedib0t/go-pretty/v6/progress"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/qemu"
	"github.com/bradsjm/qemu-manage/internal/store"
)

const (
	defaultMemoryMiB = 2 * 1024
	defaultDiskBytes = uint64(32) * 1024 * 1024 * 1024
	defaultVNCBind   = "127.0.0.1"
	defaultVNCPort   = 5900
	defaultVNCPortTo = 5999
)

type usbValues []model.USBDeviceConfig

func (v *usbValues) String() string { return "" }

type createDrive struct {
	Source   string
	Format   string
	Cache    string
	AIO      string
	ReadOnly bool
}

type driveValues []createDrive

func (v *driveValues) String() string { return "" }

// shareValue implements flag.Value for the single create-time --share option.
// QEMU exposes one smb folder per user netdev, so a second Set is rejected.
type shareValue struct {
	path string
	set  bool
}

func (v *shareValue) String() string {
	if v == nil {
		return ""
	}
	return v.path
}

func (v *shareValue) Set(raw string) error {
	if raw == "" {
		return errors.New("share path is empty")
	}
	if v.set {
		return errors.New("only one --share may be specified because QEMU supports one SMB folder per user network")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return fmt.Errorf("resolve share path: %w", err)
	}
	v.path = filepath.Clean(absolute)
	v.set = true
	return nil
}

func (a *App) runCreate(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	_ = stdin
	defaultFirmwareCode, defaultFirmwareVars := "", ""
	if a.DiscoverFirmware != nil {
		defaultFirmwareCode, defaultFirmwareVars = a.DiscoverFirmware()
	}

	flags := quietFlagSet("create")
	cpus := flags.Int("cpus", 2, "number of virtual CPUs")
	memory := flags.String("memory", "2GiB", "guest memory (whole MiB or GiB)")
	diskSize := flags.String("disk-size", "32GiB", "primary disk size (whole MiB or GiB)")
	image := flags.String("image", "", "source disk image")
	iso := flags.String("iso", "", "installer ISO")
	qemu := flags.String("qemu", "qemu-system-aarch64", "QEMU executable")
	qemuImg := flags.String("qemu-img", "qemu-img", "qemu-img executable")
	firmwareCode := flags.String("firmware-code", defaultFirmwareCode, "AArch64 UEFI code image (auto-detected)")
	firmwareVars := flags.String("firmware-vars", defaultFirmwareVars, "AArch64 UEFI variables template (auto-detected)")
	restartPolicy := flags.String("restart-policy", string(model.RestartNever), "restart policy")
	shutdownTimeout := flags.String("shutdown-timeout", "180s", "shutdown timeout")
	network := flags.String("network", string(model.NetworkUser), "network mode")
	guestAgent := flags.String("guest-agent", "off", "guest agent")
	socketVMNetInterface := flags.String("socket-vmnet-interface", "", "socket_vmnet interface")
	rtcBase := flags.String("rtc-base", defaultRTCBase, "QEMU RTC base")
	vncEnabled := flags.Bool("vnc", false, "enable QEMU VNC")
	vncPassword := flags.String("vnc-password", "", "QEMU VNC password")
	vncBind := flags.String("vnc-bind", defaultVNCBind, "VNC bind IPv4 address")
	vncPort := flags.String("vnc-port", strconv.Itoa(defaultVNCPort), "minimum VNC TCP port")
	vncPortTo := flags.String("vnc-port-to", strconv.Itoa(defaultVNCPortTo), "maximum VNC TCP port")
	keyboardLayout := flags.String("keyboard-layout", "", "QEMU VNC keyboard layout")
	var forwards forwardValues
	flags.Var(&forwards, "forward", "proto:IPv4:host-port:guest-port (repeatable)")
	var usbs usbValues
	flags.Var(&usbs, "usb", "USB passthrough selector")
	var drives driveValues
	flags.Var(&drives, "drive", "additional virtio drive")
	var share shareValue
	flags.Var(&share, "share", "host folder exported over SMB (user network only)")
	if err := flags.Parse(args); err != nil {
		return usageErrorf("create: %v", err)
	}
	firmwareCodeExplicit, firmwareVarsExplicit := false, false
	vncDetailExplicit := false
	keyboardLayoutExplicit := false
	flags.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "firmware-code":
			firmwareCodeExplicit = true
		case "firmware-vars":
			firmwareVarsExplicit = true
		case "vnc-password", "vnc-bind", "vnc-port", "vnc-port-to":
			vncDetailExplicit = true
		case "keyboard-layout":
			vncDetailExplicit = true
			keyboardLayoutExplicit = true
		}
	})
	if firmwareCodeExplicit != firmwareVarsExplicit {
		return usageErrorf("create: --firmware-code and --firmware-vars must be provided together")
	}
	if flags.NArg() != 0 {
		return usageErrorf("create: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *restartPolicy != string(model.RestartNever) && *restartPolicy != string(model.RestartOnFailure) {
		return usageErrorf(
			"create: --restart-policy %q is invalid; valid values: never, on-failure",
			*restartPolicy,
		)
	}

	memoryMiB, err := parseMiB(*memory)
	if err != nil {
		return usageErrorf("create: --memory: %v", err)
	}
	diskBytes, err := parseSizeBytes(*diskSize)
	if err != nil {
		return usageErrorf("create: --disk-size: %v", err)
	}
	timeoutSeconds, err := parseCreateWholeSeconds(*shutdownTimeout)
	if err != nil {
		return usageErrorf("create: --shutdown-timeout: %v", err)
	}
	networkMode, err := parseNetworkMode(*network)
	if err != nil {
		return usageErrorf("create: --network: %v", err)
	}
	guestAgentEnabled, err := parseOnOff(*guestAgent)
	if err != nil {
		return usageErrorf("create: --guest-agent: %v", err)
	}
	rtcBaseValue, err := parseRTCBase(*rtcBase)
	if err != nil {
		return usageErrorf("create: --rtc-base: %v", err)
	}
	if !*vncEnabled && vncDetailExplicit {
		return usageErrorf("create: --vnc-password, --vnc-bind, --vnc-port, --vnc-port-to, and --keyboard-layout require --vnc")
	}
	if *vncEnabled && *vncPassword == "" {
		return usageErrorf("create: --vnc-password is required when --vnc is set")
	}
	if *firmwareCode == "" || *firmwareVars == "" {
		return usageErrorf(
			"create: --firmware-code and --firmware-vars are required when they cannot be auto-detected; install QEMU with `brew install qemu` or provide both paths",
		)
	}
	if networkMode == model.NetworkUser && *socketVMNetInterface != "" {
		return usageErrorf("create: --socket-vmnet-interface requires --network socket_vmnet")
	}
	if networkMode == model.NetworkSocketVMNet && len(forwards) != 0 {
		return usageErrorf("create: --forward is incompatible with socket_vmnet")
	}
	if networkMode == model.NetworkSocketVMNet && share.set {
		return usageErrorf("create: --share is incompatible with socket_vmnet")
	}
	imageSource, err := parseImageSource(*image)
	if err != nil {
		return usageErrorf("create: --image: %v", err)
	}
	var vnc *model.VNCConfig
	if *vncEnabled {
		vncPortValue, err := parsePort(*vncPort)
		if err != nil {
			return usageErrorf("create: --vnc-port: %v", err)
		}
		vncPortToValue, err := parsePort(*vncPortTo)
		if err != nil {
			return usageErrorf("create: --vnc-port-to: %v", err)
		}
		selectedKeyboardLayout := defaultKeyboardLayout
		if keyboardLayoutExplicit {
			selectedKeyboardLayout, err = parseKeyboardLayout(*keyboardLayout)
			if err != nil {
				return usageErrorf("create: --keyboard-layout: %v", err)
			}
		}
		vnc = &model.VNCConfig{
			Bind:           *vncBind,
			Port:           vncPortValue,
			PortTo:         vncPortToValue,
			Password:       *vncPassword,
			KeyboardLayout: selectedKeyboardLayout,
		}
	}
	if len(usbs) > usbDeviceLimit(vnc) {
		return usageErrorf("create: --usb supports at most %d devices with current VNC settings", usbDeviceLimit(vnc))
	}
	for i := range drives {
		var err error
		if drives[i].Format == "" {
			drives[i].Format, err = detectDriveFormat(drives[i].Source)
		} else {
			err = requireRegularSource(drives[i].Source)
		}
		if err != nil {
			return fmt.Errorf("create: --drive file %q: %w", drives[i].Source, err)
		}
	}
	if share.set {
		if err := requireSharedDirectory(share.path); err != nil {
			return fmt.Errorf("create: --share %q: %w", share.path, err)
		}
		if a.RequireSMBD != nil {
			if err := a.RequireSMBD(); err != nil {
				return fmt.Errorf("create: --share: %w", err)
			}
		}
	}
	qemuPath, err := resolveExecutable(*qemu)
	if err != nil {
		return fmt.Errorf("qemu: %w", err)
	}
	qemuImgPath, err := resolveExecutable(*qemuImg)
	if err != nil {
		return fmt.Errorf("qemu: %w", err)
	}
	machine := "virt"
	if a.DiscoverMachine != nil {
		machine, err = a.DiscoverMachine(ctx, qemuPath)
		if err != nil {
			return err
		}
	}
	id, err := model.GenerateID()
	if err != nil {
		return err
	}
	uuid, err := model.GenerateUUIDv4()
	if err != nil {
		return err
	}
	mac, err := model.GenerateMAC()
	if err != nil {
		return err
	}

	networkConfig := model.NetworkConfig{Mode: networkMode, MAC: mac, Forwards: []model.PortForward{}}
	switch networkMode {
	case model.NetworkUser:
		networkConfig.Forwards = append(networkConfig.Forwards, forwards...)
		if share.set {
			networkConfig.SMBFolder = share.path
		}
	case model.NetworkSocketVMNet:
		interfaceName := *socketVMNetInterface
		if interfaceName == "" {
			interfaceName = defaultSocketVMNetInterface
		}
		socketVMNetConfig, err := a.resolveSocketVMNet(interfaceName)
		if err != nil {
			return err
		}
		if interfaceName != defaultSocketVMNetInterface {
			if a.ProvisionSocketVMNetBridge == nil {
				return errors.New("socket_vmnet: bridge provisioner is unavailable")
			}
			socketVMNetConfig, err = a.ProvisionSocketVMNetBridge(ctx, socketVMNetConfig.ClientPath, interfaceName)
			if err != nil {
				return err
			}
		}
		networkConfig.SocketVMNet = socketVMNetConfig
	}

	primaryBootIndex := 0
	var installer *model.InstallerConfig
	if *iso != "" {
		primaryBootIndex = 1
		installer = &model.InstallerConfig{Path: "installer.iso", BootIndex: 0}
	}
	disks := []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk-" + id[:16], BootIndex: primaryBootIndex}}
	for i, drive := range drives {
		disks = append(disks, model.DiskConfig{
			Path:      drive.Source,
			Format:    drive.Format,
			Serial:    fmt.Sprintf("disk-%s-%d", id[:12], i+1),
			BootIndex: primaryBootIndex + i + 1,
			ReadOnly:  drive.ReadOnly,
			Cache:     drive.Cache,
			AIO:       drive.AIO,
		})
	}
	config := &model.Config{
		SchemaVersion:          model.SchemaVersion,
		ID:                     id,
		Name:                   name,
		Backend:                model.BackendQEMU,
		Architecture:           "aarch64",
		UUID:                   uuid,
		CPUs:                   *cpus,
		MemoryMiB:              memoryMiB,
		RestartPolicy:          model.RestartPolicy(*restartPolicy),
		ShutdownTimeoutSeconds: timeoutSeconds,
		Firmware:               model.FirmwareConfig{Code: "firmware-code.fd", Variables: "firmware-vars.fd"},
		Installer:              installer,
		Disks:                  disks,
		Network:                networkConfig,
		GuestAgent:             model.GuestAgentConfig{Enabled: guestAgentEnabled},
		VNC:                    vnc,
		USB:                    []model.USBDeviceConfig(usbs),
		QEMU:                   model.QEMUConfig{Binary: qemuPath, ImageTool: qemuImgPath, Machine: machine, RTCBase: rtcBaseValue, ExtraArgs: []string{}},
		Autostart:              model.AutostartConfig{Scope: model.AutostartNone},
	}
	if err := config.Validate(); err != nil {
		return err
	}

	if err := a.Store.Create(config, func(_ *model.Config, paths store.Paths) error {
		if err := copyRegularFile(*firmwareCode, filepath.Join(paths.VMDir, config.Firmware.Code), 0o400, stderr, true, a.progressInteractive(stderr), "Copying firmware code"); err != nil {
			return fmt.Errorf("copy firmware code: %w", err)
		}
		if err := copyRegularFile(*firmwareVars, filepath.Join(paths.VMDir, config.Firmware.Variables), 0o600, stderr, true, a.progressInteractive(stderr), "Copying firmware variables"); err != nil {
			return fmt.Errorf("copy firmware variables: %w", err)
		}
		if installer != nil {
			if err := copyRegularFile(*iso, filepath.Join(paths.VMDir, installer.Path), 0o400, stderr, true, a.progressInteractive(stderr), "Copying installer ISO"); err != nil {
				return fmt.Errorf("copy installer: %w", err)
			}
		}
		diskPath := filepath.Join(paths.VMDir, config.Disks[0].Path)
		if *image == "" {
			if err := withWaitingProgress(stderr, true, a.progressInteractive(stderr), "Creating primary disk", func() error {
				return a.runExternal(ctx, qemuImgPath, []string{"create", "-f", "qcow2", diskPath, strconv.FormatUint(diskBytes, 10)})
			}); err != nil {
				return err
			}
			if err := os.Chmod(diskPath, 0o600); err != nil {
				return fmt.Errorf("set disk mode: %w", err)
			}
			return nil
		}
		sourcePath, temporarySource, err := a.materializeImage(ctx, imageSource, paths.VMDir, stderr, true, a.progressInteractive(stderr))
		if err != nil {
			return err
		}
		convertErr := withWaitingProgress(stderr, true, a.progressInteractive(stderr), "Converting image", func() error {
			return a.runExternal(ctx, qemuImgPath, []string{"convert", "-O", "qcow2", sourcePath, diskPath})
		})
		if temporarySource {
			removeErr := os.Remove(sourcePath)
			if convertErr != nil {
				return errors.Join(convertErr, removeErr)
			}
			if removeErr != nil {
				return fmt.Errorf("remove downloaded image: %w", removeErr)
			}
		}
		if convertErr != nil {
			return convertErr
		}
		if err := os.Chmod(diskPath, 0o600); err != nil {
			return fmt.Errorf("set disk mode: %w", err)
		}
		virtualSize, err := qcow2VirtualSize(diskPath)
		if err != nil {
			return fmt.Errorf("query converted image virtual size: %w", err)
		}
		if virtualSize < diskBytes {
			if err := withWaitingProgress(stderr, true, a.progressInteractive(stderr), "Resizing image", func() error {
				return a.runExternal(ctx, qemuImgPath, []string{"resize", diskPath, strconv.FormatUint(diskBytes, 10)})
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if config.Network.SMBFolder != "" {
		if err := writeSMBMountHelp(stdout, config.Network.SMBFolder); err != nil {
			return fmt.Errorf("write smb guidance: %w", err)
		}
	}
	return nil
}

func (v *usbValues) Set(raw string) error {
	if raw == "" {
		return errors.New("specification is empty")
	}
	parts := strings.Split(raw, ",")
	selector := model.USBDeviceConfig{}
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if part == "" {
			return errors.New("contains an empty item")
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok || key == "" || value == "" {
			return fmt.Errorf("item %q must have the form key=value", part)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = struct{}{}
		switch key {
		case "vendor":
			normalized, err := parseUSBHexValue(value)
			if err != nil {
				return fmt.Errorf("vendor: %w", err)
			}
			selector.VendorID = normalized
		case "product":
			normalized, err := parseUSBHexValue(value)
			if err != nil {
				return fmt.Errorf("product: %w", err)
			}
			selector.ProductID = normalized
		case "bus":
			bus, err := parseUSBDecimalValue(value, 255)
			if err != nil {
				return fmt.Errorf("bus: %w", err)
			}
			selector.HostBus = bus
		case "address":
			address, err := parseUSBDecimalValue(value, 127)
			if err != nil {
				return fmt.Errorf("address: %w", err)
			}
			selector.HostAddress = address
		default:
			return fmt.Errorf("unknown key %q", key)
		}
	}
	switch {
	case selector.VendorID != "" || selector.ProductID != "":
		if selector.VendorID == "" || selector.ProductID == "" {
			return errors.New("vendor and product must be provided together")
		}
		if selector.HostBus != 0 || selector.HostAddress != 0 {
			return errors.New("vendor/product cannot be mixed with bus/address")
		}
	case selector.HostBus != 0 || selector.HostAddress != 0:
		if selector.HostBus == 0 || selector.HostAddress == 0 {
			return errors.New("bus and address must be provided together")
		}
	default:
		return errors.New("selector is required")
	}
	*v = append(*v, selector)
	return nil
}

func (v *driveValues) Set(raw string) error {
	items, err := splitDriveItems(raw)
	if err != nil {
		return err
	}
	drive := createDrive{}
	keys := make(map[string]struct{}, len(items))
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || value == "" {
			return fmt.Errorf("item %q must have the form key=value", item)
		}
		if _, exists := keys[key]; exists {
			return fmt.Errorf("duplicate key %q", key)
		}
		keys[key] = struct{}{}
		switch key {
		case "file":
			source, err := filepath.Abs(value)
			if err != nil {
				return fmt.Errorf("resolve file %q: %w", value, err)
			}
			drive.Source = filepath.Clean(source)
		case "if":
			if value != "virtio" {
				return fmt.Errorf("if %q is invalid; valid values: virtio", value)
			}
		case "format":
			if value != "raw" && value != "qcow2" {
				return fmt.Errorf("format %q is invalid; valid values: raw, qcow2", value)
			}
			drive.Format = value
		case "cache":
			if !validDriveCache(value) {
				return fmt.Errorf(
					"cache %q is invalid; valid values: none, writeback, writethrough, directsync, unsafe",
					value,
				)
			}
			drive.Cache = value
		case "aio":
			if value != "threads" && value != "native" {
				return fmt.Errorf("aio %q is invalid; valid values: threads, native", value)
			}
			drive.AIO = value
		case "readonly":
			switch value {
			case "on":
				drive.ReadOnly = true
			case "off":
				drive.ReadOnly = false
			default:
				return fmt.Errorf("readonly %q is invalid; valid values: on, off", value)
			}
		default:
			return fmt.Errorf("unknown key %q", key)
		}
	}
	if drive.Source == "" {
		return errors.New("file is required")
	}
	*v = append(*v, drive)
	return nil
}

func usbDeviceLimit(vnc *model.VNCConfig) int {
	if vnc != nil {
		return 2
	}
	return 4
}

func parseUSBHexValue(raw string) (string, error) {
	if len(raw) != 4 {
		return "", errors.New("must be exactly four hexadecimal digits")
	}
	value, err := strconv.ParseUint(raw, 16, 16)
	if err != nil {
		return "", errors.New("must be exactly four hexadecimal digits")
	}
	if value == 0 {
		return "", errors.New("must be between 0001 and ffff")
	}
	return fmt.Sprintf("%04x", value), nil
}

func parseUSBDecimalValue(raw string, maximum int) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("must be between 1 and %d", maximum)
	}
	if value < 1 || value > maximum {
		return 0, fmt.Errorf("must be between 1 and %d", maximum)
	}
	return value, nil
}

func splitDriveItems(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("specification is empty")
	}
	items := make([]string, 0, 6)
	var current strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != ',' {
			current.WriteByte(raw[i])
			continue
		}
		if i+1 < len(raw) && raw[i+1] == ',' {
			current.WriteByte(',')
			i++
			continue
		}
		if current.Len() == 0 {
			return nil, errors.New("contains an empty item")
		}
		items = append(items, current.String())
		current.Reset()
	}
	if current.Len() == 0 {
		return nil, errors.New("contains an empty item")
	}
	items = append(items, current.String())
	return items, nil
}

func validDriveCache(value string) bool {
	switch value {
	case "none", "writeback", "writethrough", "directsync", "unsafe":
		return true
	default:
		return false
	}
}

func detectDriveFormat(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	var header [4]byte
	if _, err := io.ReadFull(file, header[:]); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if string(header[:]) == "QFI\xfb" {
		return "qcow2", nil
	}
	return "raw", nil
}

func parseMiB(value string) (int, error) {
	bytes, err := parseSizeBytes(value)
	if err != nil {
		return 0, err
	}
	mib := bytes / (1024 * 1024)
	if mib > uint64(math.MaxInt) {
		return 0, errors.New("value overflows int")
	}
	return int(mib), nil
}

func parseSizeBytes(value string) (uint64, error) {
	var multiplier uint64
	switch {
	case strings.HasSuffix(value, "MiB"):
		multiplier = 1024 * 1024
		value = strings.TrimSuffix(value, "MiB")
	case strings.HasSuffix(value, "GiB"):
		multiplier = 1024 * 1024 * 1024
		value = strings.TrimSuffix(value, "GiB")
	default:
		return 0, errors.New("must be a whole-number MiB or GiB value")
	}
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, errors.New("must be a whole-number MiB or GiB value")
	}
	amount, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("must be a whole-number MiB or GiB value")
	}
	if amount == 0 {
		return 0, errors.New("must be greater than zero")
	}
	if amount > math.MaxUint64/multiplier {
		return 0, errors.New("value overflows bytes")
	}
	return amount * multiplier, nil
}

func parseCreateWholeSeconds(value string) (int, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration%time.Second != 0 {
		return 0, errors.New("must be a positive whole-second duration")
	}
	seconds := duration / time.Second
	if seconds > time.Duration(math.MaxInt) {
		return 0, errors.New("duration overflows int")
	}
	return int(seconds), nil
}

func resolveExecutable(value string) (string, error) {
	if value == "" {
		return "", errors.New("executable path is empty")
	}
	if filepath.IsAbs(value) {
		info, err := os.Stat(value)
		if err != nil {
			return "", fmt.Errorf("resolve executable %q: %w", value, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%q is not an executable regular file", value)
		}
		return filepath.Clean(value), nil
	}
	if strings.ContainsRune(value, filepath.Separator) {
		return "", fmt.Errorf("executable %q must be absolute or a PATH name", value)
	}
	path, err := exec.LookPath(value)
	if err != nil {
		return "", fmt.Errorf("resolve executable %q: %w", value, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make executable %q absolute: %w", value, err)
	}
	return path, nil
}

func requireRegularSource(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func copyRegularFile(source, destination string, mode os.FileMode, progressOutput io.Writer, progressEnabled, interactive bool, message string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		output.Close()
		if !committed {
			os.Remove(destination)
		}
	}()
	if err := withProgress(progressOutput, progressEnabled, interactive, message, info.Size(), progress.UnitsBytes, func(tracker *progress.Tracker) error {
		return copyWithProgress(input, output, info.Size(), tracker)
	}); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Chmod(destination, mode); err != nil {
		return err
	}
	committed = true
	return nil
}
func requireSharedDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat share path: %w", err)
	}
	if !info.IsDir() {
		return errors.New("share path is not a directory")
	}
	return nil
}

// requireSMBDDefault is the default QEMU smbd helper check used when App does
// not inject its own. It returns nil when QEMU's user-network SMB server will
// be able to launch smbd; otherwise an actionable error pointing at the
// Homebrew samba formula.
func requireSMBDDefault() error {
	if path := qemu.DiscoverSMBD(); path != "" {
		return nil
	}
	return errors.New("smbd not found; install with `brew install samba` (provides /opt/homebrew/sbin/samba-dot-org-smbd on Apple Silicon, which QEMU's user-network SMB server invokes)")
}

func qcow2VirtualSize(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var header [32]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return 0, err
	}
	if string(header[:4]) != "QFI\xfb" {
		return 0, errors.New("converted image is not qcow2")
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if version != 2 && version != 3 {
		return 0, fmt.Errorf("unsupported qcow2 version %d", version)
	}
	return binary.BigEndian.Uint64(header[24:32]), nil
}

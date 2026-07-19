package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qemu-manage/internal/model"
	"qemu-manage/internal/store"
)

const (
	defaultMemoryMiB = 2 * 1024
	defaultDiskBytes = uint64(32) * 1024 * 1024 * 1024
	defaultVNCBind   = "127.0.0.1"
	defaultVNCPort   = 5900
	defaultVNCPortTo = 5999
)

func (a *App) runCreate(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	_ = stdin
	_ = stdout
	_ = stderr
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
	vncEnabled := flags.Bool("vnc", false, "enable QEMU VNC")
	vncPassword := flags.String("vnc-password", "", "QEMU VNC password")
	vncBind := flags.String("vnc-bind", defaultVNCBind, "VNC bind IPv4 address")
	vncPort := flags.String("vnc-port", strconv.Itoa(defaultVNCPort), "minimum VNC TCP port")
	vncPortTo := flags.String("vnc-port-to", strconv.Itoa(defaultVNCPortTo), "maximum VNC TCP port")
	if err := flags.Parse(args); err != nil {
		return usageErrorf("create: %v", err)
	}
	firmwareCodeExplicit, firmwareVarsExplicit := false, false
	vncDetailExplicit := false
	flags.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "firmware-code":
			firmwareCodeExplicit = true
		case "firmware-vars":
			firmwareVarsExplicit = true
		case "vnc-password", "vnc-bind", "vnc-port", "vnc-port-to":
			vncDetailExplicit = true
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
	if !*vncEnabled && vncDetailExplicit {
		return usageErrorf("create: --vnc-password, --vnc-bind, --vnc-port, and --vnc-port-to require --vnc")
	}
	if *vncEnabled && *vncPassword == "" {
		return usageErrorf("create: --vnc-password is required when --vnc is set")
	}
	if *firmwareCode == "" || *firmwareVars == "" {
		return usageErrorf(
			"create: --firmware-code and --firmware-vars are required when they cannot be auto-detected; install QEMU with `brew install qemu` or provide both paths",
		)
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
		vnc = &model.VNCConfig{
			Bind:     *vncBind,
			Port:     vncPortValue,
			PortTo:   vncPortToValue,
			Password: *vncPassword,
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

	bootIndex := 0
	var installer *model.InstallerConfig
	if *iso != "" {
		bootIndex = 1
		installer = &model.InstallerConfig{Path: "installer.iso", BootIndex: 0}
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
		Disks:                  []model.DiskConfig{{Path: "disk.qcow2", Format: "qcow2", Serial: "disk-" + id[:16], BootIndex: bootIndex}},
		Network:                model.NetworkConfig{Mode: model.NetworkUser, MAC: mac, Forwards: []model.PortForward{}},
		GuestAgent:             model.GuestAgentConfig{},
		VNC:                    vnc,
		QEMU:                   model.QEMUConfig{Binary: qemuPath, ImageTool: qemuImgPath, Machine: "virt", ExtraArgs: []string{}},
		Autostart:              model.AutostartConfig{Scope: model.AutostartNone},
	}
	if err := config.Validate(); err != nil {
		return err
	}

	return a.Store.Create(config, func(_ *model.Config, paths store.Paths) error {
		if err := copyRegularFile(*firmwareCode, filepath.Join(paths.VMDir, config.Firmware.Code), 0o400); err != nil {
			return fmt.Errorf("copy firmware code: %w", err)
		}
		if err := copyRegularFile(*firmwareVars, filepath.Join(paths.VMDir, config.Firmware.Variables), 0o600); err != nil {
			return fmt.Errorf("copy firmware variables: %w", err)
		}
		if installer != nil {
			if err := copyRegularFile(*iso, filepath.Join(paths.VMDir, installer.Path), 0o400); err != nil {
				return fmt.Errorf("copy installer: %w", err)
			}
		}
		diskPath := filepath.Join(paths.VMDir, config.Disks[0].Path)
		if *image == "" {
			if err := a.RunExternal(ctx, qemuImgPath, []string{"create", "-f", "qcow2", diskPath, strconv.FormatUint(diskBytes, 10)}); err != nil {
				return err
			}
			if err := os.Chmod(diskPath, 0o600); err != nil {
				return fmt.Errorf("set disk mode: %w", err)
			}
			return nil
		}
		sourcePath, temporarySource, err := a.materializeImage(ctx, imageSource, paths.VMDir, stderr)
		if err != nil {
			return err
		}
		convertErr := a.RunExternal(ctx, qemuImgPath, []string{"convert", "-O", "qcow2", sourcePath, diskPath})
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
			if err := a.RunExternal(ctx, qemuImgPath, []string{"resize", diskPath, strconv.FormatUint(diskBytes, 10)}); err != nil {
				return err
			}
		}
		return nil
	})
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

func copyRegularFile(source, destination string, mode os.FileMode) error {
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
	if _, err := io.Copy(output, input); err != nil {
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

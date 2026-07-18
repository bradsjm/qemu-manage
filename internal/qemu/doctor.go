package qemu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"qemu-manage/internal/backend"
	"qemu-manage/internal/model"
)

// CheckStatus is the result of one doctor check.
type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"

	probeTimeout = 5 * time.Second
)

// Check is one deterministic, machine-readable doctor result.
type Check struct {
	Name     string      `json:"name"`
	Status   CheckStatus `json:"status"`
	Evidence string      `json:"evidence"`
}

var qemuVersionPattern = regexp.MustCompile(`(?i)QEMU emulator version\s+([0-9]+)\.([0-9]+)\.([0-9]+)(?:\s|$)`)

var (
	firmwareCodeCandidates = []string{
		"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
		"/opt/homebrew/opt/qemu/share/qemu/edk2-aarch64-code.fd",
		"/usr/local/share/qemu/edk2-aarch64-code.fd",
		"/usr/local/opt/qemu/share/qemu/edk2-aarch64-code.fd",
	}
	firmwareVarsCandidates = []string{
		"/opt/homebrew/share/qemu/edk2-arm-vars.fd",
		"/opt/homebrew/share/qemu/edk2-aarch64-vars.fd",
		"/opt/homebrew/opt/qemu/share/qemu/edk2-arm-vars.fd",
		"/opt/homebrew/opt/qemu/share/qemu/edk2-aarch64-vars.fd",
		"/usr/local/share/qemu/edk2-arm-vars.fd",
		"/usr/local/share/qemu/edk2-aarch64-vars.fd",
		"/usr/local/opt/qemu/share/qemu/edk2-arm-vars.fd",
		"/usr/local/opt/qemu/share/qemu/edk2-aarch64-vars.fd",
	}
)

const (
	qemuInstallInstruction          = "install with: `brew install qemu`"
	socketVMNetInstallInstruction   = "install with: `brew install socket_vmnet`; start the shared service with: `sudo \"$(brew --prefix)/bin/brew\" services start socket_vmnet`"
	socketVMNetRootOwnedInstruction = "create a root-owned client copy with: `sudo install -d -o root -g wheel -m 0755 /opt/socket_vmnet/bin && sudo install -o root -g wheel -m 0755 $(brew --prefix socket_vmnet)/bin/socket_vmnet_client /opt/socket_vmnet/bin/socket_vmnet_client` (repeat the second command after Homebrew upgrades)"
)

// Doctor checks the configured QEMU installation and VM artifacts. Relative
// configured paths belong to paths.VMDir. When VMDir is empty, executable names
// are resolved through PATH instead.
func Doctor(ctx context.Context, cfg model.Config, paths backend.RuntimePaths) []Check {
	qemuPath, qemuPathErr := resolveExecutable(paths.VMDir, cfg.QEMU.Binary, "qemu-system-aarch64")
	checks := make([]Check, 0, 10)
	checks = append(checks, executableCheck("qemu_binary", qemuPath, qemuPathErr))

	if qemuPathErr != nil {
		checks = append(checks,
			Check{Name: "qemu_version", Status: CheckFail, Evidence: "qemu binary unavailable; version probe not run"},
			Check{Name: "hvf", Status: CheckFail, Evidence: "qemu binary unavailable; -accel help probe not run"},
			Check{Name: "run_with_parent", Status: CheckFail, Evidence: "qemu binary unavailable; -run-with help probe not run"},
		)
	} else {
		checks = append(checks, checkVersion(ctx, qemuPath))
		checks = append(checks, capabilityCheck(ctx, qemuPath, "hvf", []string{"-accel", "help"}, "hvf"))
		checks = append(checks, capabilityCheck(ctx, qemuPath, "run_with_parent", []string{"-run-with", "help"}, "exit-with-parent"))
	}

	imageTool, imageToolErr := resolveExecutable(paths.VMDir, cfg.QEMU.ImageTool, "qemu-img")
	imageToolCheck := executableCheck("qemu_img", imageTool, imageToolErr)
	if paths.VMDir != "" && imageToolCheck.Status == CheckFail {
		imageToolCheck.Status = CheckWarn
	}
	checks = append(checks, imageToolCheck)
	if paths.VMDir == "" && cfg.Firmware.Code == "" && cfg.Firmware.Variables == "" {
		firmwareCodeCheck, firmwareVarsCheck := discoveredFirmwareChecks(firmwareInstallations)
		checks = append(checks, firmwareCodeCheck, firmwareVarsCheck)
	} else {
		checks = append(checks,
			artifactCheck("firmware_code", configuredPath(paths.VMDir, cfg.Firmware.Code), cfg.Firmware.Code, false),
			artifactCheck("firmware_vars", configuredPath(paths.VMDir, cfg.Firmware.Variables), cfg.Firmware.Variables, false),
		)
	}
	for index, disk := range cfg.Disks {
		checks = append(checks, artifactCheck(
			fmt.Sprintf("disk_%d", index),
			configuredPath(paths.VMDir, disk.Path),
			disk.Path,
			false,
		))
	}
	if cfg.Installer != nil {
		checks = append(checks, artifactCheck(
			"installer",
			configuredPath(paths.VMDir, cfg.Installer.Path),
			cfg.Installer.Path,
			false,
		))
	}

	if cfg.Network.Mode == model.NetworkSocketVMNet {
		if cfg.Network.SocketVMNet == nil {
			checks = append(checks,
				Check{Name: "socket_vmnet_client", Status: CheckFail, Evidence: "socket_vmnet configuration is missing; " + socketVMNetInstallInstruction},
				Check{Name: "socket_vmnet_socket", Status: CheckFail, Evidence: "socket_vmnet configuration is missing; " + socketVMNetInstallInstruction},
			)
		} else {
			client := configuredPath(paths.VMDir, cfg.Network.SocketVMNet.ClientPath)
			checks = append(checks, socketVMNetClientCheck(client, cfg.Network.SocketVMNet.ClientPath))
			socket := configuredPath(paths.VMDir, cfg.Network.SocketVMNet.SocketPath)
			checks = append(checks, socketVMNetSocketCheck(ctx, socket, cfg.Network.SocketVMNet.SocketPath))
		}
	}
	return checks
}

// RequiredPassed rejects the first failed check. Warnings are advisory.
func RequiredPassed(checks []Check) error {
	for _, check := range checks {
		if check.Status == CheckFail {
			return fmt.Errorf("qemu: doctor check %s failed: %s", check.Name, check.Evidence)
		}
	}
	return nil
}

func resolveExecutable(vmDir, configured, fallback string) (string, error) {
	candidate := configured
	if candidate == "" {
		candidate = fallback
	}
	if vmDir != "" {
		candidate = backend.ResolvePath(vmDir, candidate)
		if !filepath.IsAbs(candidate) {
			return candidate, fmt.Errorf("path is not absolute: %s", candidate)
		}
		return candidate, executableError(candidate)
	}
	if filepath.IsAbs(candidate) {
		return filepath.Clean(candidate), executableError(filepath.Clean(candidate))
	}
	resolved, err := exec.LookPath(candidate)
	if err != nil {
		return candidate, fmt.Errorf("%s was not found in PATH", candidate)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return resolved, fmt.Errorf("resolve %s: %w", candidate, err)
	}
	return resolved, executableError(resolved)
}

func executableError(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Mode().Perm()&0111 == 0 {
		return errors.New("not executable")
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return fmt.Errorf("not executable for current credentials: %w", err)
	}
	return nil
}

func executableCheck(name, path string, err error) Check {
	if err != nil {
		evidence := fmt.Sprintf("%s: %v", path, err)
		if name == "qemu_binary" || name == "qemu_img" {
			evidence += "; " + qemuInstallInstruction
		}
		return Check{Name: name, Status: CheckFail, Evidence: evidence}
	}
	return Check{Name: name, Status: CheckPass, Evidence: path}
}

func checkVersion(ctx context.Context, binary string) Check {
	output, err := probe(ctx, binary, "--version")
	if err != nil {
		return Check{Name: "qemu_version", Status: CheckFail, Evidence: err.Error()}
	}
	match := qemuVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return Check{Name: "qemu_version", Status: CheckFail, Evidence: "unrecognized version output: " + output}
	}
	version := strings.Join(match[1:4], ".")
	if version == "11.0.0" {
		return Check{Name: "qemu_version", Status: CheckFail, Evidence: "QEMU 11.0.0 has a known macOS AArch64 HVF regression; upgrade with: `brew upgrade qemu` (11.0.1 fixes this regression)"}
	}
	return Check{Name: "qemu_version", Status: CheckPass, Evidence: "QEMU " + version}
}

type probeTimeoutError struct {
	message string
}

func (err probeTimeoutError) Error() string {
	return err.message
}

func capabilityCheck(ctx context.Context, binary, name string, args []string, capability string) Check {
	output, err := probe(ctx, binary, args...)
	var timeoutErr probeTimeoutError
	if !errors.As(err, &timeoutErr) {
		for _, field := range strings.Fields(output) {
			field = strings.Trim(field, " ,:()[]")
			if field == capability || strings.HasPrefix(field, capability+"=") {
				return Check{Name: name, Status: CheckPass, Evidence: capability + " is supported"}
			}
		}
	}
	if err != nil {
		return Check{Name: name, Status: CheckFail, Evidence: err.Error()}
	}
	return Check{Name: name, Status: CheckFail, Evidence: capability + " is absent from " + strings.Join(args, " ")}
}

func probe(parent context.Context, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	text := strings.Join(strings.Fields(string(output)), " ")
	if ctx.Err() != nil {
		return text, probeTimeoutError{
			message: fmt.Sprintf("%s %s timed out after %s", binary, strings.Join(args, " "), probeTimeout),
		}
	}
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s failed: %v: %s", binary, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s failed: %v", binary, strings.Join(args, " "), err)
	}
	return text, nil
}

func configuredPath(vmDir, configured string) string {
	if configured == "" {
		return ""
	}
	if vmDir == "" {
		return filepath.Clean(configured)
	}
	return backend.ResolvePath(vmDir, configured)
}

func discoveredFirmwareChecks(installations []firmwareInstallation) (Check, Check) {
	code, variables := discoverFirmware(installations)
	checked := make([]string, 0, len(installations))
	for _, installation := range installations {
		checked = append(checked, installation.codePath+" + "+strings.Join(installation.variablesPath, "|"))
	}
	evidence := "checked coherent pairs: " + strings.Join(checked, ", ")
	if code == "" || variables == "" {
		failure := "no readable firmware code and variables pair found in one QEMU installation; " + evidence + "; " + qemuInstallInstruction
		return Check{Name: "firmware_code", Status: CheckFail, Evidence: failure},
			Check{Name: "firmware_vars", Status: CheckFail, Evidence: failure}
	}
	return Check{Name: "firmware_code", Status: CheckPass, Evidence: code + "; paired with " + variables},
		Check{Name: "firmware_vars", Status: CheckPass, Evidence: variables + "; paired with " + code}
}

func artifactCheck(name, path, configured string, executable bool) Check {
	if configured == "" {
		return Check{Name: name, Status: CheckFail, Evidence: "path is not configured"}
	}
	info, err := os.Stat(path)
	if err != nil {
		return Check{Name: name, Status: CheckFail, Evidence: fmt.Sprintf("%s: %v", path, err)}
	}
	if !info.Mode().IsRegular() {
		return Check{Name: name, Status: CheckFail, Evidence: path + ": not a regular file"}
	}
	if !executable {
		if err := unix.Access(path, unix.R_OK); err != nil {
			return Check{Name: name, Status: CheckFail, Evidence: fmt.Sprintf("%s: not readable for current credentials: %v", path, err)}
		}
	}
	if executable {
		if info.Mode().Perm()&0111 == 0 {
			return Check{Name: name, Status: CheckFail, Evidence: path + ": not executable"}
		}
		if err := unix.Access(path, unix.X_OK); err != nil {
			return Check{Name: name, Status: CheckFail, Evidence: fmt.Sprintf("%s: not executable for current credentials: %v", path, err)}
		}
	}
	return Check{Name: name, Status: CheckPass, Evidence: path}
}

func socketVMNetClientCheck(path, configured string) Check {
	check := artifactCheck("socket_vmnet_client", path, configured, true)
	if check.Status != CheckPass {
		check.Evidence += "; " + socketVMNetInstallInstruction + "; " + socketVMNetRootOwnedInstruction
		return check
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Check{Name: "socket_vmnet_client", Status: CheckFail, Evidence: fmt.Sprintf("%s: resolve symlinks: %v", path, err)}
	}
	if isHomebrewPath(path) || isHomebrewPath(resolvedPath) || userWritable(path) || userWritable(resolvedPath) {
		location := path
		if resolvedPath != path {
			location += " (resolves to " + resolvedPath + ")"
		}
		return Check{
			Name:   "socket_vmnet_client",
			Status: CheckWarn,
			Evidence: location + " is in a Homebrew prefix or user-writable; " +
				socketVMNetRootOwnedInstruction,
		}
	}
	return check
}

func socketVMNetSocketCheck(ctx context.Context, path, configured string) Check {
	serviceHint := "; start the configured socket_vmnet service; for Homebrew shared networking run: `sudo \"$(brew --prefix)/bin/brew\" services start socket_vmnet`"
	if configured == "" {
		return Check{Name: "socket_vmnet_socket", Status: CheckFail, Evidence: "path is not configured" + serviceHint}
	}
	info, err := os.Stat(path)
	if err != nil {
		return Check{Name: "socket_vmnet_socket", Status: CheckFail, Evidence: fmt.Sprintf("%s: %v%s", path, err, serviceHint)}
	}
	if info.Mode()&os.ModeSocket == 0 {
		return Check{Name: "socket_vmnet_socket", Status: CheckFail, Evidence: path + ": not a Unix socket" + serviceHint}
	}
	dialCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", path)
	if err != nil {
		return Check{Name: "socket_vmnet_socket", Status: CheckFail, Evidence: fmt.Sprintf("%s is not connectable: %v%s", path, err, serviceHint)}
	}
	_ = conn.Close()
	return Check{Name: "socket_vmnet_socket", Status: CheckPass, Evidence: path + " is connectable"}
}

func isHomebrewPath(path string) bool {
	clean := filepath.Clean(path)
	return clean == "/opt/homebrew" || strings.HasPrefix(clean, "/opt/homebrew/") ||
		clean == "/usr/local/Homebrew" || strings.HasPrefix(clean, "/usr/local/Homebrew/") ||
		strings.Contains(clean, "/homebrew/")
}

func userWritable(path string) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok {
			mode := info.Mode().Perm()
			if stat.Uid == uint32(os.Getuid()) && mode&0200 != 0 {
				return true
			}
			if mode&0002 != 0 {
				return true
			}
			if mode&0020 != 0 && processInGroup(int(stat.Gid)) {
				return true
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func processInGroup(gid int) bool {
	if os.Getgid() == gid {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, group := range groups {
		if group == gid {
			return true
		}
	}
	return false
}

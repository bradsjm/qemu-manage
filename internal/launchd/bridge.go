package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"text/template"
	"time"

	"github.com/bradsjm/qemu-manage/internal/model"
)

const (
	socketVMNetInstallRootDefault = "/opt/socket_vmnet"
	socketVMNetRunRootDefault     = "/var/run"
	socketVMNetSocketGroup        = "staff"

	bridgeSocketReadyTimeout  = 5 * time.Second
	bridgeSocketProbeInterval = 100 * time.Millisecond
)

var socketVMNetBridgePlistTemplate = template.Must(template.New("socket_vmnet_bridge").Funcs(template.FuncMap{
	"xml": escapeXML,
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
	<dict>
		<key>Label</key>
		<string>{{xml .Label}}</string>
		<key>Program</key>
		<string>{{xml .DaemonPath}}</string>
		<key>ProgramArguments</key>
		<array>
			<string>{{xml .DaemonPath}}</string>
			<string>--socket-group={{xml .SocketGroup}}</string>
			<string>--vmnet-mode=bridged</string>
			<string>--vmnet-interface={{xml .Interface}}</string>
			<string>{{xml .SocketPath}}</string>
		</array>
		<key>RunAtLoad</key>
		<true />
		<key>KeepAlive</key>
		<true />
		<key>UserName</key>
		<string>root</string>
		<key>ProcessType</key>
		<string>Interactive</string>
	</dict>
</plist>
`))

// socketVMNetBridgePlistData holds the template fields for one bridged
// socket_vmnet LaunchDaemon plist
type socketVMNetBridgePlistData struct {
	Label       string
	DaemonPath  string
	SocketGroup string
	Interface   string
	SocketPath  string
}

// ProvisionSocketVMNetBridge hardens a Homebrew socket_vmnet installation into
// a root-owned bridged launchd service while keeping QEMU itself unprivileged.
func (m *Manager) ProvisionSocketVMNetBridge(ctx context.Context, sourceClientPath, interfaceName string) (*model.SocketVMNetConfig, error) {
	if m == nil {
		return nil, errors.New("socket_vmnet: manager is nil")
	}
	if interfaceName == "shared" {
		return nil, errors.New("socket_vmnet: shared interface does not use bridged provisioning")
	}
	if !validSocketVMNetInterface(interfaceName) {
		return nil, fmt.Errorf("socket_vmnet: invalid bridged interface %q", interfaceName)
	}

	sourceClientPath, err := validateSocketVMNetExecutable(sourceClientPath, "client")
	if err != nil {
		return nil, err
	}
	sourceDaemonPath, err := resolveSocketVMNetDaemon(sourceClientPath)
	if err != nil {
		return nil, err
	}

	config := &model.SocketVMNetConfig{
		ClientPath: m.socketVMNetClientPath(),
		SocketPath: m.socketVMNetBridgeSocketPath(interfaceName),
		Interface:  interfaceName,
	}
	plistPath := m.socketVMNetBridgePlistPath(interfaceName)
	expectedPlist, err := m.renderSocketVMNetBridgePlist(interfaceName, config.SocketPath)
	if err != nil {
		return nil, err
	}
	existingPlist, err := inspectSocketVMNetBridgePlist(plistPath)
	if err != nil {
		return nil, err
	}
	if existingPlist.Present && !bytes.Equal(existingPlist.Bytes, expectedPlist) {
		return nil, fmt.Errorf("socket_vmnet: existing launchd plist %s differs from expected contents", plistPath)
	}

	candidate, err := writeCandidate(expectedPlist)
	if err != nil {
		return nil, err
	}
	defer os.Remove(candidate)
	if err := m.lint(ctx, candidate); err != nil {
		return nil, err
	}

	if err := m.ensureSocketVMNetBinDir(ctx); err != nil {
		return nil, err
	}
	if err := m.installSocketVMNetBinary(ctx, sourceDaemonPath, m.socketVMNetDaemonPath()); err != nil {
		return nil, err
	}
	if err := m.installSocketVMNetBinary(ctx, sourceClientPath, config.ClientPath); err != nil {
		return nil, err
	}
	if !existingPlist.Present {
		if err := m.installCandidate(ctx, domainSystem, candidate, plistPath); err != nil {
			return nil, err
		}
		installed, verifyErr := inspectSocketVMNetBridgePlist(plistPath)
		if verifyErr != nil || !installed.Present || !bytes.Equal(installed.Bytes, expectedPlist) {
			if verifyErr == nil {
				verifyErr = fmt.Errorf("socket_vmnet: installed launchd plist %s differs from expected contents", plistPath)
			}
			return nil, errors.Join(verifyErr, m.removePlist(ctx, domainSystem, plistPath))
		}
	}

	label := socketVMNetBridgeLabel(interfaceName)
	target := "system/" + label
	loaded, err := m.printSocketVMNetBridgeLoaded(ctx, target, label)
	if err != nil {
		return nil, err
	}
	if !loaded {
		if err := m.bootstrap(ctx, domainSystem, plistPath); err != nil {
			return nil, err
		}
	}
	if err := m.enableSocketVMNetBridge(ctx, target); err != nil {
		return nil, err
	}
	if loaded {
		if err := m.waitForSocketVMNetReady(ctx, config.SocketPath); err == nil {
			return config, nil
		} else if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	if err := m.kickstartSocketVMNetBridge(ctx, target); err != nil {
		return nil, err
	}
	if err := m.waitForSocketVMNetReady(ctx, config.SocketPath); err != nil {
		return nil, err
	}
	return config, nil
}

// validSocketVMNetInterface enforces the interface-name character and length rules
func validSocketVMNetInterface(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '_' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// validateSocketVMNetExecutable resolves an absolute path and verifies that it
// points to an executable regular file.
func validateSocketVMNetExecutable(path string, kind string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("socket_vmnet: %s path must be absolute", kind)
	}
	clean := filepath.Clean(path)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("socket_vmnet: inspect %s %s: %w", kind, clean, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("socket_vmnet: %s %s must be an executable regular file", kind, clean)
	}
	return clean, nil
}

func resolveSocketVMNetDaemon(sourceClientPath string) (string, error) {
	candidates := []string{
		filepath.Join(filepath.Dir(sourceClientPath), "socket_vmnet"),
		"/opt/homebrew/opt/socket_vmnet/bin/socket_vmnet",
		"/usr/local/opt/socket_vmnet/bin/socket_vmnet",
	}
	var lastErr error
	for _, candidate := range candidates {
		daemonPath, err := validateSocketVMNetExecutable(candidate, "daemon")
		if err == nil {
			return daemonPath, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("socket_vmnet: Homebrew daemon not found: %w", lastErr)
}

// socketVMNetInstallRoot returns the root-owned prefix that receives the
// installed socket_vmnet binaries
func (m *Manager) socketVMNetInstallRoot() string {
	if m != nil && m.SocketVMNetInstallRoot != "" {
		return m.SocketVMNetInstallRoot
	}
	return socketVMNetInstallRootDefault
}

// socketVMNetRunRoot returns the runtime directory that holds bridged sockets
func (m *Manager) socketVMNetRunRoot() string {
	if m != nil && m.SocketVMNetRunRoot != "" {
		return m.SocketVMNetRunRoot
	}
	return socketVMNetRunRootDefault
}

// socketVMNetSystemDir returns the LaunchDaemons directory for root-scoped bridge plists
func (m *Manager) socketVMNetSystemDir() string {
	if m != nil && m.SystemDir != "" {
		return m.SystemDir
	}
	return "/Library/LaunchDaemons"
}

// socketVMNetBridgeWaiter returns the readiness probe, letting tests override it
func (m *Manager) socketVMNetBridgeWaiter() func(context.Context, string) error {
	if m != nil && m.WaitForSocketVMNet != nil {
		return m.WaitForSocketVMNet
	}
	return waitForSocketVMNetReady
}

// socketVMNetBinDir groups the installed socket_vmnet binaries under one prefix
func (m *Manager) socketVMNetBinDir() string {
	return filepath.Join(m.socketVMNetInstallRoot(), "bin")
}

// socketVMNetClientPath returns the managed client binary path
func (m *Manager) socketVMNetClientPath() string {
	return filepath.Join(m.socketVMNetBinDir(), "socket_vmnet_client")
}

// socketVMNetDaemonPath returns the managed daemon binary path
func (m *Manager) socketVMNetDaemonPath() string {
	return filepath.Join(m.socketVMNetBinDir(), "socket_vmnet")
}

// socketVMNetBridgeSocketPath names the per-interface bridged socket
func (m *Manager) socketVMNetBridgeSocketPath(interfaceName string) string {
	return filepath.Join(m.socketVMNetRunRoot(), "socket_vmnet.bridged."+interfaceName)
}

// socketVMNetBridgePlistPath returns the LaunchDaemon plist path for one bridge
func (m *Manager) socketVMNetBridgePlistPath(interfaceName string) string {
	return filepath.Join(m.socketVMNetSystemDir(), socketVMNetBridgeLabel(interfaceName)+".plist")
}

// socketVMNetBridgeLabel builds the stable launchd label for one bridge
func socketVMNetBridgeLabel(interfaceName string) string {
	return "io.github.bradsjm.qemu-manage.socket_vmnet.bridged." + interfaceName
}

func (m *Manager) renderSocketVMNetBridgePlist(interfaceName, socketPath string) ([]byte, error) {
	var buf bytes.Buffer
	if err := socketVMNetBridgePlistTemplate.Execute(&buf, socketVMNetBridgePlistData{
		Label:       socketVMNetBridgeLabel(interfaceName),
		DaemonPath:  m.socketVMNetDaemonPath(),
		SocketGroup: socketVMNetSocketGroup,
		Interface:   interfaceName,
		SocketPath:  socketPath,
	}); err != nil {
		return nil, fmt.Errorf("socket_vmnet: render bridged launchd plist: %w", err)
	}
	return buf.Bytes(), nil
}

func inspectSocketVMNetBridgePlist(path string) (pathInspection, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return pathInspection{Path: path}, nil
	}
	if err != nil {
		return pathInspection{Path: path}, fmt.Errorf("socket_vmnet: inspect %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pathInspection{Path: path}, fmt.Errorf("socket_vmnet: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return pathInspection{Path: path}, fmt.Errorf("socket_vmnet: %s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pathInspection{Path: path}, fmt.Errorf("socket_vmnet: cannot inspect ownership of %s", path)
	}
	if stat.Uid != 0 || stat.Gid != 0 || info.Mode().Perm() != 0644 {
		return pathInspection{Path: path, Present: true}, fmt.Errorf("socket_vmnet: launchd plist %s must be root:wheel with mode 0644", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return pathInspection{Path: path}, fmt.Errorf("socket_vmnet: read %s: %w", path, err)
	}
	return pathInspection{Path: path, Present: true, Bytes: data}, nil
}

func (m *Manager) ensureSocketVMNetBinDir(ctx context.Context) error {
	output, err := m.runner().Run(ctx, true, "/usr/bin/install", "-d", "-o", "root", "-g", "wheel", "-m", "0755", m.socketVMNetBinDir())
	if err != nil {
		return socketVMNetCommandError("install "+m.socketVMNetBinDir(), output, err)
	}
	return nil
}

func (m *Manager) installSocketVMNetBinary(ctx context.Context, source, destination string) error {
	if filepath.Clean(source) == filepath.Clean(destination) {
		return nil
	}
	output, err := m.runner().Run(ctx, true, "/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "0755", source, destination)
	if err != nil {
		return socketVMNetCommandError("install "+destination, output, err)
	}
	return nil
}

func (m *Manager) printSocketVMNetBridgeLoaded(ctx context.Context, target, label string) (bool, error) {
	output, err := m.runner().Run(ctx, false, launchctlPath, "print", target)
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, commandError("print "+target, output, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, commandError("print "+target, output, err)
	}
	if serviceNotFound(output, err, label) {
		return false, nil
	}
	return false, commandError("print "+target, output, err)
}

func (m *Manager) enableSocketVMNetBridge(ctx context.Context, target string) error {
	output, err := m.runner().Run(ctx, true, launchctlPath, "enable", target)
	if err != nil {
		return commandError("enable "+target, output, err)
	}
	return nil
}

func (m *Manager) kickstartSocketVMNetBridge(ctx context.Context, target string) error {
	output, err := m.runner().Run(ctx, true, launchctlPath, "kickstart", "-kp", target)
	if err != nil {
		return commandError("kickstart "+target, output, err)
	}
	return nil
}

func (m *Manager) waitForSocketVMNetReady(ctx context.Context, socketPath string) error {
	readyCtx, cancel := context.WithTimeout(ctx, bridgeSocketReadyTimeout)
	defer cancel()
	if err := m.socketVMNetBridgeWaiter()(readyCtx, socketPath); err != nil {
		return fmt.Errorf("socket_vmnet: wait for bridged socket %s: %w", socketPath, err)
	}
	return nil
}

func waitForSocketVMNetReady(ctx context.Context, socketPath string) error {
	dialer := net.Dialer{}
	ticker := time.NewTicker(bridgeSocketProbeInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return fmt.Errorf("%s is not connectable: %w", socketPath, lastErr)
		case <-ticker.C:
		}
	}
}

func socketVMNetCommandError(action string, output []byte, err error) error {
	text := bytes.TrimSpace(output)
	if len(text) == 0 {
		return fmt.Errorf("socket_vmnet: %s: %w", action, err)
	}
	return fmt.Errorf("socket_vmnet: %s: %w: %s", action, err, text)
}

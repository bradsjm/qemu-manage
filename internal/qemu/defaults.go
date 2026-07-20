package qemu

import (
	"os"

	"github.com/bradsjm/qemu-manage/internal/model"
)

// firmwareInstallation describes one install layout whose code image must pair
// with one of the candidate variables images.
type firmwareInstallation struct {
	codePath      string
	variablesPath []string
}

var firmwareInstallations = []firmwareInstallation{
	{
		codePath: "/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
		variablesPath: []string{
			"/opt/homebrew/share/qemu/edk2-arm-vars.fd",
			"/opt/homebrew/share/qemu/edk2-aarch64-vars.fd",
		},
	},
	{
		codePath: "/opt/homebrew/opt/qemu/share/qemu/edk2-aarch64-code.fd",
		variablesPath: []string{
			"/opt/homebrew/opt/qemu/share/qemu/edk2-arm-vars.fd",
			"/opt/homebrew/opt/qemu/share/qemu/edk2-aarch64-vars.fd",
		},
	},
	{
		codePath: "/usr/local/share/qemu/edk2-aarch64-code.fd",
		variablesPath: []string{
			"/usr/local/share/qemu/edk2-arm-vars.fd",
			"/usr/local/share/qemu/edk2-aarch64-vars.fd",
		},
	},
	{
		codePath: "/usr/local/opt/qemu/share/qemu/edk2-aarch64-code.fd",
		variablesPath: []string{
			"/usr/local/opt/qemu/share/qemu/edk2-arm-vars.fd",
			"/usr/local/opt/qemu/share/qemu/edk2-aarch64-vars.fd",
		},
	},
}

// socketVMNetInstallation is one packaged socket_vmnet client, daemon, and
// socket path layout to probe as a unit.
type socketVMNetInstallation struct {
	clientPath string
	daemonPath string
	socketPath string
}

var socketVMNetInstallations = []socketVMNetInstallation{
	{
		clientPath: "/opt/socket_vmnet/bin/socket_vmnet_client",
		daemonPath: "/opt/socket_vmnet/bin/socket_vmnet",
		socketPath: "/var/run/socket_vmnet",
	},
	{
		clientPath: "/opt/homebrew/opt/socket_vmnet/bin/socket_vmnet_client",
		daemonPath: "/opt/homebrew/opt/socket_vmnet/bin/socket_vmnet",
		socketPath: "/opt/homebrew/var/run/socket_vmnet",
	},
	{
		clientPath: "/usr/local/opt/socket_vmnet/bin/socket_vmnet_client",
		daemonPath: "/usr/local/opt/socket_vmnet/bin/socket_vmnet",
		socketPath: "/usr/local/var/run/socket_vmnet",
	},
	{
		clientPath: "/opt/local/bin/socket_vmnet_client",
		daemonPath: "/opt/local/bin/socket_vmnet",
		socketPath: "/var/run/socket_vmnet",
	},
}

// DiscoverFirmware returns the first readable AArch64 UEFI code and variables
// pair from one installation. Empty results mean no coherent default was found.
func DiscoverFirmware() (code, variables string) {
	return discoverFirmware(firmwareInstallations)
}

func discoverFirmware(installations []firmwareInstallation) (code, variables string) {
	for _, installation := range installations {
		if !readableRegularFile(installation.codePath) {
			continue
		}
		for _, variablesPath := range installation.variablesPath {
			if readableRegularFile(variablesPath) {
				return installation.codePath, variablesPath
			}
		}
	}
	return "", ""
}

// DiscoverSocketVMNet returns the strongest discoverable shared-network
// configuration. Client and daemon discovery are independent so a hardened
// client copied to /opt can continue using a Homebrew or MacPorts daemon
// socket, or vice versa.
func DiscoverSocketVMNet() *model.SocketVMNetConfig {
	return discoverSocketVMNet(socketVMNetInstallations)
}

// smbdInstallations lists the absolute smbd helper paths QEMU's slirp SMB
// server probes on Darwin, in priority order. Homebrew QEMU looks for the
// samba-dot-org-smbd symlink that the Homebrew samba formula installs to avoid
// clashing with macOS system smbd at /usr/sbin/smbd.
var smbdInstallations = []string{
	"/opt/homebrew/sbin/samba-dot-org-smbd",
	"/usr/local/sbin/samba-dot-org-smbd",
}

// DiscoverSMBD returns the first readable smbd helper that QEMU's slirp SMB
// server will invoke for -netdev user,...,smb=PATH. Empty means no candidate
// was found; install the Homebrew samba formula to provide it.
func DiscoverSMBD() string {
	for _, candidate := range smbdInstallations {
		if readableRegularFile(candidate) {
			return candidate
		}
	}
	return ""
}

func discoverSocketVMNet(installations []socketVMNetInstallation) *model.SocketVMNetConfig {
	clientPath := ""
	for _, installation := range installations {
		if executableAvailable(installation.clientPath) {
			clientPath = installation.clientPath
			break
		}
	}

	socketPath := ""
	for _, installation := range installations {
		if executableAvailable(installation.daemonPath) {
			socketPath = installation.socketPath
			break
		}
	}

	if clientPath == "" && socketPath == "" {
		return nil
	}
	return &model.SocketVMNetConfig{
		ClientPath: clientPath,
		SocketPath: socketPath,
		Interface:  "shared",
	}
}

// executableAvailable reports whether doctor-style executable validation would
// accept path as a usable helper.
func executableAvailable(path string) bool {
	return artifactCheck("default", path, path, true).Status == CheckPass
}

// readableRegularFile reports whether path can be opened and resolves to a
// regular file.
func readableRegularFile(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	return err == nil && info.Mode().IsRegular()
}

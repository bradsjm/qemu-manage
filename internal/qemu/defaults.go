package qemu

import (
	"os"

	"github.com/bradsjm/qemu-manage/internal/model"
)

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

// DiscoverSocketVMNet returns a complete shared-network configuration. Client
// and daemon discovery are independent so a hardened client copied to /opt can
// continue using a Homebrew or MacPorts daemon socket.
func DiscoverSocketVMNet() *model.SocketVMNetConfig {
	return discoverSocketVMNet(socketVMNetInstallations)
}

func discoverSocketVMNet(installations []socketVMNetInstallation) *model.SocketVMNetConfig {
	clientPath := ""
	for _, installation := range installations {
		if executableAvailable(installation.clientPath) {
			clientPath = installation.clientPath
			break
		}
	}
	if clientPath == "" {
		return nil
	}

	for _, installation := range installations {
		if executableAvailable(installation.daemonPath) {
			return &model.SocketVMNetConfig{
				ClientPath: clientPath,
				SocketPath: installation.socketPath,
				Interface:  "shared",
			}
		}
	}
	return nil
}

func executableAvailable(path string) bool {
	return artifactCheck("default", path, path, true).Status == CheckPass
}

func readableRegularFile(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	return err == nil && info.Mode().IsRegular()
}

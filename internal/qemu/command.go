// Package qemu renders deterministic AArch64 QEMU command lines and owns the
// concrete backend's QMP- and QGA-based process control.
package qemu

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

const defaultStartTimeout = 15 * time.Second

// Backend renders deterministic QEMU argv for one VM and starts the matching
// child process implementation.
type Backend struct {
	// StartTimeout limits how long Start waits for the child to publish its
	// initial readiness signals. Zero falls back to the package default.
	StartTimeout time.Duration
	removeFile   func(string) error
}

// NewBackend constructs a Backend with the default startup timeout.
func NewBackend() *Backend {
	return &Backend{StartTimeout: defaultStartTimeout}
}

// Render produces the complete, deterministic argv used to start QEMU for one
// AArch64 VM configuration.
func (b *Backend) Render(config *model.Config, paths backend.RuntimePaths, options backend.RenderOptions) (backend.Command, error) {
	if config == nil {
		return backend.Command{}, fmt.Errorf("qemu: config is nil")
	}
	qemuPath := backend.ResolvePath(paths.VMDir, config.QEMU.Binary)
	if !filepath.IsAbs(qemuPath) {
		return backend.Command{}, fmt.Errorf("qemu: binary path must resolve to an absolute path")
	}
	if !filepath.IsAbs(paths.QMPCommand) {
		return backend.Command{}, fmt.Errorf("qemu: QMP command socket path must be absolute")
	}
	if !filepath.IsAbs(paths.Monitor) {
		return backend.Command{}, fmt.Errorf("qemu: monitor socket path must be absolute")
	}
	if !filepath.IsAbs(paths.Console) {
		return backend.Command{}, fmt.Errorf("qemu: console socket path must be absolute")
	}
	if !filepath.IsAbs(paths.SerialLogPipe) {
		return backend.Command{}, fmt.Errorf("qemu: serial log pipe path must be absolute")
	}
	args := []string{
		"-nodefaults",
		"-display", "none",
		"-machine", effectiveMachine(config.QEMU.Machine),
		"-accel", "hvf",
		"-cpu", "host",
		"-smp", fmt.Sprintf("cpus=%d,sockets=1,cores=%d,threads=1", config.CPUs, config.CPUs),
		"-m", strconv.Itoa(config.MemoryMiB),
		"-name", keyval(config.Name),
		"-uuid", config.UUID,
		"-run-with", "exit-with-parent=on",
	}
	if config.QEMU.RTCBase != "" {
		args = append(args, "-rtc", "base="+config.QEMU.RTCBase)
	}
	if options.BootMenu {
		args = append(args, "-boot", "menu=on")
	}

	code := backend.ResolvePath(paths.VMDir, config.Firmware.Code)
	variables := backend.ResolvePath(paths.VMDir, config.Firmware.Variables)
	args = append(args,
		"-drive", "if=pflash,unit=0,format=raw,readonly=on,file.locking=off,file.filename="+keyval(code),
		"-drive", "if=pflash,unit=1,format=raw,file.filename="+keyval(variables),
	)

	for i, disk := range config.Disks {
		id := "disk" + strconv.Itoa(i)
		drive := "if=none,media=disk,id=" + id + ",file.filename=" + keyval(backend.ResolvePath(paths.VMDir, disk.Path)) + ",format=" + keyval(disk.Format) + ",aio=threads"
		if disk.Cache != "" {
			drive += ",cache=" + keyval(disk.Cache)
		}
		if disk.ReadOnly {
			drive += ",readonly=on"
		} else {
			drive += ",discard=unmap,detect-zeroes=unmap"
		}
		device := "virtio-blk-pci,drive=" + id + ",serial=" + keyval(disk.Serial) + ",bootindex=" + strconv.Itoa(disk.BootIndex)
		args = append(args, "-drive", drive, "-device", device)
	}
	args = append(args, "-device", "virtio-rng-pci")

	if config.Installer != nil {
		args = append(args,
			"-device", "virtio-scsi-pci,id=scsi0",
			"-drive", "if=none,media=cdrom,id=install,file.filename="+keyval(backend.ResolvePath(paths.VMDir, config.Installer.Path))+",format=raw,readonly=on",
			"-device", "scsi-cd,drive=install,bus=scsi0.0,bootindex="+strconv.Itoa(config.Installer.BootIndex),
		)
	}

	args = append(args,
		"-chardev", "socket,id=console0,path="+keyval(paths.Console)+",server=on,wait=off,logfile="+keyval(paths.SerialLogPipe)+",logappend=on",
		"-serial", "chardev:console0",
		"-qmp", "unix:"+keyval(paths.QMP)+",server=on,wait=off",
		"-chardev", "socket,id=qmpcommand0,path="+keyval(paths.QMPCommand)+",server=on,wait=off",
		"-mon", "chardev=qmpcommand0,mode=control",
		"-chardev", "socket,id=monitor0,path="+keyval(paths.Monitor)+",server=on,wait=off",
		"-mon", "chardev=monitor0,mode=readline",
	)
	if config.VNC != nil {
		if !filepath.IsAbs(paths.VNCSecret) {
			return backend.Command{}, fmt.Errorf("qemu: VNC secret path must be absolute")
		}
		args = append(args,
			"-device", "virtio-gpu-pci",
			"-device", "nec-usb-xhci,id=usb",
			"-device", "usb-kbd,bus=usb.0",
			"-device", "usb-tablet,bus=usb.0",
			"-object", "secret,id=vnc-password,file="+keyval(paths.VNCSecret),
			"-vnc", fmt.Sprintf("%s:%d,to=%d,password-secret=vnc-password", config.VNC.Bind, int(config.VNC.Port)-5900, int(config.VNC.PortTo)-5900),
		)
		if config.VNC.KeyboardLayout != "" {
			args = append(args, "-k", config.VNC.KeyboardLayout)
		}
	} else if len(config.USB) != 0 {
		args = append(args, "-device", "nec-usb-xhci,id=usb")
	}
	for i, usb := range config.USB {
		device := "usb-host,id=usb-host" + strconv.Itoa(i) + ",bus=usb.0"
		if usb.VendorID != "" || usb.ProductID != "" {
			device += ",vendorid=0x" + keyval(usb.VendorID) + ",productid=0x" + keyval(usb.ProductID)
		} else {
			device += ",hostbus=" + strconv.Itoa(usb.HostBus) + ",hostaddr=" + strconv.Itoa(usb.HostAddress)
		}
		args = append(args, "-device", device)
	}
	if config.GuestAgent.Enabled {
		args = append(args,
			"-device", "virtio-serial-pci",
			"-chardev", "socket,id=qga0,path="+keyval(paths.QGA)+",server=on,wait=off",
			"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
		)
	}

	commandPath := qemuPath
	switch config.Network.Mode {
	case model.NetworkUser:
		netdev := "user,id=net0"
		for _, forward := range sortedForwards(config.Network.Forwards) {
			netdev += ",hostfwd=" + keyval(forward.Protocol) + ":" + keyval(forward.HostAddress) + ":" + strconv.Itoa(int(forward.HostPort)) + "-:" + strconv.Itoa(int(forward.GuestPort))
		}
		if config.Network.SMBFolder != "" {
			netdev += ",smb=" + keyval(config.Network.SMBFolder)
		}
		args = append(args,
			"-netdev", netdev,
			"-device", "virtio-net-pci,netdev=net0,mac="+keyval(config.Network.MAC),
		)
	case model.NetworkSocketVMNet:
		bridge, err := validateSocketVMNet(config.Network)
		if err != nil {
			return backend.Command{}, err
		}
		args = append(args,
			"-netdev", "socket,id=net0,fd=3",
			"-device", "virtio-net-pci,netdev=net0,mac="+keyval(config.Network.MAC),
		)
		commandPath = filepath.Clean(bridge.ClientPath)
		args = append([]string{filepath.Clean(bridge.SocketPath), qemuPath}, args...)
	default:
		return backend.Command{}, fmt.Errorf("qemu: unsupported network mode %q", config.Network.Mode)
	}

	args = append(args, config.QEMU.ExtraArgs...)
	return backend.Command{Path: commandPath, Args: args}, nil
}

// keyval escapes literal commas for QEMU's key=value option syntax, where a
// single comma separates fields
func keyval(value string) string {
	return strings.ReplaceAll(value, ",", ",,")
}

// sortedForwards returns a sorted copy so rendered port-forward argv stays
// deterministic regardless of config input order
func sortedForwards(forwards []model.PortForward) []model.PortForward {
	result := append([]model.PortForward(nil), forwards...)
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.HostAddress != b.HostAddress {
			return a.HostAddress < b.HostAddress
		}
		if a.HostPort != b.HostPort {
			return a.HostPort < b.HostPort
		}
		return a.GuestPort < b.GuestPort
	})
	return result
}

// validateSocketVMNet rejects network features this backend cannot express and
// enforces absolute helper paths before argv rendering
func validateSocketVMNet(network model.NetworkConfig) (*model.SocketVMNetConfig, error) {
	if len(network.Forwards) != 0 {
		return nil, fmt.Errorf("qemu: socket_vmnet network cannot have port forwards")
	}
	bridge := network.SocketVMNet
	if bridge == nil {
		return nil, fmt.Errorf("qemu: socket_vmnet configuration is required")
	}
	if !filepath.IsAbs(bridge.ClientPath) {
		return nil, fmt.Errorf("qemu: socket_vmnet client path must be absolute")
	}
	if !filepath.IsAbs(bridge.SocketPath) {
		return nil, fmt.Errorf("qemu: socket_vmnet socket path must be absolute")
	}
	if bridge.Interface == "" {
		return nil, fmt.Errorf("qemu: socket_vmnet interface must not be empty")
	}
	return bridge, nil
}

func effectiveMachine(configured string) string {
	if configured == "" {
		return "virt"
	}
	return configured
}

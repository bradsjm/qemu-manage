package qemu

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
)

func renderFixture() (*model.Config, backend.RuntimePaths) {
	return &model.Config{
			Name: "ha,vm", UUID: "123e4567-e89b-42d3-a456-426614174000", CPUs: 4, MemoryMiB: 4096,
			Firmware: model.FirmwareConfig{Code: "firmware/code,uefi.fd", Variables: "firmware/vars.fd"},
			Disks: []model.DiskConfig{
				{Path: "disks/root,one.qcow2", Format: "qcow2", Cache: "none", AIO: "native", Serial: "root,serial", BootIndex: 1},
				{Path: "/images/recovery.raw", Format: "raw", Serial: "recovery", BootIndex: 2, ReadOnly: true},
			},
			Installer:  &model.InstallerConfig{Path: "install/os,arm.iso", BootIndex: 0},
			GuestAgent: model.GuestAgentConfig{Enabled: true},
			Network: model.NetworkConfig{Mode: model.NetworkUser, MAC: "02:00:00:00:00:01", Forwards: []model.PortForward{
				{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 8123, GuestPort: 8123},
				{Protocol: "tcp", HostAddress: "127.0.0.1", HostPort: 2222, GuestPort: 22},
				{Protocol: "udp", HostAddress: "127.0.0.1", HostPort: 5353, GuestPort: 5353},
			}},
			QEMU: model.QEMUConfig{Binary: "bin/qemu-system-aarch64"},
		}, backend.RuntimePaths{
			VMDir: "/vms/ha", QMP: "/run/qmp,0.sock", QMPCommand: "/run/qmp-command,0.sock",
			QGA: "/run/qga.sock", Console: "/run/console.sock", Monitor: "/run/monitor,0.sock",
			SerialLog: "/logs/serial,0.log",
		}
}

func TestRenderUserNetworkGolden(t *testing.T) {
	config, paths := renderFixture()
	got, err := NewBackend().Render(config, paths)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := backend.Command{Path: "/vms/ha/bin/qemu-system-aarch64", Args: []string{
		"-nodefaults", "-display", "none", "-machine", "virt", "-accel", "hvf", "-cpu", "host",
		"-smp", "cpus=4,sockets=1,cores=4,threads=1", "-m", "4096", "-name", "ha,,vm", "-uuid", "123e4567-e89b-42d3-a456-426614174000", "-run-with", "exit-with-parent=on",
		"-drive", "if=pflash,unit=0,format=raw,readonly=on,file.locking=off,file.filename=/vms/ha/firmware/code,,uefi.fd",
		"-drive", "if=pflash,unit=1,format=raw,file.filename=/vms/ha/firmware/vars.fd",
		"-drive", "if=none,media=disk,id=disk0,file.filename=/vms/ha/disks/root,,one.qcow2,format=qcow2,cache=none,aio=native,discard=unmap,detect-zeroes=unmap",
		"-device", "virtio-blk-pci,drive=disk0,serial=root,,serial,bootindex=1",
		"-drive", "if=none,media=disk,id=disk1,file.filename=/images/recovery.raw,format=raw,readonly=on",
		"-device", "virtio-blk-pci,drive=disk1,serial=recovery,bootindex=2", "-device", "virtio-rng-pci",
		"-device", "virtio-scsi-pci,id=scsi0", "-drive", "if=none,media=cdrom,id=install,file.filename=/vms/ha/install/os,,arm.iso,format=raw,readonly=on",
		"-device", "scsi-cd,drive=install,bus=scsi0.0,bootindex=0",
		"-chardev", "socket,id=console0,path=/run/console.sock,server=on,wait=off,logfile=/logs/serial,,0.log,logappend=on", "-serial", "chardev:console0",
		"-qmp", "unix:/run/qmp,,0.sock,server=on,wait=off",
		"-chardev", "socket,id=qmpcommand0,path=/run/qmp-command,,0.sock,server=on,wait=off",
		"-mon", "chardev=qmpcommand0,mode=control",
		"-chardev", "socket,id=monitor0,path=/run/monitor,,0.sock,server=on,wait=off",
		"-mon", "chardev=monitor0,mode=readline",
		"-device", "virtio-serial-pci", "-chardev", "socket,id=qga0,path=/run/qga.sock,server=on,wait=off",
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
		"-netdev", "user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:8123-:8123,hostfwd=udp:127.0.0.1:5353-:5353",
		"-device", "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command mismatch\n got: %#v\nwant: %#v", got, want)
	}
	assertSafeQEMUCommand(t, got)
}

func TestRenderSocketVMNetGolden(t *testing.T) {
	config, paths := renderFixture()
	config.Name = "ha"
	config.Firmware.Code = "/fw/code.fd"
	config.Firmware.Variables = "/fw/vars.fd"
	config.Disks = nil
	config.Installer = nil
	config.GuestAgent.Enabled = false
	config.Network = model.NetworkConfig{Mode: model.NetworkSocketVMNet, MAC: "02:00:00:00:00:01", SocketVMNet: &model.SocketVMNetConfig{ClientPath: "/opt/socket_vmnet/bin/socket_vmnet_client", SocketPath: "/var/run/socket_vmnet.bridged.vlan0", Interface: "vlan0"}}
	got, err := NewBackend().Render(config, paths)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := backend.Command{Path: "/opt/socket_vmnet/bin/socket_vmnet_client", Args: []string{
		"/var/run/socket_vmnet.bridged.vlan0", "/vms/ha/bin/qemu-system-aarch64", "-nodefaults", "-display", "none", "-machine", "virt", "-accel", "hvf", "-cpu", "host", "-smp", "cpus=4,sockets=1,cores=4,threads=1", "-m", "4096", "-name", "ha", "-uuid", "123e4567-e89b-42d3-a456-426614174000", "-run-with", "exit-with-parent=on",
		"-drive", "if=pflash,unit=0,format=raw,readonly=on,file.locking=off,file.filename=/fw/code.fd", "-drive", "if=pflash,unit=1,format=raw,file.filename=/fw/vars.fd", "-device", "virtio-rng-pci",
		"-chardev", "socket,id=console0,path=/run/console.sock,server=on,wait=off,logfile=/logs/serial,,0.log,logappend=on", "-serial", "chardev:console0",
		"-qmp", "unix:/run/qmp,,0.sock,server=on,wait=off",
		"-chardev", "socket,id=qmpcommand0,path=/run/qmp-command,,0.sock,server=on,wait=off",
		"-mon", "chardev=qmpcommand0,mode=control",
		"-chardev", "socket,id=monitor0,path=/run/monitor,,0.sock,server=on,wait=off",
		"-mon", "chardev=monitor0,mode=readline",
		"-netdev", "socket,id=net0,fd=3", "-device", "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command mismatch\n got: %#v\nwant: %#v", got, want)
	}
	assertSafeQEMUCommand(t, got)
}

func TestRenderUSBWithoutVNCAddsControllerAndHostDevicesInOrder(t *testing.T) {
	config, paths := renderFixture()
	config.USB = []model.USBDeviceConfig{
		{VendorID: "1a2b", ProductID: "c3d4"},
		{HostBus: 7, HostAddress: 9},
	}
	got, err := NewBackend().Render(config, paths)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	requireArgSequence(t, got.Args, []string{
		"-device", "nec-usb-xhci,id=usb",
		"-device", "usb-host,id=usb-host0,bus=usb.0,vendorid=0x1a2b,productid=0xc3d4",
		"-device", "usb-host,id=usb-host1,bus=usb.0,hostbus=7,hostaddr=9",
	})
	if count := strings.Count(strings.Join(got.Args, " "), "nec-usb-xhci,id=usb"); count != 1 {
		t.Fatalf("controller count = %d, want 1", count)
	}
	assertSafeQEMUCommand(t, got)
}

func TestRenderUSBWithVNCReusesController(t *testing.T) {
	config, paths := renderFixture()
	config.VNC = &model.VNCConfig{
		Bind:     "127.0.0.1",
		Port:     5900,
		PortTo:   5999,
		Password: "secret12",
	}
	config.USB = []model.USBDeviceConfig{
		{VendorID: "1a2b", ProductID: "c3d4"},
		{HostBus: 7, HostAddress: 9},
	}
	paths.VNCSecret = "/run/vnc,password"
	got, err := NewBackend().Render(config, paths)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	requireArgSequence(t, got.Args, []string{
		"-device", "virtio-gpu-pci",
		"-device", "nec-usb-xhci,id=usb",
		"-device", "usb-kbd,bus=usb.0",
		"-device", "usb-tablet,bus=usb.0",
		"-object", "secret,id=vnc-password,file=/run/vnc,,password",
		"-vnc", "127.0.0.1:0,to=99,password-secret=vnc-password",
		"-device", "usb-host,id=usb-host0,bus=usb.0,vendorid=0x1a2b,productid=0xc3d4",
		"-device", "usb-host,id=usb-host1,bus=usb.0,hostbus=7,hostaddr=9",
	})
	if count := strings.Count(strings.Join(got.Args, " "), "nec-usb-xhci,id=usb"); count != 1 {
		t.Fatalf("controller count = %d, want 1", count)
	}
	if strings.Contains(strings.Join(append([]string{got.Path}, got.Args...), " "), config.VNC.Password) {
		t.Fatalf("command leaked VNC password: %#v", got)
	}
	assertSafeQEMUCommand(t, got)
}

func TestRenderRequiresAbsolutePrivateMonitorPaths(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*backend.RuntimePaths)
		want   string
	}{
		{
			name: "qmp command",
			mutate: func(paths *backend.RuntimePaths) {
				paths.QMPCommand = "relative/qmp-command.sock"
			},
			want: "qemu: QMP command socket path must be absolute",
		},
		{
			name: "monitor",
			mutate: func(paths *backend.RuntimePaths) {
				paths.Monitor = "relative/monitor.sock"
			},
			want: "qemu: monitor socket path must be absolute",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config, paths := renderFixture()
			tc.mutate(&paths)
			if _, err := NewBackend().Render(config, paths); err == nil || err.Error() != tc.want {
				t.Fatalf("Render error = %v, want %q", err, tc.want)
			}
		})
	}
}

func requireArgSequence(t *testing.T, got, want []string) {
	t.Helper()
	for start := 0; start+len(want) <= len(got); start++ {
		if reflect.DeepEqual(got[start:start+len(want)], want) {
			return
		}
	}
	t.Fatalf("args missing sequence\n got: %#v\nwant: %#v", got, want)
}

func assertSafeQEMUCommand(t *testing.T, command backend.Command) {
	t.Helper()
	all := append([]string{command.Path}, command.Args...)
	joined := strings.Join(all, " ")
	for _, forbidden := range []string{"-nographic", "vmnet-bridged", "tcg"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("command contains forbidden %q: %s", forbidden, joined)
		}
	}
	for _, shell := range []string{"sh", "bash", "zsh"} {
		if command.Path == shell || strings.HasSuffix(command.Path, "/"+shell) {
			t.Errorf("command invokes shell %q", command.Path)
		}
	}
	if !strings.Contains(joined, "id=net0") || !strings.Contains(joined, "netdev=net0") {
		t.Errorf("network does not bind both sides to net0: %s", joined)
	}
}

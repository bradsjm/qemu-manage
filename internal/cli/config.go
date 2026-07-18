package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qemu-manage/internal/model"
)

type optionalValue[T any] struct {
	set   bool
	value T
	parse func(string) (T, error)
}

func (v *optionalValue[T]) String() string {
	if !v.set {
		return ""
	}
	return fmt.Sprint(v.value)
}

func (v *optionalValue[T]) Set(raw string) error {
	value, err := v.parse(raw)
	if err != nil {
		return err
	}
	v.set = true
	v.value = value
	return nil
}

type forwardValues []model.PortForward

func (v *forwardValues) String() string { return "" }

func (v *forwardValues) Set(raw string) error {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 {
		return fmt.Errorf("must have the form proto:IPv4:host-port:guest-port")
	}
	protocol := strings.ToLower(parts[0])
	if protocol != "tcp" && protocol != "udp" {
		return fmt.Errorf("invalid protocol %q; valid values: tcp, udp", parts[0])
	}
	address := net.ParseIP(parts[1])
	if address == nil || address.To4() == nil || strings.Contains(parts[1], ":") {
		return fmt.Errorf("host address must be an IPv4 literal")
	}
	hostPort, err := parsePort(parts[2])
	if err != nil {
		return fmt.Errorf("host port: %w", err)
	}
	guestPort, err := parsePort(parts[3])
	if err != nil {
		return fmt.Errorf("guest port: %w", err)
	}
	*v = append(*v, model.PortForward{
		Protocol: protocol, HostAddress: address.To4().String(), HostPort: hostPort, GuestPort: guestPort,
	})
	return nil
}

func (a *App) runSet(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	_ = ctx
	_ = stdin
	_ = stdout

	positiveInt := func(raw string) (int, error) {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return 0, fmt.Errorf("must be a positive integer")
		}
		return value, nil
	}
	var cpus optionalValue[int]
	cpus.parse = positiveInt
	var memory optionalValue[int]
	memory.parse = parseMemoryMiB
	var restart optionalValue[model.RestartPolicy]
	restart.parse = func(raw string) (model.RestartPolicy, error) {
		value := model.RestartPolicy(raw)
		if value != model.RestartNever && value != model.RestartOnFailure {
			return "", fmt.Errorf("valid values: never, on-failure")
		}
		return value, nil
	}
	var timeout optionalValue[int]
	timeout.parse = parseSetWholeSeconds
	var guestAgent optionalValue[bool]
	guestAgent.parse = func(raw string) (bool, error) {
		switch raw {
		case "on":
			return true, nil
		case "off":
			return false, nil
		default:
			return false, fmt.Errorf("valid values: on, off")
		}
	}
	var networkMode optionalValue[model.NetworkMode]
	networkMode.parse = func(raw string) (model.NetworkMode, error) {
		value := model.NetworkMode(raw)
		if value != model.NetworkUser && value != model.NetworkSocketVMNet {
			return "", fmt.Errorf("valid values: user, socket_vmnet")
		}
		return value, nil
	}
	var clientPath, socketPath, interfaceName optionalValue[string]
	for _, value := range []*optionalValue[string]{&clientPath, &socketPath, &interfaceName} {
		value.parse = func(raw string) (string, error) { return raw, nil }
	}
	var forwards forwardValues
	var clearForwards bool

	flags := quietFlagSet("set")
	flags.Var(&cpus, "cpus", "virtual CPU count")
	flags.Var(&memory, "memory", "memory as whole MiB or GiB")
	flags.Var(&restart, "restart-policy", "never or on-failure")
	flags.Var(&timeout, "shutdown-timeout", "whole-second shutdown timeout")
	flags.Var(&guestAgent, "guest-agent", "on or off")
	flags.Var(&networkMode, "network", "user or socket_vmnet")
	flags.Var(&forwards, "forward", "proto:IPv4:host-port:guest-port (repeatable)")
	flags.BoolVar(&clearForwards, "clear-forwards", false, "replace existing forwards")
	flags.Var(&clientPath, "socket-vmnet-client", "absolute socket_vmnet client path")
	flags.Var(&socketPath, "socket-vmnet-socket", "absolute socket_vmnet socket path")
	flags.Var(&interfaceName, "socket-vmnet-interface", "socket_vmnet interface")
	if err := flags.Parse(args); err != nil {
		return usageErrorf("set %s: %v", name, err)
	}
	if flags.NArg() != 0 {
		return usageErrorf("set %s: unexpected argument %q", name, flags.Arg(0))
	}
	if flags.NFlag() == 0 {
		return usageErrorf("set %s: no changes requested; provide at least one option", name)
	}

	lock, err := a.Store.LockName(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	config, err := lock.Load()
	if err != nil {
		return err
	}
	oldRestartPolicy := config.RestartPolicy
	oldShutdownTimeoutSeconds := config.ShutdownTimeoutSeconds

	if cpus.set {
		config.CPUs = cpus.value
	}
	if memory.set {
		config.MemoryMiB = memory.value
	}
	if restart.set {
		config.RestartPolicy = restart.value
	}
	if timeout.set {
		config.ShutdownTimeoutSeconds = timeout.value
	}
	if guestAgent.set {
		config.GuestAgent.Enabled = guestAgent.value
	}

	targetMode := config.Network.Mode
	if networkMode.set {
		targetMode = networkMode.value
	}
	switch targetMode {
	case model.NetworkUser:
		if config.Network.Mode == model.NetworkSocketVMNet {
			config.Network.Forwards = nil
		}
		config.Network.Mode = model.NetworkUser
		config.Network.SocketVMNet = nil
		if clearForwards {
			config.Network.Forwards = nil
		}
		config.Network.Forwards = append(config.Network.Forwards, forwards...)
		if clientPath.set || socketPath.set || interfaceName.set {
			return usageErrorf("socket_vmnet fields require --network socket_vmnet")
		}
	case model.NetworkSocketVMNet:
		settings := config.Network.SocketVMNet
		customSocketVMNet := clientPath.set || socketPath.set || interfaceName.set
		if settings == nil && !customSocketVMNet && a.DiscoverSocketVMNet != nil {
			if discovered := a.DiscoverSocketVMNet(); discovered != nil {
				copy := *discovered
				settings = &copy
			}
		}
		if settings == nil {
			settings = &model.SocketVMNetConfig{}
		}
		if clientPath.set {
			settings.ClientPath = clientPath.value
		}
		if socketPath.set {
			settings.SocketPath = socketPath.value
		}
		if interfaceName.set {
			settings.Interface = interfaceName.value
		}
		if !filepath.IsAbs(settings.ClientPath) || !filepath.IsAbs(settings.SocketPath) || settings.Interface == "" {
			return usageErrorf(
				"set %s: socket_vmnet requires --socket-vmnet-client, --socket-vmnet-socket, and --socket-vmnet-interface when defaults cannot be discovered; install with `brew install socket_vmnet`",
				name,
			)
		}
		config.Network.Mode = model.NetworkSocketVMNet
		config.Network.SocketVMNet = settings
		config.Network.Forwards = nil
		if len(forwards) != 0 {
			return usageErrorf("--forward is incompatible with socket_vmnet")
		}
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if err := lock.Save(config); err != nil {
		return err
	}
	if config.Autostart.Scope != model.AutostartNone &&
		(oldRestartPolicy != config.RestartPolicy || oldShutdownTimeoutSeconds != config.ShutdownTimeoutSeconds) {
		warnStaleAutostart(stderr, name, config.Autostart.Scope)
	}
	return nil
}

func (a *App) runConfig(ctx context.Context, subcommand string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	_ = ctx
	_ = stdin
	switch subcommand {
	case "show":
		if len(args) != 1 || strings.HasPrefix(args[0], "-") {
			return usageErrorf("config show requires NAME")
		}
		config, err := a.Store.Load(args[0])
		if err != nil {
			return err
		}
		data, err := model.CanonicalJSON(config)
		if err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	case "validate":
		if len(args) != 1 {
			return usageErrorf("config validate requires FILE")
		}
		file, err := os.Open(args[0])
		if err != nil {
			return fmt.Errorf("config: open %q: %w", args[0], err)
		}
		_, decodeErr := model.Decode(file)
		closeErr := file.Close()
		if decodeErr != nil {
			return decodeErr
		}
		return closeErr
	case "apply":
		if len(args) != 2 || strings.HasPrefix(args[0], "-") {
			return usageErrorf("config apply requires NAME FILE")
		}
		file, err := os.Open(args[1])
		if err != nil {
			return fmt.Errorf("config: open %q: %w", args[1], err)
		}
		replacement, decodeErr := model.Decode(file)
		closeErr := file.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if closeErr != nil {
			return closeErr
		}
		lock, err := a.Store.LockName(args[0])
		if err != nil {
			return err
		}
		defer lock.Close()
		current, err := lock.Load()
		if err != nil {
			return err
		}
		if err := model.ValidateApply(current, replacement); err != nil {
			return err
		}
		if err := lock.Save(replacement); err != nil {
			return err
		}
		if current.Autostart.Scope != model.AutostartNone &&
			(current.RestartPolicy != replacement.RestartPolicy ||
				current.ShutdownTimeoutSeconds != replacement.ShutdownTimeoutSeconds) {
			warnStaleAutostart(stderr, args[0], current.Autostart.Scope)
		}
		return nil
	default:
		return usageErrorf("unknown config command %q", subcommand)
	}
}

func warnStaleAutostart(stderr io.Writer, name string, scope model.AutostartScope) {
	fmt.Fprintf(
		stderr,
		"warning: launchd configuration is stale; run %q, %q, then %q\n",
		"qemu-manage stop "+name,
		"qemu-manage autostart disable "+name,
		"qemu-manage autostart enable "+name+" --scope "+string(scope),
	)
}

func parseMemoryMiB(raw string) (int, error) {
	multiplier := 1
	number := raw
	switch {
	case strings.HasSuffix(raw, "MiB"):
		number = strings.TrimSuffix(raw, "MiB")
	case strings.HasSuffix(raw, "GiB"):
		number = strings.TrimSuffix(raw, "GiB")
		multiplier = 1024
	default:
		return 0, fmt.Errorf("must be a whole number of MiB or GiB")
	}
	value, err := strconv.ParseUint(number, 10, 31)
	if err != nil || value == 0 || value > uint64(^uint(0)>>1)/uint64(multiplier) {
		return 0, fmt.Errorf("must be a positive whole number of MiB or GiB")
	}
	return int(value) * multiplier, nil
}

func parseSetWholeSeconds(raw string) (int, error) {
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 || duration%time.Second != 0 {
		return 0, fmt.Errorf("must be a positive whole-second duration")
	}
	seconds := duration / time.Second
	if seconds > time.Duration(^uint(0)>>1) {
		return 0, fmt.Errorf("duration is too large")
	}
	return int(seconds), nil
}

func parsePort(raw string) (uint16, error) {
	value, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("must be between 1 and 65535")
	}
	return uint16(value), nil
}

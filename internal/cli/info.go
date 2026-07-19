package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
)

func (a *App) runInfoCommand(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch command {
	case "showcmd":
		return a.runShowcmd(args, stdout)
	case "status":
		return a.runStatus(ctx, args, stdout)
	case "list":
		return a.runList(ctx, args, stdout)
	case "delete":
		return a.runDelete(ctx, args, stdin, stdout)
	default:
		return usageErrorf("unknown information command %q", command)
	}
}

func (a *App) runShowcmd(args []string, stdout io.Writer) error {
	name, remaining, err := nameBeforeFlags("showcmd", args)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return usageErrorf("showcmd %s: unexpected argument %q", name, remaining[0])
	}
	config, err := a.Store.Load(name)
	if err != nil {
		return err
	}
	implementation, err := a.Backends.Lookup(string(config.Backend))
	if err != nil {
		return fmt.Errorf("qemu: %w", err)
	}
	paths := a.Store.Paths(config)
	command, err := implementation.Render(config, backendPaths(paths), backend.RenderOptions{})
	if err != nil {
		return fmt.Errorf("qemu: render command: %w", err)
	}
	words := make([]string, 0, len(command.Args)+1)
	words = append(words, command.Path)
	words = append(words, command.Args...)
	for index, word := range words {
		if index != 0 {
			if _, err := io.WriteString(stdout, " "); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(stdout, quotePOSIX(word)); err != nil {
			return err
		}
	}
	_, err = io.WriteString(stdout, "\n")
	return err
}

func quotePOSIX(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (a *App) runStatus(ctx context.Context, args []string, stdout io.Writer) error {
	name, remaining := "", args
	if len(remaining) != 0 && !strings.HasPrefix(remaining[0], "-") {
		name, remaining = remaining[0], remaining[1:]
	}
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(remaining); err != nil || flags.NArg() != 0 {
		if err != nil {
			return usageErrorf("status: %v", err)
		}
		return usageErrorf("usage: qemu-manage status [NAME] [--json]")
	}
	if name == "" {
		return a.writeStatusRows(ctx, *jsonOutput, stdout)
	}
	config, err := a.Store.Load(name)
	if err != nil {
		if !*jsonOutput {
			return err
		}
		row := StatusRow{
			Name:            name,
			State:           model.RunStateFailed,
			RestartRequired: false,
			Error:           err.Error(),
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(row)
	}
	row, err := a.statusRow(ctx, config)
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(row)
	}
	if err := writeRows([]StatusRow{row}, false, stdout); err != nil {
		return err
	}
	if config.Network.SMBFolder != "" {
		return writeSMBMountHelp(stdout, config.Network.SMBFolder)
	}
	return nil
}

func (a *App) runList(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err != nil {
			return usageErrorf("list: %v", err)
		}
		return usageErrorf("usage: qemu-manage list [--json]")
	}
	return a.writeStatusRows(ctx, *jsonOutput, stdout)
}

func (a *App) writeStatusRows(ctx context.Context, jsonOutput bool, stdout io.Writer) error {
	entries, err := os.ReadDir(a.Store.DataRoot)
	if err != nil {
		return fmt.Errorf("config: list VMs: %w", err)
	}
	rows := make([]StatusRow, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".locks" {
			continue
		}
		config, loadErr := a.Store.Load(entry.Name())
		if loadErr != nil {
			rows = append(rows, StatusRow{Name: entry.Name(), State: model.RunStateFailed, Error: loadErr.Error()})
			continue
		}
		row, statusErr := a.statusRow(ctx, config)
		if statusErr != nil {
			rows = append(rows, StatusRow{Name: config.Name, State: model.RunStateFailed, Error: statusErr.Error()})
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return writeRows(rows, jsonOutput, stdout)
}

func (a *App) statusRow(ctx context.Context, config *model.Config) (StatusRow, error) {
	currentHash, err := model.Hash(config)
	if err != nil {
		return StatusRow{}, fmt.Errorf("config: hash %q: %w", config.Name, err)
	}
	if a.Runtime == nil {
		return StatusRow{}, errors.New("runtime: service is unavailable")
	}
	row, err := a.Runtime.Status(ctx, config)
	if err != nil {
		return StatusRow{}, fmt.Errorf("runtime: status %q: %w", config.Name, err)
	}
	row.Name = config.Name
	row.CurrentConfigSHA256 = currentHash
	row.RestartRequired = row.RunningConfigSHA256 != "" && row.RunningConfigSHA256 != currentHash
	return row, nil
}

func writeRows(rows []StatusRow, jsonOutput bool, stdout io.Writer) error {
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(rows)
	}
	if _, err := fmt.Fprintln(stdout, "NAME\tSTATE\tRESTART REQUIRED\tERROR"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\n", row.Name, row.State, row.RestartRequired, row.Error); err != nil {
			return err
		}
	}
	return nil
}

// writeSMBMountHelp emits the stable SMB host-folder and Linux CIFS mount recipe
// shared by create and named status output. QEMU's built-in user-network SMB
// server always exports one [qemu] share at 10.0.2.4, so the recipe is fixed.
func writeSMBMountHelp(stdout io.Writer, hostPath string) error {
	if _, err := fmt.Fprintf(stdout, "SMB host folder: %s\n", hostPath); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stdout, "Linux guest mount: sudo mount -t cifs //10.0.2.4/qemu /mnt/share -o username=guest")
	return err
}

func terminalReader(input io.Reader) bool {
	file, ok := input.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (a *App) runDelete(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageErrorf("usage: qemu-manage delete NAME [--force]")
	}
	name := args[0]
	flags := flag.NewFlagSet("delete", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	force := flags.Bool("force", false, "skip confirmation")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		if err != nil {
			return usageErrorf("delete: %v", err)
		}
		return usageErrorf("usage: qemu-manage delete NAME [--force]")
	}

	config, err := a.Store.Load(name)
	if err != nil {
		return err
	}
	initialID := config.ID
	if config.Autostart.Scope != model.AutostartNone {
		return fmt.Errorf("launchd: VM %q has autostart scope %q; run `qemu-manage autostart disable %s` first", name, config.Autostart.Scope, name)
	}
	if !*force {
		if a.IsTerminal == nil || !a.IsTerminal(stdin) {
			return fmt.Errorf("config: deleting %q noninteractively requires --force; this permanently removes its managed disks, firmware, and configuration", name)
		}
		if _, err := fmt.Fprintf(
			stdout,
			"WARNING: Permanently delete VM %q, including its managed disks, firmware, and configuration? [y/N] ",
			name,
		); err != nil {
			return err
		}
		answer, readErr := bufio.NewReader(stdin).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("config: read deletion confirmation: %w", readErr)
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			_, err := fmt.Fprintln(stdout, "Deletion cancelled.")
			return err
		}
	}
	return a.Store.Delete(name, func(lockedConfig *model.Config, _ store.Paths) error {
		if lockedConfig.ID != initialID {
			return fmt.Errorf("config: VM %q identity changed before deletion; refusing to delete a replacement VM", name)
		}
		recovery := fmt.Sprintf("run `qemu-manage autostart disable %s` first", name)
		if a.Launchd == nil {
			return fmt.Errorf("launchd: service is unavailable; %s", recovery)
		}
		status, err := a.Launchd.Status(ctx, lockedConfig.Name)
		if err != nil {
			return fmt.Errorf("launchd: inspect VM %q before deletion: %w; %s", name, err, recovery)
		}
		if status.Login.Error != "" {
			return fmt.Errorf("launchd: inspect login job for VM %q: %s; %s", name, status.Login.Error, recovery)
		}
		if status.Boot.Error != "" {
			return fmt.Errorf("launchd: inspect boot job for VM %q: %s; %s", name, status.Boot.Error, recovery)
		}
		if status.Login.FilePresent || status.Login.Loaded || status.Boot.FilePresent || status.Boot.Loaded {
			return fmt.Errorf("launchd: VM %q still has an autostart plist or loaded job; %s", name, recovery)
		}
		return nil
	})
}

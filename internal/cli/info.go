package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/pterm/pterm"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
)

const (
	monitoringRequestTimeout   = 2 * time.Second
	maxMonitoringResponseBytes = 128 << 20
)

type monitoringInfoResponse struct {
	APIVersion *int                 `json:"api_version"`
	VM         *monitoringInfoVM    `json:"vm"`
	QEMU       *monitoringInfoQEMU  `json:"qemu"`
	GuestAgent *monitoringInfoAgent `json:"guest_agent"`
}

type monitoringInfoVM struct {
	ID        *string `json:"id"`
	Name      *string `json:"name"`
	CPUs      *int    `json:"cpus"`
	MemoryMiB *int    `json:"memory_mib"`
}

type monitoringInfoQEMU struct {
	Version     *monitoringInfoQEMUVersion `json:"version"`
	Accelerator *string                    `json:"accelerator"`
}

type monitoringInfoQEMUVersion struct {
	Major   *int    `json:"major"`
	Minor   *int    `json:"minor"`
	Micro   *int    `json:"micro"`
	Package *string `json:"package"`
}

type monitoringInfoAgent struct {
	Configured   *bool            `json:"configured"`
	Version      *string          `json:"version"`
	Capabilities *map[string]bool `json:"capabilities"`
}

type monitoringStatusResponse struct {
	APIVersion   *int                                  `json:"api_version"`
	ObservedAt   *time.Time                            `json:"observed_at"`
	Health       *monitoringStatusHealth               `json:"health"`
	VM           *monitoringStatusVM                   `json:"vm"`
	QMP          *monitoringStatusQMP                  `json:"qmp"`
	Process      *monitoringStatusProcess              `json:"process"`
	BlockDevices *[]backend.QEMUBlockDevice            `json:"block_devices"`
	GuestAgent   *monitoringStatusAgent                `json:"guest_agent"`
	Collectors   *map[string]monitoringStatusCollector `json:"collectors"`
	Guest        *monitoringStatusGuest                `json:"guest"`
}

type monitoringStatusHealth struct {
	Status *string `json:"status"`
	Code   string  `json:"code,omitempty"`
}

type monitoringStatusVM struct {
	ID            *string    `json:"id"`
	Name          *string    `json:"name"`
	State         *string    `json:"state"`
	StartedAt     *time.Time `json:"started_at"`
	UptimeSeconds *float64   `json:"uptime_seconds"`
}

type monitoringStatusQMP struct {
	Version *monitoringInfoQEMUVersion `json:"version"`
}

type monitoringStatusProcess struct {
	StatsUp             *bool          `json:"stats_up"`
	PID                 *int           `json:"pid"`
	CPUSeconds          *monitoringCPU `json:"cpu_seconds"`
	ResidentMemoryBytes *uint64        `json:"resident_memory_bytes"`
	Threads             *int           `json:"threads"`
}

type monitoringCPU struct {
	User   float64 `json:"user"`
	System float64 `json:"system"`
}

type monitoringStatusAgent struct {
	Configured *bool   `json:"configured"`
	Up         *bool   `json:"up"`
	Version    *string `json:"version"`
}

type monitoringStatusCollector struct {
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

type monitoringStatusGuest struct {
	Load              *backend.GuestLoad           `json:"load"`
	Filesystems       []backend.GuestFilesystem    `json:"filesystems"`
	NetworkInterfaces []monitoringNetworkInterface `json:"network_interfaces"`
	Disks             []backend.GuestDisk          `json:"disks"`
}

type monitoringNetworkInterface struct {
	Name      string                    `json:"name"`
	Addresses *[]backend.GuestIPAddress `json:"addresses"`
}

type monitoringPayload struct {
	infoRaw   json.RawMessage
	statusRaw json.RawMessage
	info      monitoringInfoResponse
	status    monitoringStatusResponse
}

type infoMonitoringOutput struct {
	Available bool            `json:"available"`
	Reason    string          `json:"reason,omitempty"`
	Port      *uint16         `json:"port,omitempty"`
	Error     string          `json:"error,omitempty"`
	Info      json.RawMessage `json:"info,omitempty"`
	Status    json.RawMessage `json:"status,omitempty"`
}

type infoOutput struct {
	Name            string                `json:"name"`
	State           model.RunState        `json:"state"`
	Running         *bool                 `json:"running,omitempty"`
	RestartRequired *bool                 `json:"restart_required,omitempty"`
	CPUs            *int                  `json:"cpus,omitempty"`
	MemoryMiB       *int                  `json:"memory_mib,omitempty"`
	Network         *model.NetworkMode    `json:"network,omitempty"`
	Autostart       *model.AutostartScope `json:"autostart,omitempty"`
	PID             *int                  `json:"pid,omitempty"`
	StartedAt       *time.Time            `json:"started_at,omitempty"`
	VNC             *backend.VNCEndpoint  `json:"vnc,omitempty"`
	Monitoring      *infoMonitoringOutput `json:"monitoring,omitempty"`
}

// fetchMonitoringEndpoint performs one bounded request to the VM's loopback
// monitoring service. Callers deliberately pass only /info or /status.
func fetchMonitoringEndpoint(ctx context.Context, client *http.Client, port uint16, path string) (json.RawMessage, error) {
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if client == nil {
		client = newImageHTTPClient()
	}
	selected := *client
	selected.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("redirects are not permitted")
	}
	response, err := selected.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	closeResponse := func() error {
		if closeErr := response.Body.Close(); closeErr != nil {
			return fmt.Errorf("close %s response: %w", path, closeErr)
		}
		return nil
	}
	if response.StatusCode != http.StatusOK {
		closeErr := closeResponse()
		if closeErr != nil {
			return nil, closeErr
		}
		return nil, fmt.Errorf("GET %s: HTTP %d %s", path, response.StatusCode, http.StatusText(response.StatusCode))
	}
	contentType := response.Header.Get("Content-Type")
	mediaType, _, parseErr := mime.ParseMediaType(contentType)
	if parseErr != nil || !strings.EqualFold(mediaType, "application/json") {
		closeErr := closeResponse()
		if closeErr != nil {
			return nil, closeErr
		}
		return nil, fmt.Errorf("%s returned content type %q, want application/json", path, contentType)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxMonitoringResponseBytes+1))
	closeErr := closeResponse()
	if readErr != nil {
		return nil, fmt.Errorf("read %s response: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) > maxMonitoringResponseBytes {
		return nil, fmt.Errorf("%s response exceeds %d bytes", path, maxMonitoringResponseBytes)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("%s returned invalid JSON", path)
	}
	return json.RawMessage(body), nil
}

func decodeMonitoringInfo(path string, raw json.RawMessage) (monitoringInfoResponse, error) {
	var response monitoringInfoResponse
	if err := decodeMonitoringJSON(raw, &response); err != nil {
		return response, fmt.Errorf("%s returned invalid JSON", path)
	}
	if err := validateMonitoringInfo(path, &response); err != nil {
		return response, err
	}
	return response, nil
}

func decodeMonitoringStatus(path string, raw json.RawMessage) (monitoringStatusResponse, error) {
	var response monitoringStatusResponse
	if err := decodeMonitoringJSON(raw, &response); err != nil {
		return response, fmt.Errorf("%s returned invalid JSON", path)
	}
	if err := validateMonitoringStatus(path, &response); err != nil {
		return response, err
	}
	return response, nil
}

func decodeMonitoringJSON(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func missingMonitoringField(path, field string) error {
	return fmt.Errorf("%s is missing required field %s", path, field)
}

func invalidMonitoringField(path, field string) error {
	return fmt.Errorf("%s has invalid required field %s", path, field)
}

func validateMonitoringVersion(path string, version *int) error {
	if version == nil {
		return missingMonitoringField(path, "api_version")
	}
	if *version != 1 {
		return fmt.Errorf("%s uses unsupported API version %d", path, *version)
	}
	return nil
}

func validateMonitoringInfo(path string, response *monitoringInfoResponse) error {
	if err := validateMonitoringVersion(path, response.APIVersion); err != nil {
		return err
	}
	if response.VM == nil {
		return missingMonitoringField(path, "vm")
	}
	if response.VM.ID == nil {
		return missingMonitoringField(path, "vm.id")
	}
	if response.VM.Name == nil {
		return missingMonitoringField(path, "vm.name")
	}
	if *response.VM.ID == "" {
		return invalidMonitoringField(path, "vm.id")
	}
	if *response.VM.Name == "" {
		return invalidMonitoringField(path, "vm.name")
	}
	if response.VM.CPUs == nil {
		return missingMonitoringField(path, "vm.cpus")
	}
	if *response.VM.CPUs <= 0 {
		return invalidMonitoringField(path, "vm.cpus")
	}
	if response.VM.MemoryMiB == nil {
		return missingMonitoringField(path, "vm.memory_mib")
	}
	if *response.VM.MemoryMiB <= 0 {
		return invalidMonitoringField(path, "vm.memory_mib")
	}
	if response.QEMU == nil {
		return missingMonitoringField(path, "qemu")
	}
	if response.QEMU.Version == nil {
		return missingMonitoringField(path, "qemu.version")
	}
	for field, value := range map[string]*int{
		"qemu.version.major": response.QEMU.Version.Major,
		"qemu.version.minor": response.QEMU.Version.Minor,
		"qemu.version.micro": response.QEMU.Version.Micro,
	} {
		if value == nil {
			return missingMonitoringField(path, field)
		}
		if *value < 0 {
			return invalidMonitoringField(path, field)
		}
	}
	if response.QEMU.Version.Package == nil {
		return missingMonitoringField(path, "qemu.version.package")
	}
	if response.QEMU.Accelerator == nil {
		return missingMonitoringField(path, "qemu.accelerator")
	}
	if *response.QEMU.Accelerator == "" {
		return invalidMonitoringField(path, "qemu.accelerator")
	}
	if response.GuestAgent == nil {
		return missingMonitoringField(path, "guest_agent")
	}
	if response.GuestAgent.Configured == nil {
		return missingMonitoringField(path, "guest_agent.configured")
	}
	if response.GuestAgent.Version == nil {
		return missingMonitoringField(path, "guest_agent.version")
	}
	if response.GuestAgent.Capabilities == nil {
		return missingMonitoringField(path, "guest_agent.capabilities")
	}
	return nil
}

func validateMonitoringStatus(path string, response *monitoringStatusResponse) error {
	if err := validateMonitoringVersion(path, response.APIVersion); err != nil {
		return err
	}
	if response.ObservedAt == nil {
		return missingMonitoringField(path, "observed_at")
	}
	if response.ObservedAt.IsZero() {
		return invalidMonitoringField(path, "observed_at")
	}
	if response.Health == nil {
		return missingMonitoringField(path, "health")
	}
	if response.Health.Status == nil {
		return missingMonitoringField(path, "health.status")
	}
	if *response.Health.Status == "" {
		return invalidMonitoringField(path, "health.status")
	}
	if response.VM == nil {
		return missingMonitoringField(path, "vm")
	}
	for field, value := range map[string]*string{"vm.id": response.VM.ID, "vm.name": response.VM.Name, "vm.state": response.VM.State} {
		if value == nil {
			return missingMonitoringField(path, field)
		}
		if *value == "" {
			return invalidMonitoringField(path, field)
		}
	}
	if response.VM.StartedAt == nil {
		return missingMonitoringField(path, "vm.started_at")
	}
	if response.VM.StartedAt.IsZero() {
		return invalidMonitoringField(path, "vm.started_at")
	}
	if response.VM.UptimeSeconds == nil {
		return missingMonitoringField(path, "vm.uptime_seconds")
	}
	if *response.VM.UptimeSeconds < 0 {
		return invalidMonitoringField(path, "vm.uptime_seconds")
	}
	if response.Process == nil {
		return missingMonitoringField(path, "process")
	}
	if response.Process.StatsUp == nil {
		return missingMonitoringField(path, "process.stats_up")
	}
	if response.Process.PID == nil {
		return missingMonitoringField(path, "process.pid")
	}
	if *response.Process.PID <= 0 {
		return invalidMonitoringField(path, "process.pid")
	}
	if response.BlockDevices == nil {
		return missingMonitoringField(path, "block_devices")
	}
	if response.GuestAgent == nil {
		return missingMonitoringField(path, "guest_agent")
	}
	if response.GuestAgent.Configured == nil {
		return missingMonitoringField(path, "guest_agent.configured")
	}
	if response.GuestAgent.Up == nil {
		return missingMonitoringField(path, "guest_agent.up")
	}
	if response.Collectors == nil {
		return missingMonitoringField(path, "collectors")
	}
	return nil
}

func validateMonitoringIdentity(path string, vmID, vmName string, config *model.Config) error {
	if vmID != config.ID || vmName != config.Name {
		return fmt.Errorf("%s VM identity does not match config", path)
	}
	return nil
}

func validateAuthenticatedRun(response *monitoringStatusResponse, row StatusRow) error {
	if response.VM == nil || response.Process == nil || response.VM.StartedAt == nil || response.Process.PID == nil || row.StartedAt == nil || row.PID == nil {
		return errors.New("/status does not match the authenticated VM run")
	}
	if !response.VM.StartedAt.Equal(*row.StartedAt) || *response.Process.PID != *row.PID {
		return errors.New("/status does not match the authenticated VM run")
	}
	return nil
}

func (a *App) readMonitoring(ctx context.Context, config *model.Config, row StatusRow) (monitoringPayload, error) {
	var payload monitoringPayload
	infoRaw, err := fetchMonitoringEndpoint(ctx, a.HTTPClient, config.Metrics.Port, "/info")
	if err != nil {
		return payload, err
	}
	info, err := decodeMonitoringInfo("/info", infoRaw)
	if err != nil {
		return payload, err
	}
	if err := validateMonitoringIdentity("/info", *info.VM.ID, *info.VM.Name, config); err != nil {
		return payload, err
	}
	statusRaw, err := fetchMonitoringEndpoint(ctx, a.HTTPClient, config.Metrics.Port, "/status")
	if err != nil {
		return payload, err
	}
	status, err := decodeMonitoringStatus("/status", statusRaw)
	if err != nil {
		return payload, err
	}
	if err := validateMonitoringIdentity("/status", *status.VM.ID, *status.VM.Name, config); err != nil {
		return payload, err
	}
	if err := validateAuthenticatedRun(&status, row); err != nil {
		return payload, err
	}
	payload.infoRaw, payload.statusRaw = infoRaw, statusRaw
	payload.info, payload.status = info, status
	return payload, nil
}

func (a *App) runInfo(ctx context.Context, args []string, stdout io.Writer) error {
	name, remaining, err := nameBeforeFlags("info", args)
	if err != nil {
		return err
	}
	jsonOutput := false
	for _, argument := range remaining {
		if argument != "--json" {
			return usageErrorf("info: unknown flag %q", argument)
		}
		jsonOutput = true
	}
	config, err := a.Store.Load(name)
	if err != nil {
		return err
	}
	row, err := a.statusRow(ctx, config)
	if err != nil {
		return err
	}
	switch row.State {
	case model.RunStateStopped:
		return writeInfoInactive(stdout, jsonOutput, name, row.State, boolPtr(false))
	case model.RunStateFailed, model.RunStateStarting, model.RunStateStopping:
		return writeInfoInactive(stdout, jsonOutput, name, row.State, nil)
	case model.RunStateRunning, model.RunStatePaused:
		// Continue below; these are the only states for which monitoring is safe
		// to query.
	default:
		return writeInfoInactive(stdout, jsonOutput, name, row.State, nil)
	}
	monitoring := infoMonitoringOutput{Available: false}
	var payload monitoringPayload
	var monitoringErr error
	if config.Metrics == nil {
		monitoring.Reason = "disabled"
	} else {
		monitorCtx, cancel := context.WithTimeout(ctx, monitoringRequestTimeout)
		defer cancel()
		if row.PID == nil || row.StartedAt == nil {
			monitoringErr = errors.New("authenticated run identity is unavailable")
		} else {
			payload, monitoringErr = a.readMonitoring(monitorCtx, config, row)
		}
		if monitoringErr != nil {
			monitoring.Reason = "unavailable"
			monitoring.Error = sanitizeMonitoringError(monitoringErr)
			port := config.Metrics.Port
			monitoring.Port = &port
		} else {
			monitoring.Available = true
			port := config.Metrics.Port
			monitoring.Port = &port
			monitoring.Info = payload.infoRaw
			monitoring.Status = payload.statusRaw
		}
	}
	output := infoOutput{
		Name: name, State: row.State, Running: boolPtr(true),
		RestartRequired: &row.RestartRequired, CPUs: &row.CPUs, MemoryMiB: &row.MemoryMiB,
		Network: &row.Network, Autostart: &row.Autostart, PID: row.PID,
		StartedAt: row.StartedAt, VNC: row.VNC, Monitoring: &monitoring,
	}
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(output)
	}
	return writeInfoHuman(stdout, name, row, monitoring, payload, monitoringErr)
}

func boolPtr(value bool) *bool { return &value }

func writeInfoInactive(stdout io.Writer, jsonOutput bool, name string, state model.RunState, running *bool) error {
	if jsonOutput {
		output := infoOutput{Name: name, State: state, Running: running}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(output)
	}
	switch state {
	case model.RunStateStopped:
		_, err := fmt.Fprintf(stdout, "VM %q is not running.\n", name)
		return err
	case model.RunStateFailed:
		_, err := fmt.Fprintf(stdout, "VM %q state is failed; live information is not available.\n", name)
		return err
	default:
		_, err := fmt.Fprintf(stdout, "VM %q is %s; live information is not available.\n", name, state)
		return err
	}
}

func sanitizeMonitoringError(err error) string {
	if err == nil {
		return ""
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(err.Error()))
}

func monitoringValue(monitoring infoMonitoringOutput, name string, restartRequired bool) string {
	var value string
	switch monitoring.Reason {
	case "disabled":
		value = fmt.Sprintf("disabled. Enable it with: qemu-manage set %s --metrics-port PORT", name)
	case "unavailable":
		value = "unavailable: " + monitoring.Error
	default:
		value = fmt.Sprintf("available on 127.0.0.1:%d", *monitoring.Port)
	}
	if restartRequired {
		value += " Restart the VM to apply current configuration changes."
	}
	return value
}

func writeInfoHuman(stdout io.Writer, name string, row StatusRow, monitoring infoMonitoringOutput, payload monitoringPayload, monitoringErr error) error {
	_ = monitoringErr
	rows := [][]string{
		{"name", name},
		{"state", string(row.State)},
	}
	if row.StartedAt != nil {
		rows = append(rows, []string{"started at", row.StartedAt.UTC().Format(time.RFC3339Nano)})
	}
	rows = append(rows,
		[]string{"pid", formatPositiveInt(pointerIntValue(row.PID))},
		[]string{"resources", fmt.Sprintf("%d CPUs, %s memory", row.CPUs, formatMemoryMiB(row.MemoryMiB))},
		[]string{"network", formatOptionalString(string(row.Network))},
		[]string{"autostart", displayAutostart(row.Autostart)},
		[]string{"vnc", formatVNCEndpoint(row.VNC)},
		[]string{"restart required", strconv.FormatBool(row.RestartRequired)},
		[]string{"monitoring", monitoringValue(monitoring, name, row.RestartRequired)},
	)
	if monitoring.Available {
		rows = append(rows,
			[]string{"uptime", formatUptime(payload.status.VM.UptimeSeconds)},
			[]string{"health", formatHealth(payload.status.Health)},
			[]string{"qemu", formatQEMU(payload.info.QEMU)},
		)
		appendProcessRows(&rows, payload.status.Process)
		appendGuestRows(&rows, payload.info.GuestAgent, payload.status)
	}
	if err := writeTable(stdout, false, []string{"FIELD", "VALUE"}, rows); err != nil {
		return err
	}
	if !monitoring.Available {
		return nil
	}
	return writeMonitoringTables(stdout, payload.status.Guest, payload.status.BlockDevices)
}

func pointerIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func formatHealth(health *monitoringStatusHealth) string {
	if health == nil || health.Status == nil {
		return "-"
	}
	if health.Code == "" {
		return *health.Status
	}
	return fmt.Sprintf("%s (%s)", *health.Status, health.Code)
}

func formatQEMU(qemu *monitoringInfoQEMU) string {
	if qemu == nil || qemu.Version == nil || qemu.Accelerator == nil {
		return "-"
	}
	version := fmt.Sprintf("%d.%d.%d", *qemu.Version.Major, *qemu.Version.Minor, *qemu.Version.Micro)
	if *qemu.Version.Package != "" {
		version += " [" + *qemu.Version.Package + "]"
	}
	return fmt.Sprintf("%s (%s)", version, *qemu.Accelerator)
}

func appendProcessRows(rows *[][]string, process *monitoringStatusProcess) {
	if process == nil || process.StatsUp == nil || !*process.StatsUp {
		return
	}
	resident := "-"
	if process.ResidentMemoryBytes != nil {
		resident = formatBytes(process.ResidentMemoryBytes)
	}
	cpu := "-"
	if process.CPUSeconds != nil {
		cpu = strconv.FormatFloat(process.CPUSeconds.User+process.CPUSeconds.System, 'f', -1, 64) + "s"
	}
	threads := "-"
	if process.Threads != nil {
		threads = strconv.Itoa(*process.Threads)
	}
	*rows = append(*rows, []string{"process", fmt.Sprintf("%s resident, %s CPU, %s threads", resident, cpu, threads)})
}

func appendGuestRows(rows *[][]string, info *monitoringInfoAgent, status monitoringStatusResponse) {
	agent := "unavailable"
	if status.GuestAgent != nil && status.GuestAgent.Configured != nil && !*status.GuestAgent.Configured {
		agent = "disabled"
	} else if status.GuestAgent != nil && status.GuestAgent.Up != nil && *status.GuestAgent.Up {
		agent = "up"
	}
	if agent == "up" {
		if status.GuestAgent.Version != nil && *status.GuestAgent.Version != "" {
			agent += ", version " + *status.GuestAgent.Version
		}
		if info != nil && info.Capabilities != nil {
			enabled := make([]string, 0)
			for name, value := range *info.Capabilities {
				if value {
					enabled = append(enabled, name)
				}
			}
			sort.Strings(enabled)
			if len(enabled) != 0 {
				agent += ", capabilities " + strings.Join(enabled, ", ")
			}
		}
	}
	*rows = append(*rows, []string{"guest agent", agent})
	collectors := make([]string, 0)
	if status.Collectors != nil {
		for name, collector := range *status.Collectors {
			if collector.Status == "" || collector.Status == "ok" {
				continue
			}
			value := name + "=" + collector.Status
			if collector.Code != "" {
				value += " (" + collector.Code + ")"
			}
			collectors = append(collectors, value)
		}
	}
	sort.Strings(collectors)
	if len(collectors) != 0 {
		*rows = append(*rows, []string{"collectors", strings.Join(collectors, ", ")})
	}
	if status.Guest == nil || status.Guest.Load == nil {
		return
	}
	load := status.Guest.Load
	*rows = append(*rows, []string{"guest load", formatLoad(load.Load1) + " / " + formatLoad(load.Load5) + " / " + formatLoad(load.Load15)})
}

func formatLoad(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func writeMonitoringTables(stdout io.Writer, guest *monitoringStatusGuest, blocks *[]backend.QEMUBlockDevice) error {
	interfaceRows := make([][]string, 0)
	filesystemRows := make([][]string, 0)
	if guest != nil {
		for _, network := range guest.NetworkInterfaces {
			if network.Addresses == nil {
				continue
			}
			if len(*network.Addresses) == 0 {
				interfaceRows = append(interfaceRows, []string{network.Name, "-"})
				continue
			}
			for _, address := range *network.Addresses {
				interfaceRows = append(interfaceRows, []string{network.Name, fmt.Sprintf("%s/%d", address.Address, address.Prefix)})
			}
		}
		for _, filesystem := range guest.Filesystems {
			size, used := "-", "-"
			if filesystem.SizeBytes != nil {
				size = formatBytes(filesystem.SizeBytes)
			}
			if filesystem.UsedBytes != nil {
				used = formatBytes(filesystem.UsedBytes)
			}
			filesystemRows = append(filesystemRows, []string{filesystem.Mountpoint, filesystem.Type, used, size})
		}
	}
	if len(interfaceRows) != 0 {
		if err := writeTable(stdout, false, []string{"INTERFACE", "ADDRESS"}, interfaceRows); err != nil {
			return err
		}
	}
	if len(filesystemRows) != 0 {
		if err := writeTable(stdout, false, []string{"MOUNTPOINT", "TYPE", "USED", "SIZE"}, filesystemRows); err != nil {
			return err
		}
	}
	blockRows := make([][]string, 0)
	if blocks != nil {
		blockRows = make([][]string, 0, len(*blocks))
		for _, block := range *blocks {
			status, read, written := "-", "-", "-"
			if block.IOStatus != nil {
				status = *block.IOStatus
			}
			if block.ReadBytes != nil {
				read = formatBytes(block.ReadBytes)
			}
			if block.WriteBytes != nil {
				written = formatBytes(block.WriteBytes)
			}
			blockRows = append(blockRows, []string{block.Device, status, read, written})
		}
	}
	if len(blockRows) != 0 {
		return writeTable(stdout, false, []string{"BLOCK DEVICE", "STATUS", "READ", "WRITTEN"}, blockRows)
	}
	return nil
}

func formatBytes(value *uint64) string {
	if value == nil {
		return "-"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	scales := []float64{1, 1 << 10, 1 << 20, 1 << 30, 1 << 40}
	number := float64(*value)
	unit := 0
	for index := len(scales) - 1; index > 0; index-- {
		if number >= scales[index] {
			unit = index
			break
		}
	}
	formatted := strconv.FormatFloat(number/scales[unit], 'f', 1, 64)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + units[unit]
}

func formatUptime(value *float64) string {
	if value == nil {
		return "-"
	}
	return (time.Duration(math.Round(*value)) * time.Second).String()
}

func (a *App) runInfoCommand(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch command {
	case "showcmd":
		return a.runShowcmd(args, stdout)
	case "log":
		return a.runLog(args, stdout)
	case "status":
		return a.runStatus(ctx, args, stdout, stderr)
	case "list":
		return a.runList(ctx, args, stdout, stderr)
	case "info":
		return a.runInfo(ctx, args, stdout)
	case "delete":
		return a.runDelete(ctx, args, stdin, stdout, stderr)
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

func (a *App) runLog(args []string, stdout io.Writer) (err error) {
	name, remaining, err := nameBeforeFlags("log", args)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return usageErrorf("log %s: unexpected argument %q", name, remaining[0])
	}
	config, err := a.Store.Load(name)
	if err != nil {
		return err
	}
	file, err := openActiveSerialLog(a.Store.Paths(config).SerialLog)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("serial log: close: %w", closeErr))
		}
	}()
	if _, err := io.Copy(stdout, file); err != nil {
		return fmt.Errorf("serial log: copy to stdout: %w", err)
	}
	return nil
}

// quotePOSIX wraps a shell word in single quotes and escapes embedded single
// quotes for POSIX sh.
func quotePOSIX(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (a *App) runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
		return a.writeStatusRows(ctx, *jsonOutput, false, stdout, stderr)
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
	var row StatusRow
	collect := func() error {
		var collectErr error
		row, collectErr = a.statusRow(ctx, config)
		return collectErr
	}
	if *jsonOutput {
		err = collect()
	} else {
		err = withWaitingProgress(
			stderr,
			true,
			a.liveProgressInteractive(stderr),
			fmt.Sprintf("Reading status for %s VM", name),
			fmt.Sprintf("Loaded status for %s VM", name),
			func() error {
				return collect()
			},
		)
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(row)
	}
	if err := writeRows([]StatusRow{row}, false, stdout, a.progressInteractive(stdout)); err != nil {
		return err
	}
	if config.Network.SMBFolder != "" {
		return writeSMBMountHelp(stdout, config.Network.SMBFolder)
	}
	return nil
}

func (a *App) runList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err != nil {
			return usageErrorf("list: %v", err)
		}
		return usageErrorf("usage: qemu-manage list [--json]")
	}
	return a.writeStatusRows(ctx, *jsonOutput, true, stdout, stderr)
}

func (a *App) writeStatusRows(ctx context.Context, jsonOutput, listOutput bool, stdout, stderr io.Writer) error {
	var rows []StatusRow
	collect := func() error {
		entries, err := os.ReadDir(a.Store.DataRoot)
		if err != nil {
			return fmt.Errorf("config: list VMs: %w", err)
		}
		rows = make([]StatusRow, 0, len(entries))
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
				row.State = model.RunStateFailed
				row.Error = statusErr.Error()
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		return nil
	}
	var err error
	if jsonOutput {
		err = collect()
	} else {
		err = withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), "Reading status for all VMs", "Loaded status for all VMs", collect)
	}
	if err != nil {
		return err
	}
	if listOutput && !jsonOutput {
		return writeListRows(rows, stdout, a.progressInteractive(stdout))
	}
	return writeRows(rows, jsonOutput, stdout, a.progressInteractive(stdout))
}

func configStatusRow(config *model.Config) StatusRow {
	return StatusRow{
		Name:      config.Name,
		CPUs:      config.CPUs,
		MemoryMiB: config.MemoryMiB,
		Network:   config.Network.Mode,
		Autostart: config.Autostart.Scope,
	}
}

func (a *App) statusRow(ctx context.Context, config *model.Config) (StatusRow, error) {
	row := configStatusRow(config)
	currentHash, err := model.Hash(config)
	if err != nil {
		return row, fmt.Errorf("config: hash %q: %w", config.Name, err)
	}
	if a.Runtime == nil {
		return row, errors.New("runtime: service is unavailable")
	}
	runtimeRow, err := a.Runtime.Status(ctx, config)
	if err != nil {
		return row, fmt.Errorf("runtime: status %q: %w", config.Name, err)
	}
	row.State = runtimeRow.State
	row.PID = runtimeRow.PID
	row.Backend = runtimeRow.Backend
	row.RunningConfigSHA256 = runtimeRow.RunningConfigSHA256
	row.VNC = runtimeRow.VNC
	row.Error = runtimeRow.Error
	row.StartedAt = runtimeRow.StartedAt
	row.CurrentConfigSHA256 = currentHash
	row.RestartRequired = row.RunningConfigSHA256 != "" && row.RunningConfigSHA256 != currentHash
	return row, nil
}

func displayRunState(state model.RunState, interactive bool) string {
	text := string(state)
	if !interactive {
		return text
	}

	switch state {
	case model.RunStateRunning:
		return pterm.LightGreen(text)
	case model.RunStatePaused:
		return pterm.LightYellow(text)
	case model.RunStateStarting, model.RunStateStopping:
		return pterm.LightCyan(text)
	case model.RunStateStopped:
		return pterm.Gray(text)
	case model.RunStateFailed:
		return pterm.LightRed(text)
	default:
		return text
	}
}

func displayRestartRequired(required, interactive bool) string {
	text := fmt.Sprintf("%t", required)
	if interactive && required {
		return pterm.LightYellow(text)
	}
	return text
}

func writeRows(rows []StatusRow, jsonOutput bool, stdout io.Writer, interactive bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(rows)
	}
	tableRows := make([][]string, 0, len(rows))
	for _, row := range rows {
		tableRows = append(tableRows, []string{
			row.Name,
			displayRunState(row.State, interactive),
			displayRestartRequired(row.RestartRequired, interactive),
			row.Error,
		})
	}
	return writeTable(stdout, interactive, []string{"NAME", "STATE", "RESTART REQUIRED", "ERROR"}, tableRows)
}

func writeListRows(rows []StatusRow, stdout io.Writer, interactive bool) error {
	tableRows := make([][]string, 0, len(rows))
	for _, row := range rows {
		tableRows = append(tableRows, []string{
			row.Name,
			displayRunState(row.State, interactive),
			formatPositiveInt(row.CPUs),
			formatMemoryMiB(row.MemoryMiB),
			formatOptionalString(string(row.Network)),
			displayAutostart(row.Autostart),
			formatVNCEndpoint(row.VNC),
			displayRestartRequired(row.RestartRequired, interactive),
			row.Error,
		})
	}
	return writeTable(stdout, interactive, []string{"NAME", "STATE", "CPUS", "MEMORY", "NETWORK", "AUTOSTART", "VNC", "RESTART", "ERROR"}, tableRows)
}

func displayAutostart(scope model.AutostartScope) string {
	if scope == model.AutostartNone {
		return "disabled"
	}
	return formatOptionalString(string(scope))
}

func formatMemoryMiB(memoryMiB int) string {
	if memoryMiB <= 0 {
		return "-"
	}
	if memoryMiB%1024 == 0 {
		return fmt.Sprintf("%dGiB", memoryMiB/1024)
	}
	return fmt.Sprintf("%dMiB", memoryMiB)
}

func formatPositiveInt(value int) string {
	if value <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", value)
}

func formatOptionalString(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatVNCEndpoint(endpoint *backend.VNCEndpoint) string {
	if endpoint == nil {
		return "-"
	}
	return net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port)))
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

// terminalReader reports whether input is an interactive terminal for prompt
// gating.
func terminalReader(input io.Reader) bool {
	file, ok := input.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (a *App) runDelete(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
		stdoutInteractive := a.progressInteractive(stdout)
		if a.IsTerminal == nil || !a.IsTerminal(stdin) || !stdoutInteractive {
			return fmt.Errorf("config: deleting %q noninteractively requires --force; this permanently removes its managed disks, firmware, and configuration", name)
		}
		if err := writeMessage(stdout, stdoutInteractive, messageWarning, fmt.Sprintf("Deleting %s VM permanently removes its managed disks, firmware, and configuration.", name)); err != nil {
			return err
		}
		if a.Confirm == nil {
			return errors.New("config: deletion confirmation is unavailable")
		}
		confirmed, confirmErr := a.Confirm(fmt.Sprintf("Permanently delete %s VM?", name))
		if confirmErr != nil {
			return fmt.Errorf("config: read deletion confirmation: %w", confirmErr)
		}
		if !confirmed {
			return writeMessage(stdout, stdoutInteractive, messageInfo, "Deletion cancelled.")
		}
	}
	return withWaitingProgress(stderr, true, a.liveProgressInteractive(stderr), fmt.Sprintf("Deleting %s VM and its managed files", name), fmt.Sprintf("Deleted %s VM and its managed files", name), func() error {
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
	})
}

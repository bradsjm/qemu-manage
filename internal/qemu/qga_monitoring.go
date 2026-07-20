package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

var _ backend.MonitoringInstance = (*instance)(nil)

var guestCommands = map[string]string{
	"cpu": "guest-get-cpustats", "load": "guest-get-load", "vcpus": "guest-get-vcpus",
	"clock": "guest-get-time", "filesystem_freeze": "guest-fsfreeze-status",
	"filesystems": "guest-get-fsinfo", "network": "guest-network-get-interfaces", "disk": "guest-get-diskstats",
}

func (i *instance) CollectGuest(ctx context.Context) backend.GuestObservation {
	observation := backend.GuestObservation{Results: make(map[string]backend.ObservationResult)}
	if !i.useQGA {
		observation.Results["info"] = backend.ObservationResult{Code: "guest_agent_not_configured", Err: errors.New("guest agent is not configured")}
		return observation
	}
	infoRaw, err := i.qgaCommand(ctx, GuestAgentRequest{Execute: "guest-info"})
	if err != nil {
		observation.Results["info"] = qgaObservationResult(err)
		return observation
	}
	info, err := decodeGuestInfo(infoRaw)
	if err != nil {
		observation.Results["info"] = backend.ObservationResult{Code: "guest_agent_protocol_error", Err: err}
		return observation
	}
	observation.Info = info
	keys := make([]string, 0, len(guestCommands))
	for key := range guestCommands {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command := guestCommands[key]
		enabled, known := info.Capabilities[command]
		if !known || !enabled {
			observation.Results[key] = backend.ObservationResult{Code: "guest_agent_command_disabled"}
			continue
		}
		started := time.Now()
		raw, callErr := i.qgaCommand(ctx, GuestAgentRequest{Execute: command})
		finished := time.Now()
		if callErr != nil {
			observation.Results[key] = qgaObservationResult(callErr)
			continue
		}
		midpoint := started.Add(finished.Sub(started) / 2)
		if decodeErr := decodeGuestFamily(key, raw, midpoint, &observation); decodeErr != nil {
			observation.Results[key] = backend.ObservationResult{Code: "guest_agent_protocol_error", Err: decodeErr}
		}
		if _, exists := observation.Results[key]; !exists {
			observation.Results[key] = backend.ObservationResult{}
		}
	}
	return observation
}

func (i *instance) PingGuest(ctx context.Context) backend.GuestProbe {
	if !i.useQGA {
		return backend.GuestProbe{Result: backend.ObservationResult{Code: "guest_agent_not_configured", Err: errors.New("guest agent is not configured")}}
	}
	raw, err := i.qgaCommand(ctx, GuestAgentRequest{Execute: "guest-ping"})
	if err != nil {
		result := qgaObservationResult(err)
		if result.Code == "guest_agent_command_failed" {
			result.Code = "guest_agent_unavailable"
		}
		return backend.GuestProbe{Result: result}
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil || response == nil {
		return backend.GuestProbe{Result: backend.ObservationResult{Code: "guest_agent_protocol_error", Err: errors.New("guest-ping response must be an object")}}
	}
	return backend.GuestProbe{}
}

func qgaObservationResult(err error) backend.ObservationResult {
	if err == nil {
		return backend.ObservationResult{}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return backend.ObservationResult{Code: "guest_agent_timeout", Err: err}
	}
	var qgaErr *QGAError
	if errors.As(err, &qgaErr) {
		return backend.ObservationResult{Code: "guest_agent_command_failed", Err: err}
	}
	return backend.ObservationResult{Code: "guest_agent_unavailable", Err: err}
}

func decodeGuestInfo(raw json.RawMessage) (backend.GuestInfo, error) {
	var response struct {
		Version  *string `json:"version"`
		Commands *[]struct {
			Name    *string `json:"name"`
			Enabled *bool   `json:"enabled"`
		} `json:"supported_commands"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Version == nil || response.Commands == nil {
		return backend.GuestInfo{}, errors.New("guest-info response is missing required fields")
	}
	capabilities := make(map[string]bool, len(*response.Commands))
	for _, command := range *response.Commands {
		if command.Name == nil || *command.Name == "" || command.Enabled == nil {
			return backend.GuestInfo{}, errors.New("guest-info supported command is malformed")
		}
		capabilities[*command.Name] = *command.Enabled
	}
	return backend.GuestInfo{Version: *response.Version, Capabilities: capabilities}, nil
}

func decodeGuestFamily(key string, raw json.RawMessage, midpoint time.Time, observation *backend.GuestObservation) error {
	switch key {
	case "cpu":
		value, err := decodeGuestCPU(raw)
		observation.CPU = value
		return err
	case "load":
		value, err := decodeGuestLoad(raw)
		observation.Load = value
		return err
	case "vcpus":
		value, err := decodeGuestVCPUs(raw)
		observation.VCPUs = value
		return err
	case "clock":
		var nanoseconds int64
		if err := json.Unmarshal(raw, &nanoseconds); err != nil || nanoseconds < 0 {
			return errors.New("guest-get-time response must be a nonnegative integer")
		}
		offset := float64(nanoseconds-midpoint.UTC().UnixNano()) / 1e9
		observation.ClockOffset = &offset
		return nil
	case "filesystem_freeze":
		var status string
		if err := json.Unmarshal(raw, &status); err != nil || (status != "frozen" && status != "thawed") {
			return errors.New("guest-fsfreeze-status response is invalid")
		}
		frozen := status == "frozen"
		observation.Frozen = &frozen
		return nil
	case "filesystems":
		value, err := decodeGuestFilesystems(raw)
		observation.Filesystems = value
		return err
	case "network":
		value, err := decodeGuestNetworks(raw)
		observation.Networks = value
		return err
	case "disk":
		value, err := decodeGuestDisks(raw)
		observation.Disks = value
		return err
	default:
		return errors.New("unknown guest collector")
	}
}

func decodeGuestCPU(raw json.RawMessage) ([]backend.GuestCPU, error) {
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, errors.New("guest-get-cpustats response must be an array")
	}
	result := make([]backend.GuestCPU, 0, len(records))
	for _, record := range records {
		var cpu int
		cpuRaw, ok := record["cpu"]
		if !ok || json.Unmarshal(cpuRaw, &cpu) != nil || cpu < 0 {
			return nil, errors.New("guest CPU has invalid cpu")
		}
		seconds := make(map[string]float64)
		for _, mode := range []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal", "guest", "guestnice"} {
			valueRaw, present := record[mode]
			if !present {
				continue
			}
			var milliseconds int64
			if json.Unmarshal(valueRaw, &milliseconds) != nil || milliseconds < 0 {
				return nil, fmt.Errorf("guest CPU %s must be nonnegative", mode)
			}
			seconds[mode] = float64(milliseconds) / 1000
		}
		result = append(result, backend.GuestCPU{CPU: cpu, Seconds: seconds})
	}
	sort.Slice(result, func(a, b int) bool { return result[a].CPU < result[b].CPU })
	return result, nil
}

func decodeGuestLoad(raw json.RawMessage) (*backend.GuestLoad, error) {
	var record struct{ Load1, Load5, Load15 *float64 }
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("guest-get-load response must be an object")
	}
	for name, target := range map[string]**float64{"load1": &record.Load1, "load5": &record.Load5, "load15": &record.Load15} {
		value, ok := fields[name]
		if !ok {
			continue
		}
		var decoded float64
		if json.Unmarshal(value, &decoded) != nil || decoded < 0 {
			return nil, fmt.Errorf("%s must be nonnegative", name)
		}
		*target = &decoded
	}
	return &backend.GuestLoad{Load1: record.Load1, Load5: record.Load5, Load15: record.Load15}, nil
}

func decodeGuestVCPUs(raw json.RawMessage) ([]backend.GuestVCPU, error) {
	var records []struct {
		LogicalID *int  `json:"logical-id"`
		Online    *bool `json:"online"`
	}
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, errors.New("guest-get-vcpus response must be an array")
	}
	result := make([]backend.GuestVCPU, 0, len(records))
	for _, record := range records {
		if record.LogicalID == nil || *record.LogicalID < 0 || record.Online == nil {
			return nil, errors.New("guest vCPU is missing required fields")
		}
		result = append(result, backend.GuestVCPU{LogicalID: *record.LogicalID, Online: *record.Online})
	}
	sort.Slice(result, func(a, b int) bool { return result[a].LogicalID < result[b].LogicalID })
	return result, nil
}

func decodeGuestFilesystems(raw json.RawMessage) ([]backend.GuestFilesystem, error) {
	var records []struct {
		Mountpoint *string `json:"mountpoint"`
		Type       *string `json:"type"`
		Total      *int64  `json:"total-bytes"`
		Used       *int64  `json:"used-bytes"`
	}
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, errors.New("guest-get-fsinfo response must be an array")
	}
	result := make([]backend.GuestFilesystem, 0, len(records))
	for _, record := range records {
		if record.Mountpoint == nil || record.Type == nil {
			return nil, errors.New("guest filesystem is missing required fields")
		}
		total, err := optionalUint64("total-bytes", record.Total)
		if err != nil {
			return nil, err
		}
		used, err := optionalUint64("used-bytes", record.Used)
		if err != nil {
			return nil, err
		}
		result = append(result, backend.GuestFilesystem{Mountpoint: *record.Mountpoint, Type: *record.Type, SizeBytes: total, UsedBytes: used})
	}
	sort.Slice(result, func(a, b int) bool {
		if result[a].Mountpoint != result[b].Mountpoint {
			return result[a].Mountpoint < result[b].Mountpoint
		}
		return result[a].Type < result[b].Type
	})
	return result, nil
}

func decodeGuestNetworks(raw json.RawMessage) ([]backend.GuestNetworkInterface, error) {
	var records []struct {
		Name       *string         `json:"name"`
		Addresses  json.RawMessage `json:"ip-addresses"`
		Statistics *struct {
			RXBytes   *int64 `json:"rx-bytes"`
			TXBytes   *int64 `json:"tx-bytes"`
			RXPackets *int64 `json:"rx-packets"`
			TXPackets *int64 `json:"tx-packets"`
			RXErrors  *int64 `json:"rx-errs"`
			TXErrors  *int64 `json:"tx-errs"`
			RXDropped *int64 `json:"rx-dropped"`
			TXDropped *int64 `json:"tx-dropped"`
		} `json:"statistics"`
	}
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, errors.New("guest-network-get-interfaces response must be an array")
	}
	result := make([]backend.GuestNetworkInterface, 0, len(records))
	for _, record := range records {
		if record.Name == nil || *record.Name == "" {
			return nil, errors.New("guest network interface has invalid name")
		}
		item := backend.GuestNetworkInterface{Name: *record.Name, AddressesPresent: record.Addresses != nil}
		if record.Addresses != nil {
			addresses, err := decodeGuestAddresses(record.Addresses)
			if err != nil {
				return nil, err
			}
			item.Addresses = addresses
		}
		if record.Statistics != nil {
			var err error
			if item.ReceiveBytes, err = optionalUint64("rx-bytes", record.Statistics.RXBytes); err != nil {
				return nil, err
			}
			if item.TransmitBytes, err = optionalUint64("tx-bytes", record.Statistics.TXBytes); err != nil {
				return nil, err
			}
			if item.ReceivePackets, err = optionalUint64("rx-packets", record.Statistics.RXPackets); err != nil {
				return nil, err
			}
			if item.TransmitPackets, err = optionalUint64("tx-packets", record.Statistics.TXPackets); err != nil {
				return nil, err
			}
			if item.ReceiveErrors, err = optionalUint64("rx-errs", record.Statistics.RXErrors); err != nil {
				return nil, err
			}
			if item.TransmitErrors, err = optionalUint64("tx-errs", record.Statistics.TXErrors); err != nil {
				return nil, err
			}
			if item.ReceiveDropped, err = optionalUint64("rx-dropped", record.Statistics.RXDropped); err != nil {
				return nil, err
			}
			if item.TransmitDropped, err = optionalUint64("tx-dropped", record.Statistics.TXDropped); err != nil {
				return nil, err
			}
		}
		if record.Statistics != nil || record.Addresses != nil {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Name < result[b].Name })
	return result, nil
}

func decodeGuestAddresses(raw json.RawMessage) ([]backend.GuestIPAddress, error) {
	var records []struct {
		Address *string `json:"ip-address"`
		Type    *string `json:"ip-address-type"`
		Prefix  *int    `json:"prefix"`
	}
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, errors.New("guest ip-addresses must be an array")
	}
	seen := make(map[backend.GuestIPAddress]struct{}, len(records))
	result := make([]backend.GuestIPAddress, 0, len(records))
	for _, record := range records {
		if record.Address == nil || record.Type == nil || record.Prefix == nil {
			return nil, errors.New("guest IP address is missing required fields")
		}
		address, err := netip.ParseAddr(*record.Address)
		if err != nil {
			return nil, errors.New("guest IP address is invalid")
		}
		family, maximum := "ipv6", 128
		if address.Is4() {
			family, maximum = "ipv4", 32
		}
		if *record.Type != family || *record.Prefix < 0 || *record.Prefix > maximum {
			return nil, errors.New("guest IP address family or prefix is invalid")
		}
		item := backend.GuestIPAddress{Address: address.String(), Family: family, Prefix: *record.Prefix}
		if _, ok := seen[item]; !ok {
			seen[item] = struct{}{}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(a, b int) bool {
		if result[a].Family != result[b].Family {
			return result[a].Family < result[b].Family
		}
		aa, bb := netip.MustParseAddr(result[a].Address), netip.MustParseAddr(result[b].Address)
		if aa != bb {
			return aa.Less(bb)
		}
		return result[a].Prefix < result[b].Prefix
	})
	return result, nil
}

func decodeGuestDisks(raw json.RawMessage) ([]backend.GuestDisk, error) {
	var records []struct {
		Name  *string                     `json:"name"`
		Stats *map[string]json.RawMessage `json:"stats"`
	}
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, errors.New("guest-get-diskstats response must be an array")
	}
	result := make([]backend.GuestDisk, 0, len(records))
	for _, record := range records {
		if record.Name == nil || !qmpDeviceLabelPattern.MatchString(*record.Name) || record.Stats == nil {
			return nil, errors.New("guest disk is missing required fields")
		}
		disk := backend.GuestDisk{Device: *record.Name}
		fields := *record.Stats
		uintFields := map[string]**uint64{"read-sectors": &disk.ReadSectors, "write-sectors": &disk.WriteSectors, "discard-sectors": &disk.DiscardSectors, "read-ios": &disk.ReadOperations, "write-ios": &disk.WriteOperations, "discard-ios": &disk.DiscardOperations, "flush-ios": &disk.FlushOperations, "read-merges": &disk.ReadMergedOperations, "write-merges": &disk.WriteMergedOperations, "discard-merges": &disk.DiscardMergedOperations, "ios-progress": &disk.IOInFlight}
		for name, target := range uintFields {
			value, ok := fields[name]
			if !ok {
				continue
			}
			var rawValue int64
			if json.Unmarshal(value, &rawValue) != nil {
				return nil, fmt.Errorf("guest disk %s is invalid", name)
			}
			normalized, err := optionalUint64(name, &rawValue)
			if err != nil {
				return nil, err
			}
			*target = normalized
		}
		secondFields := map[string]**float64{"read-ticks": &disk.ReadSeconds, "write-ticks": &disk.WriteSeconds, "discard-ticks": &disk.DiscardSeconds, "flush-ticks": &disk.FlushSeconds, "weighted-times": &disk.WeightedIOSeconds}
		for name, target := range secondFields {
			value, ok := fields[name]
			if !ok {
				continue
			}
			var milliseconds int64
			if json.Unmarshal(value, &milliseconds) != nil || milliseconds < 0 {
				return nil, fmt.Errorf("guest disk %s must be nonnegative", name)
			}
			seconds := float64(milliseconds) / 1000
			*target = &seconds
		}
		result = append(result, disk)
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Device < result[b].Device })
	return result, nil
}

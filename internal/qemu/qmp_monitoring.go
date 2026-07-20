package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

// qmpBlockStats captures the raw counters returned by query-blockstats before
// they are normalized into backend.QEMUBlockDevice values.
type qmpBlockStats struct {
	ReadBytes              *int64 `json:"rd_bytes"`
	WriteBytes             *int64 `json:"wr_bytes"`
	UnmapBytes             *int64 `json:"unmap_bytes"`
	ReadOperations         *int64 `json:"rd_operations"`
	WriteOperations        *int64 `json:"wr_operations"`
	FlushOperations        *int64 `json:"flush_operations"`
	UnmapOperations        *int64 `json:"unmap_operations"`
	ReadTimeNS             *int64 `json:"rd_total_time_ns"`
	WriteTimeNS            *int64 `json:"wr_total_time_ns"`
	FlushTimeNS            *int64 `json:"flush_total_time_ns"`
	UnmapTimeNS            *int64 `json:"unmap_total_time_ns"`
	FailedReadOperations   *int64 `json:"failed_rd_operations"`
	FailedWriteOperations  *int64 `json:"failed_wr_operations"`
	FailedFlushOperations  *int64 `json:"failed_flush_operations"`
	FailedUnmapOperations  *int64 `json:"failed_unmap_operations"`
	InvalidReadOperations  *int64 `json:"invalid_rd_operations"`
	InvalidWriteOperations *int64 `json:"invalid_wr_operations"`
	InvalidFlushOperations *int64 `json:"invalid_flush_operations"`
	InvalidUnmapOperations *int64 `json:"invalid_unmap_operations"`
}

// qmpBlockStatsRecord is one query-blockstats array entry keyed by device
// label.
type qmpBlockStatsRecord struct {
	Device     string         `json:"device"`
	Stats      *qmpBlockStats `json:"stats"`
	IdleTimeNS *int64         `json:"idle_time_ns"`
}

// QueryBlocks returns normalized block-device counters merged from
// query-blockstats and query-block.
func (c *QMPClient) QueryBlocks(ctx context.Context) ([]backend.QEMUBlockDevice, error) {
	statsResult, err := c.execute(ctx, "query-blockstats", nil)
	if err != nil {
		return nil, err
	}
	var records []qmpBlockStatsRecord
	if err := json.Unmarshal(statsResult, &records); err != nil {
		return nil, fmt.Errorf("decode query-blockstats response: %w", err)
	}
	if records == nil {
		return nil, errors.New("query-blockstats response must be an array")
	}

	devices := make(map[string]*backend.QEMUBlockDevice, len(records))
	for _, record := range records {
		if record.Device == "" {
			continue
		}
		if !qmpDeviceLabelPattern.MatchString(record.Device) {
			return nil, fmt.Errorf("query-blockstats response has invalid device %q", record.Device)
		}
		if record.Stats == nil {
			return nil, fmt.Errorf("query-blockstats response for %q is missing stats", record.Device)
		}
		device, err := normalizeBlockStats(record)
		if err != nil {
			return nil, fmt.Errorf("query-blockstats response for %q: %w", record.Device, err)
		}
		devices[record.Device] = &device
	}

	blockResult, err := c.execute(ctx, "query-block", nil)
	if err != nil {
		return nil, err
	}
	var blockRecords []struct {
		Device   string  `json:"device"`
		IOStatus *string `json:"io-status"`
	}
	if err := json.Unmarshal(blockResult, &blockRecords); err != nil {
		return nil, fmt.Errorf("decode query-block response: %w", err)
	}
	if blockRecords == nil {
		return nil, errors.New("query-block response must be an array")
	}
	for _, record := range blockRecords {
		if record.Device == "" {
			continue
		}
		if !qmpDeviceLabelPattern.MatchString(record.Device) {
			return nil, fmt.Errorf("query-block response has invalid device %q", record.Device)
		}
		if device := devices[record.Device]; device != nil && record.IOStatus != nil {
			if *record.IOStatus == "" {
				return nil, fmt.Errorf("query-block response for %q has empty io-status", record.Device)
			}
			status := *record.IOStatus
			device.IOStatus = &status
		}
	}

	result := make([]backend.QEMUBlockDevice, 0, len(devices))
	for _, device := range devices {
		result = append(result, *device)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Device < result[j].Device })
	return result, nil
}

func normalizeBlockStats(record qmpBlockStatsRecord) (backend.QEMUBlockDevice, error) {
	stats := record.Stats
	device := backend.QEMUBlockDevice{
		Device:            record.Device,
		FailedOperations:  make(map[string]uint64),
		InvalidOperations: make(map[string]uint64),
	}
	var err error
	if device.ReadBytes, err = optionalUint64("rd_bytes", stats.ReadBytes); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.WriteBytes, err = optionalUint64("wr_bytes", stats.WriteBytes); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.UnmapBytes, err = optionalUint64("unmap_bytes", stats.UnmapBytes); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.ReadOperations, err = optionalUint64("rd_operations", stats.ReadOperations); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.WriteOperations, err = optionalUint64("wr_operations", stats.WriteOperations); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.FlushOperations, err = optionalUint64("flush_operations", stats.FlushOperations); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.UnmapOperations, err = optionalUint64("unmap_operations", stats.UnmapOperations); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.ReadSeconds, err = optionalNanoseconds("rd_total_time_ns", stats.ReadTimeNS); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.WriteSeconds, err = optionalNanoseconds("wr_total_time_ns", stats.WriteTimeNS); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.FlushSeconds, err = optionalNanoseconds("flush_total_time_ns", stats.FlushTimeNS); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.UnmapSeconds, err = optionalNanoseconds("unmap_total_time_ns", stats.UnmapTimeNS); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	if device.IdleSeconds, err = optionalNanoseconds("idle_time_ns", record.IdleTimeNS); err != nil {
		return backend.QEMUBlockDevice{}, err
	}
	for operation, value := range map[string]*int64{"read": stats.FailedReadOperations, "write": stats.FailedWriteOperations, "flush": stats.FailedFlushOperations, "unmap": stats.FailedUnmapOperations} {
		if value != nil {
			normalized, conversionErr := optionalUint64("failed_"+operation+"_operations", value)
			if conversionErr != nil {
				return backend.QEMUBlockDevice{}, conversionErr
			}
			device.FailedOperations[operation] = *normalized
		}
	}
	for operation, value := range map[string]*int64{"read": stats.InvalidReadOperations, "write": stats.InvalidWriteOperations, "flush": stats.InvalidFlushOperations, "unmap": stats.InvalidUnmapOperations} {
		if value != nil {
			normalized, conversionErr := optionalUint64("invalid_"+operation+"_operations", value)
			if conversionErr != nil {
				return backend.QEMUBlockDevice{}, conversionErr
			}
			device.InvalidOperations[operation] = *normalized
		}
	}
	return device, nil
}

// optionalUint64 converts optional nonnegative signed counters to the unsigned
// form used by backend telemetry.
func optionalUint64(field string, value *int64) (*uint64, error) {
	if value == nil {
		return nil, nil
	}
	if *value < 0 {
		return nil, fmt.Errorf("%s must be nonnegative", field)
	}
	normalized := uint64(*value)
	return &normalized, nil
}

// optionalNanoseconds converts optional QMP nanosecond counters to fractional
// seconds for API output.
func optionalNanoseconds(field string, value *int64) (*float64, error) {
	normalized, err := optionalUint64(field, value)
	if err != nil || normalized == nil {
		return nil, err
	}
	seconds := float64(*normalized) / 1e9
	return &seconds, nil
}

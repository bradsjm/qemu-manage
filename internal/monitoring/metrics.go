package monitoring

import (
	"sort"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

// metricDefinition describes one metric family emitted by the Prometheus renderer
type metricDefinition struct{ name, help, kind string }

var metricDefinitions = []metricDefinition{
	{"qemu_manage_qmp_up", "Whether the QMP collector has a fresh observation.", "gauge"},
	{"qemu_manage_vm_state_info", "Current raw QEMU run state.", "gauge"},
	{"qemu_manage_vm_uptime_seconds", "Seconds since this supervisor started the VM.", "gauge"},
	{"qemu_manage_qmp_events_total", "QMP lifecycle events observed by this supervisor.", "counter"},
	{"qemu_manage_qemu_process_stats_up", "Whether macOS process statistics are fresh.", "gauge"},
	{"qemu_manage_qemu_process_cpu_seconds_total", "QEMU child CPU time in seconds.", "counter"},
	{"qemu_manage_qemu_process_resident_memory_bytes", "QEMU child resident memory in bytes.", "gauge"},
	{"qemu_manage_qemu_process_physical_footprint_bytes", "QEMU child physical footprint in bytes.", "gauge"},
	{"qemu_manage_qemu_process_physical_footprint_peak_bytes", "QEMU child lifetime peak physical footprint in bytes.", "gauge"},
	{"qemu_manage_qemu_process_wired_memory_bytes", "QEMU child wired memory in bytes.", "gauge"},
	{"qemu_manage_qemu_process_threads", "QEMU child thread count.", "gauge"},
	{"qemu_manage_qemu_process_disk_read_bytes_total", "Bytes read by the QEMU child.", "counter"},
	{"qemu_manage_qemu_process_disk_written_bytes_total", "Bytes written by the QEMU child.", "counter"},
	{"qemu_manage_qemu_process_pageins_total", "Page-ins by the QEMU child.", "counter"},
	{"qemu_manage_qemu_process_idle_wakeups_total", "Package idle wakeups by the QEMU child.", "counter"},
	{"qemu_manage_qemu_process_interrupt_wakeups_total", "Interrupt wakeups by the QEMU child.", "counter"},
	{"qemu_manage_qemu_process_instructions_total", "Instructions executed by the QEMU child.", "counter"},
	{"qemu_manage_qemu_process_cycles_total", "CPU cycles used by the QEMU child.", "counter"},
	{"qemu_manage_block_io_bytes_total", "QMP block I/O bytes.", "counter"},
	{"qemu_manage_block_io_operations_total", "QMP block I/O operations.", "counter"},
	{"qemu_manage_block_io_seconds_total", "QMP block I/O duration in seconds.", "counter"},
	{"qemu_manage_block_failed_operations_total", "QMP failed block operations.", "counter"},
	{"qemu_manage_block_invalid_operations_total", "QMP invalid block operations.", "counter"},
	{"qemu_manage_block_idle_seconds", "QMP block device idle duration in seconds.", "gauge"},
	{"qemu_manage_block_io_status_info", "QMP block device I/O status.", "gauge"},
	{"qemu_manage_block_io_errors_total", "Structured QMP block I/O error events.", "counter"},
	{"qemu_manage_guest_agent_configured", "Whether the QEMU guest agent is configured.", "gauge"},
	{"qemu_manage_guest_agent_up", "Whether the guest agent has a fresh successful observation.", "gauge"},
	{"qemu_manage_guest_agent_probe_duration_seconds", "Duration of the latest completed guest-agent probe.", "gauge"},
	{"qemu_manage_guest_cpu_seconds_total", "Guest CPU time in seconds.", "counter"},
	{"qemu_manage_guest_load1", "Guest one-minute load average.", "gauge"},
	{"qemu_manage_guest_load5", "Guest five-minute load average.", "gauge"},
	{"qemu_manage_guest_load15", "Guest fifteen-minute load average.", "gauge"},
	{"qemu_manage_guest_vcpus", "Guest vCPU count by online state.", "gauge"},
	{"qemu_manage_guest_filesystem_size_bytes", "Guest filesystem size in bytes.", "gauge"},
	{"qemu_manage_guest_filesystem_used_bytes", "Guest filesystem used bytes.", "gauge"},
	{"qemu_manage_guest_network_receive_bytes_total", "Guest network bytes received.", "counter"},
	{"qemu_manage_guest_network_transmit_bytes_total", "Guest network bytes transmitted.", "counter"},
	{"qemu_manage_guest_network_receive_packets_total", "Guest network packets received.", "counter"},
	{"qemu_manage_guest_network_transmit_packets_total", "Guest network packets transmitted.", "counter"},
	{"qemu_manage_guest_network_receive_errors_total", "Guest network receive errors.", "counter"},
	{"qemu_manage_guest_network_transmit_errors_total", "Guest network transmit errors.", "counter"},
	{"qemu_manage_guest_network_receive_dropped_total", "Guest network receive drops.", "counter"},
	{"qemu_manage_guest_network_transmit_dropped_total", "Guest network transmit drops.", "counter"},
	{"qemu_manage_guest_disk_sectors_total", "Guest disk sectors processed.", "counter"},
	{"qemu_manage_guest_disk_operations_total", "Guest disk operations.", "counter"},
	{"qemu_manage_guest_disk_merged_operations_total", "Guest merged disk operations.", "counter"},
	{"qemu_manage_guest_disk_io_seconds_total", "Guest disk I/O duration in seconds.", "counter"},
	{"qemu_manage_guest_disk_weighted_io_seconds_total", "Guest weighted disk I/O duration in seconds.", "counter"},
	{"qemu_manage_guest_disk_io_in_flight", "Guest disk operations currently in flight.", "gauge"},
	{"qemu_manage_guest_clock_offset_seconds", "Guest clock offset from the host midpoint in seconds.", "gauge"},
	{"qemu_manage_guest_filesystems_frozen", "Whether guest filesystems are frozen.", "gauge"},
}

// renderMetrics renders the snapshot in Prometheus text exposition format 0.0.4
func renderMetrics(snapshot *Snapshot) []byte {
	var headers, output strings.Builder
	for _, definition := range metricDefinitions {
		headers.WriteString(metricHeader(definition.name, definition.help, definition.kind))
	}
	qmpUp := snapshot.Collectors["qmp"].Status == CollectorOK
	output.WriteString(metricSample("qemu_manage_qmp_up", nil, boolMetric(qmpUp)))
	state := snapshot.QMP.State
	if state == "" {
		state = "unknown"
	}
	output.WriteString(metricSample("qemu_manage_vm_state_info", map[string]string{"state": state}, "1"))
	output.WriteString(metricSample("qemu_manage_vm_uptime_seconds", nil, metricFloat(nonnegativeSeconds(snapshot.ObservedAt.Sub(snapshot.VM.StartedAt)))))
	if qmpUp {
		events := make([]string, 0, len(snapshot.QMP.Events.Lifecycle))
		for event := range snapshot.QMP.Events.Lifecycle {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			output.WriteString(metricSample("qemu_manage_qmp_events_total", map[string]string{"event": event}, metricUint(snapshot.QMP.Events.Lifecycle[event])))
		}
		for _, event := range snapshot.QMP.Events.BlockIO {
			output.WriteString(metricSample("qemu_manage_block_io_errors_total", map[string]string{"device": event.Device, "operation": event.Operation, "nospace": boolString(event.NoSpace)}, metricUint(event.Count)))
		}
	}
	renderProcessMetrics(&output, snapshot)
	if snapshot.Collectors["block"].Status == CollectorOK {
		renderBlockMetrics(&output, snapshot)
	}
	renderGuestMetrics(&output, snapshot)
	samples := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	sort.Strings(samples)
	return []byte(headers.String() + strings.Join(samples, "\n") + "\n")
}

func renderProcessMetrics(output *strings.Builder, snapshot *Snapshot) {
	up := snapshot.Collectors["process"].Status == CollectorOK
	output.WriteString(metricSample("qemu_manage_qemu_process_stats_up", nil, boolMetric(up)))
	if !up {
		return
	}
	stats := snapshot.Process.Stats
	output.WriteString(metricSample("qemu_manage_qemu_process_cpu_seconds_total", map[string]string{"mode": "user"}, metricFloat(stats.UserCPUSeconds)))
	output.WriteString(metricSample("qemu_manage_qemu_process_cpu_seconds_total", map[string]string{"mode": "system"}, metricFloat(stats.SystemCPUSeconds)))
	for name, value := range map[string]uint64{
		"qemu_manage_qemu_process_resident_memory_bytes": stats.ResidentMemoryBytes, "qemu_manage_qemu_process_physical_footprint_bytes": stats.PhysicalFootprintBytes,
		"qemu_manage_qemu_process_physical_footprint_peak_bytes": stats.PhysicalFootprintPeakBytes, "qemu_manage_qemu_process_wired_memory_bytes": stats.WiredMemoryBytes,
		"qemu_manage_qemu_process_threads": stats.Threads, "qemu_manage_qemu_process_disk_read_bytes_total": stats.DiskReadBytes,
		"qemu_manage_qemu_process_disk_written_bytes_total": stats.DiskWrittenBytes, "qemu_manage_qemu_process_pageins_total": stats.PageIns,
		"qemu_manage_qemu_process_idle_wakeups_total": stats.IdleWakeups, "qemu_manage_qemu_process_interrupt_wakeups_total": stats.InterruptWakeups,
		"qemu_manage_qemu_process_instructions_total": stats.Instructions, "qemu_manage_qemu_process_cycles_total": stats.Cycles,
	} {
		output.WriteString(metricSample(name, nil, metricUint(value)))
	}
}

func renderBlockMetrics(output *strings.Builder, snapshot *Snapshot) {
	for _, device := range snapshot.QMP.Blocks {
		for direction, sample := range map[string]*uint64{"read": device.ReadBytes, "write": device.WriteBytes, "unmap": device.UnmapBytes} {
			if sample != nil {
				output.WriteString(metricSample("qemu_manage_block_io_bytes_total", map[string]string{"device": device.Device, "direction": direction}, metricUint(*sample)))
			}
		}
		for operation, sample := range map[string]*uint64{"read": device.ReadOperations, "write": device.WriteOperations, "flush": device.FlushOperations, "unmap": device.UnmapOperations} {
			if sample != nil {
				output.WriteString(metricSample("qemu_manage_block_io_operations_total", map[string]string{"device": device.Device, "operation": operation}, metricUint(*sample)))
			}
		}
		for operation, sample := range map[string]*float64{"read": device.ReadSeconds, "write": device.WriteSeconds, "flush": device.FlushSeconds, "unmap": device.UnmapSeconds} {
			if sample != nil {
				output.WriteString(metricSample("qemu_manage_block_io_seconds_total", map[string]string{"device": device.Device, "operation": operation}, metricFloat(*sample)))
			}
		}
		for operation, value := range device.FailedOperations {
			output.WriteString(metricSample("qemu_manage_block_failed_operations_total", map[string]string{"device": device.Device, "operation": operation}, metricUint(value)))
		}
		for operation, value := range device.InvalidOperations {
			output.WriteString(metricSample("qemu_manage_block_invalid_operations_total", map[string]string{"device": device.Device, "operation": operation}, metricUint(value)))
		}
		if device.IdleSeconds != nil {
			output.WriteString(metricSample("qemu_manage_block_idle_seconds", map[string]string{"device": device.Device}, metricFloat(*device.IdleSeconds)))
		}
		if device.IOStatus != nil {
			output.WriteString(metricSample("qemu_manage_block_io_status_info", map[string]string{"device": device.Device, "status": *device.IOStatus}, "1"))
		}
	}
}

func renderGuestMetrics(output *strings.Builder, snapshot *Snapshot) {
	configured := snapshot.VM.GuestAgent
	infoUp := snapshot.Collectors["guest_info"].Status == CollectorOK
	output.WriteString(metricSample("qemu_manage_guest_agent_configured", nil, boolMetric(configured)))
	output.WriteString(metricSample("qemu_manage_guest_agent_up", nil, boolMetric(infoUp)))
	if !snapshot.Guest.ProbeAt.IsZero() {
		output.WriteString(metricSample("qemu_manage_guest_agent_probe_duration_seconds", nil, metricFloat(snapshot.Guest.ProbeDuration.Seconds())))
	}
	guest := snapshot.Guest.Observation
	if collectorOK(snapshot, "guest_cpu") {
		for _, cpu := range guest.CPU {
			for mode, seconds := range cpu.Seconds {
				output.WriteString(metricSample("qemu_manage_guest_cpu_seconds_total", map[string]string{"cpu": metricUint(uint64(cpu.CPU)), "mode": mode}, metricFloat(seconds)))
			}
		}
	}
	if collectorOK(snapshot, "guest_load") && guest.Load != nil {
		for name, value := range map[string]*float64{"qemu_manage_guest_load1": guest.Load.Load1, "qemu_manage_guest_load5": guest.Load.Load5, "qemu_manage_guest_load15": guest.Load.Load15} {
			if value != nil {
				output.WriteString(metricSample(name, nil, metricFloat(*value)))
			}
		}
	}
	if collectorOK(snapshot, "guest_vcpus") {
		online := uint64(0)
		for _, cpu := range guest.VCPUs {
			if cpu.Online {
				online++
			}
		}
		output.WriteString(metricSample("qemu_manage_guest_vcpus", map[string]string{"state": "online"}, metricUint(online)))
		output.WriteString(metricSample("qemu_manage_guest_vcpus", map[string]string{"state": "offline"}, metricUint(uint64(len(guest.VCPUs))-online)))
	}
	if collectorOK(snapshot, "guest_filesystems") {
		for _, filesystem := range guest.Filesystems {
			labels := map[string]string{"mountpoint": filesystem.Mountpoint, "fstype": filesystem.Type}
			if filesystem.SizeBytes != nil {
				output.WriteString(metricSample("qemu_manage_guest_filesystem_size_bytes", labels, metricUint(*filesystem.SizeBytes)))
			}
			if filesystem.UsedBytes != nil {
				output.WriteString(metricSample("qemu_manage_guest_filesystem_used_bytes", labels, metricUint(*filesystem.UsedBytes)))
			}
		}
	}
	if collectorOK(snapshot, "guest_network") {
		renderGuestNetworkMetrics(output, guest.Networks)
	}
	if collectorOK(snapshot, "guest_disk") {
		renderGuestDiskMetrics(output, guest.Disks)
	}
	if collectorOK(snapshot, "guest_clock") && guest.ClockOffset != nil {
		output.WriteString(metricSample("qemu_manage_guest_clock_offset_seconds", nil, metricFloat(*guest.ClockOffset)))
	}
	if collectorOK(snapshot, "guest_filesystem_freeze") && guest.Frozen != nil {
		output.WriteString(metricSample("qemu_manage_guest_filesystems_frozen", nil, boolMetric(*guest.Frozen)))
	}
}

func renderGuestNetworkMetrics(output *strings.Builder, networks []backend.GuestNetworkInterface) {
	for _, network := range networks {
		labels := map[string]string{"interface": network.Name}
		for name, value := range map[string]*uint64{"qemu_manage_guest_network_receive_bytes_total": network.ReceiveBytes, "qemu_manage_guest_network_transmit_bytes_total": network.TransmitBytes, "qemu_manage_guest_network_receive_packets_total": network.ReceivePackets, "qemu_manage_guest_network_transmit_packets_total": network.TransmitPackets, "qemu_manage_guest_network_receive_errors_total": network.ReceiveErrors, "qemu_manage_guest_network_transmit_errors_total": network.TransmitErrors, "qemu_manage_guest_network_receive_dropped_total": network.ReceiveDropped, "qemu_manage_guest_network_transmit_dropped_total": network.TransmitDropped} {
			if value != nil {
				output.WriteString(metricSample(name, labels, metricUint(*value)))
			}
		}
	}
}

func renderGuestDiskMetrics(output *strings.Builder, disks []backend.GuestDisk) {
	for _, disk := range disks {
		labels := func(operation string) map[string]string {
			return map[string]string{"device": disk.Device, "operation": operation}
		}
		for operation, value := range map[string]*uint64{"read": disk.ReadSectors, "write": disk.WriteSectors, "discard": disk.DiscardSectors} {
			if value != nil {
				output.WriteString(metricSample("qemu_manage_guest_disk_sectors_total", labels(operation), metricUint(*value)))
			}
		}
		for operation, value := range map[string]*uint64{"read": disk.ReadOperations, "write": disk.WriteOperations, "discard": disk.DiscardOperations, "flush": disk.FlushOperations} {
			if value != nil {
				output.WriteString(metricSample("qemu_manage_guest_disk_operations_total", labels(operation), metricUint(*value)))
			}
		}
		for operation, value := range map[string]*uint64{"read": disk.ReadMergedOperations, "write": disk.WriteMergedOperations, "discard": disk.DiscardMergedOperations} {
			if value != nil {
				output.WriteString(metricSample("qemu_manage_guest_disk_merged_operations_total", labels(operation), metricUint(*value)))
			}
		}
		for operation, value := range map[string]*float64{"read": disk.ReadSeconds, "write": disk.WriteSeconds, "discard": disk.DiscardSeconds, "flush": disk.FlushSeconds} {
			if value != nil {
				output.WriteString(metricSample("qemu_manage_guest_disk_io_seconds_total", labels(operation), metricFloat(*value)))
			}
		}
		if disk.WeightedIOSeconds != nil {
			output.WriteString(metricSample("qemu_manage_guest_disk_weighted_io_seconds_total", map[string]string{"device": disk.Device}, metricFloat(*disk.WeightedIOSeconds)))
		}
		if disk.IOInFlight != nil {
			output.WriteString(metricSample("qemu_manage_guest_disk_io_in_flight", map[string]string{"device": disk.Device}, metricUint(*disk.IOInFlight)))
		}
	}
}

// collectorOK reports whether a collector currently has a fresh successful snapshot
func collectorOK(snapshot *Snapshot, key string) bool {
	return snapshot.Collectors[key].Status == CollectorOK
}

// boolMetric encodes a boolean as the Prometheus 0-or-1 sample value
func boolMetric(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// boolString encodes a boolean for use in metric labels
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

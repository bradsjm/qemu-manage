package qemu

import (
	"context"
	"errors"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

// CollectQEMU gathers QMP-backed host observations for the running VM.
func (i *instance) CollectQEMU(ctx context.Context) backend.QEMUObservation {
	qmp, err := i.qmpClient()
	if err != nil {
		return backend.QEMUObservation{QMP: qmpObservationResult(err)}
	}
	observation := backend.QEMUObservation{Version: qmp.Version()}
	observation.State, err = qmp.RawStatus(ctx)
	if err != nil {
		observation.QMP = qmpObservationResult(err)
		return observation
	}
	observation.Blocks, err = qmp.QueryBlocks(ctx)
	if err != nil {
		observation.Block = backend.ObservationResult{Code: "block_query_failed", Err: err}
	}
	observation.Events, err = qmp.EventCounters(ctx)
	if err != nil {
		observation.QMP = qmpObservationResult(err)
	}
	return observation
}

// qmpObservationResult keeps timeouts separate from protocol or transport
// failures so monitoring can distinguish a slow VM from a broken QMP session.
func qmpObservationResult(err error) backend.ObservationResult {
	if err == nil {
		return backend.ObservationResult{}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return backend.ObservationResult{Code: "qmp_timeout", Err: err}
	}
	return backend.ObservationResult{Code: "qmp_protocol_error", Err: err}
}

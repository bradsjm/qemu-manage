//go:build !darwin

package monitoring

const processStatsSupported = false

func collectProcess(int) (ProcessStats, error) {
	return ProcessStats{}, ErrProcessStatsUnsupported
}

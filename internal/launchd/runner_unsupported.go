//go:build !darwin

package launchd

import (
	"context"
	"errors"
)

type platformRunner struct{}

func newPlatformRunner() Runner { return platformRunner{} }

func (platformRunner) Run(context.Context, bool, string, ...string) ([]byte, error) {
	return nil, errors.New("launchd mutations are supported only on macOS")
}

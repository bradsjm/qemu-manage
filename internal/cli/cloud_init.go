package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const cloudInitSeedFilename = "cloud-init.iso"
const hdiutilPath = "/usr/bin/hdiutil"

// cloudInitMetadata supplies the fixed instance-id metadata that cloud-init's
// NoCloud datasource expects on every seed image.
type cloudInitMetadata struct {
	InstanceID string `json:"instance-id"`
}

func (a *App) createCloudInitSeed(
	ctx context.Context,
	vmName string,
	userDataPath string,
	destination string,
	instanceID string,
	progressOutput io.Writer,
	interactive bool,
) (err error) {
	stagingDirectory, err := os.MkdirTemp(filepath.Dir(destination), ".cloud-init-")
	if err != nil {
		return fmt.Errorf("cloud-init: create staging directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(stagingDirectory); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("cloud-init: remove staging directory: %w", removeErr))
		}
	}()

	if err := copyRegularFile(
		userDataPath,
		filepath.Join(stagingDirectory, "user-data"),
		0o600,
		progressOutput,
		true,
		interactive,
		fmt.Sprintf("Copying cloud-init user-data for %s VM", vmName),
		fmt.Sprintf("Copied cloud-init user-data for %s VM", vmName),
	); err != nil {
		return fmt.Errorf("cloud-init: copy user-data: %w", err)
	}
	metadata, err := json.Marshal(cloudInitMetadata{InstanceID: instanceID})
	if err != nil {
		return fmt.Errorf("cloud-init: encode meta-data: %w", err)
	}
	metadata = append(metadata, '\n')
	if err := os.WriteFile(filepath.Join(stagingDirectory, "meta-data"), metadata, 0o600); err != nil {
		return fmt.Errorf("cloud-init: write meta-data: %w", err)
	}

	args := []string{"makehybrid", "-o", destination, stagingDirectory, "-iso", "-joliet", "-default-volume-name", "cidata"}
	if err := withWaitingProgress(
		progressOutput,
		true,
		interactive,
		fmt.Sprintf("Creating cloud-init seed for %s VM", vmName),
		fmt.Sprintf("Created cloud-init seed for %s VM", vmName),
		func() error {
			return a.runExternal(ctx, hdiutilPath, args)
		},
	); err != nil {
		return fmt.Errorf("cloud-init: create seed ISO: %w", err)
	}
	if err := os.Chmod(destination, 0o400); err != nil {
		return fmt.Errorf("cloud-init: set seed mode: %w", err)
	}
	return nil
}

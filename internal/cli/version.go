package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

const repositoryURL = "https://github.com/bradsjm/qemu-manage"

// releaseVersion is populated from the release tag by the release build.
var releaseVersion string

// buildInfo holds the build metadata reported by the version command.
type buildInfo struct {
	version    string
	revision   string
	commitTime string
	modified   string
	goVersion  string
}

// currentBuildInfo reports embedded build metadata, falling back to
// development defaults when release data is unavailable.
func currentBuildInfo() buildInfo {
	info := buildInfo{
		version:    "devel",
		revision:   "unknown",
		commitTime: "unknown",
		modified:   "unknown",
		goVersion:  runtime.Version(),
	}

	if embedded, ok := debug.ReadBuildInfo(); ok {
		if embedded.Main.Version != "" && embedded.Main.Version != "(devel)" {
			info.version = strings.TrimPrefix(embedded.Main.Version, "v")
		}
		if embedded.GoVersion != "" {
			info.goVersion = embedded.GoVersion
		}
		for _, setting := range embedded.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.revision = setting.Value
			case "vcs.time":
				info.commitTime = setting.Value
			case "vcs.modified":
				info.modified = setting.Value
			}
		}
	}
	if releaseVersion != "" {
		info.version = strings.TrimPrefix(releaseVersion, "v")
	}
	return info
}

// writeVersion prints the build metadata table.
func writeVersion(output io.Writer, interactive bool) error {
	info := currentBuildInfo()
	rows := [][]string{
		{"version", fmt.Sprintf("qemu-manage %s", info.version)},
		{"revision", info.revision},
		{"commit time", info.commitTime},
		{"modified", info.modified},
		{"go version", info.goVersion},
		{"repository", repositoryURL},
	}
	return writeTable(output, interactive, []string{"FIELD", "VALUE"}, rows)
}

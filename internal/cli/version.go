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

type buildInfo struct {
	version    string
	revision   string
	commitTime string
	modified   string
	goVersion  string
}

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

func writeVersion(output io.Writer) error {
	info := currentBuildInfo()
	_, err := fmt.Fprintf(output, `qemu-manage %s
  revision: %s
  commit time: %s
  modified: %s
  go version: %s
  repository: %s
`, info.version, info.revision, info.commitTime, info.modified, info.goVersion, repositoryURL)
	return err
}

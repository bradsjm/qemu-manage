package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"text/template"

	"github.com/bradsjm/qemu-manage/internal/model"
)

const launchdPath = "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

var plistTemplate = template.Must(template.New("plist").Funcs(template.FuncMap{
	"xml": escapeXML,
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{xml .Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{xml .Executable}}</string>
    <string>start</string>
    <string>{{xml .Name}}</string>
    <string>--foreground</string>
  </array>
  <key>WorkingDirectory</key>
  <string>{{xml .WorkDir}}</string>
  <key>StandardOutPath</key>
  <string>{{xml .StdoutLog}}</string>
  <key>StandardErrorPath</key>
  <string>{{xml .StderrLog}}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>{{xml .Home}}</string>
    <key>PATH</key>
    <string>` + launchdPath + `</string>
    <key>QEMU_MANAGE_DATA_ROOT</key>
    <string>{{xml .DataRoot}}</string>
    <key>QEMU_MANAGE_RUNTIME_ROOT</key>
    <string>{{xml .RuntimeRoot}}</string>
    <key>QEMU_MANAGE_LOG_ROOT</key>
    <string>{{xml .LogRoot}}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
{{if .SocketPath}}  <key>WatchPaths</key>
  <array>
    <string>{{xml .SocketPath}}</string>
  </array>
{{end}}
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ExitTimeOut</key>
  <integer>{{.ExitTimeOut}}</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>Umask</key>
  <integer>63</integer>
{{if .KeepAlive}}  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
{{end}}{{if .Username}}  <key>UserName</key>
  <string>{{xml .Username}}</string>
{{end}}</dict>
</plist>
`))

// plistData holds every template parameter needed to render one VM launchd job
type plistData struct {
	Label       string
	Executable  string
	Name        string
	WorkDir     string
	StdoutLog   string
	StderrLog   string
	Home        string
	DataRoot    string
	RuntimeRoot string
	LogRoot     string
	Username    string
	SocketPath  string
	ExitTimeOut int
	KeepAlive   bool
}

// Label returns the immutable launchd job label for a VM ID.
func Label(id string) string {
	if len(id) > 12 {
		id = id[:12]
	}
	return "io.qemu-manage.vm." + id
}

// Filename returns the launchd plist filename for a VM ID.
func Filename(id string) string { return Label(id) + ".plist" }

// Render validates cfg and renders its launchd property list deterministically.
func Render(cfg *model.Config, executable, workDir, stdoutLog, stderrLog, username, home, dataRoot, runtimeRoot, logRoot string) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("launchd: config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("launchd: %w", err)
	}
	for _, path := range []struct {
		field string
		value string
	}{
		{"executable", executable},
		{"working directory", workDir},
		{"stdout log", stdoutLog},
		{"stderr log", stderrLog},
		{"home", home},
		{"data root", dataRoot},
		{"runtime root", runtimeRoot},
		{"log root", logRoot},
	} {
		if !filepath.IsAbs(path.value) {
			return nil, fmt.Errorf("launchd: %s path must be absolute", path.field)
		}
	}
	if cfg.Autostart.Scope != model.AutostartBoot && cfg.Autostart.Scope != model.AutostartLogin {
		return nil, fmt.Errorf("launchd: autostart scope must be %q or %q", model.AutostartBoot, model.AutostartLogin)
	}
	if cfg.Autostart.Scope == model.AutostartBoot {
		if username == "" {
			return nil, errors.New("launchd: boot scope requires a username")
		}
		if username == "root" {
			return nil, errors.New("launchd: boot scope username must be non-root")
		}
	} else {
		username = ""
	}

	socketPath := ""
	if cfg.Network.Mode == model.NetworkSocketVMNet {
		socketPath = cfg.Network.SocketVMNet.SocketPath
	}

	data := plistData{
		Label:       Label(cfg.ID),
		Executable:  executable,
		Name:        cfg.Name,
		WorkDir:     workDir,
		StdoutLog:   stdoutLog,
		StderrLog:   stderrLog,
		Home:        home,
		DataRoot:    dataRoot,
		RuntimeRoot: runtimeRoot,
		LogRoot:     logRoot,
		Username:    username,
		SocketPath:  socketPath,
		ExitTimeOut: cfg.ShutdownTimeoutSeconds + 15,
		KeepAlive:   cfg.RestartPolicy == model.RestartOnFailure,
	}
	var rendered bytes.Buffer
	if err := plistTemplate.Execute(&rendered, data); err != nil {
		return nil, fmt.Errorf("launchd: render plist: %w", err)
	}
	return rendered.Bytes(), nil
}

// escapeXML safely escapes plist string data before template insertion
func escapeXML(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

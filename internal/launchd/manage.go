package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bradsjm/qemu-manage/internal/model"
	"github.com/bradsjm/qemu-manage/internal/store"
)

const launchctlPath = "/bin/launchctl"

type Runner interface {
	Run(ctx context.Context, privileged bool, path string, args ...string) ([]byte, error)
}

type Manager struct {
	Store                  *store.Store
	Runner                 Runner
	Executable             string
	Username               string
	Home                   string
	UID                    int
	Stopped                func(context.Context, *model.Config) error
	LoginDir               string
	SystemDir              string
	SocketVMNetInstallRoot string
	SocketVMNetRunRoot     string
	WaitForSocketVMNet     func(context.Context, string) error
}

type DomainStatus struct {
	FilePresent bool   `json:"file_present"`
	FileMatch   bool   `json:"file_match"`
	Loaded      bool   `json:"loaded"`
	Error       string `json:"error,omitempty"`
}

type StatusReport struct {
	ConfiguredScope model.AutostartScope `json:"configured_scope"`
	Login           DomainStatus         `json:"login"`
	Boot            DomainStatus         `json:"boot"`
}

type domain int

const (
	domainLogin domain = iota
	domainSystem
)

type pathInspection struct {
	Path    string
	Present bool
	Bytes   []byte
}

func NewManager(store *store.Store, executable, username, home string, uid int) *Manager {
	return &Manager{
		Store:                  store,
		Runner:                 newPlatformRunner(),
		Executable:             executable,
		Username:               username,
		Home:                   home,
		UID:                    uid,
		LoginDir:               filepath.Join(home, "Library", "LaunchAgents"),
		SystemDir:              "/Library/LaunchDaemons",
		SocketVMNetInstallRoot: socketVMNetInstallRootDefault,
		SocketVMNetRunRoot:     socketVMNetRunRootDefault,
	}
}

func (m *Manager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return newPlatformRunner()
}

func (m *Manager) plistPath(d domain, id string) string {
	dir := m.LoginDir
	if d == domainSystem {
		dir = m.SystemDir
	}
	return filepath.Join(dir, Filename(id))
}

func (m *Manager) target(d domain, id string) string {
	label := Label(id)
	if d == domainSystem {
		return "system/" + label
	}
	return "gui/" + strconv.Itoa(m.UID) + "/" + label
}

func (m *Manager) inspectPath(d domain, id string) (pathInspection, error) {
	path := m.plistPath(d, id)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return pathInspection{Path: path}, nil
	}
	if err != nil {
		return pathInspection{Path: path}, fmt.Errorf("launchd: inspect %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pathInspection{Path: path}, fmt.Errorf("launchd: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return pathInspection{Path: path}, fmt.Errorf("launchd: %s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pathInspection{Path: path}, fmt.Errorf("launchd: cannot inspect ownership of %s", path)
	}
	if d == domainLogin {
		if int(stat.Uid) != m.UID || info.Mode().Perm() != 0600 {
			return pathInspection{Path: path, Present: true}, fmt.Errorf("launchd: login plist %s must be owned by uid %d with mode 0600", path, m.UID)
		}
	} else if stat.Uid != 0 || stat.Gid != 0 || info.Mode().Perm() != 0644 {
		return pathInspection{Path: path, Present: true}, fmt.Errorf("launchd: system plist %s must be root:wheel with mode 0644", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return pathInspection{Path: path}, fmt.Errorf("launchd: read %s: %w", path, err)
	}
	label, err := plistLabel(data)
	if err != nil {
		return pathInspection{Path: path, Present: true}, fmt.Errorf("launchd: parse %s: %w", path, err)
	}
	if label != Label(id) {
		return pathInspection{Path: path, Present: true}, fmt.Errorf("launchd: foreign plist at %s has label %q", path, label)
	}
	return pathInspection{Path: path, Present: true, Bytes: data}, nil
}

func (m *Manager) inspectBoth(id string) (pathInspection, pathInspection, error) {
	login, err := m.inspectPath(domainLogin, id)
	if err != nil {
		return login, pathInspection{}, err
	}
	system, err := m.inspectPath(domainSystem, id)
	return login, system, err
}

func plistLabel(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	inDict, depth := false, 0
	wantValue, found := false, false
	var label string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "dict" && !inDict {
				inDict, depth = true, 1
				continue
			}
			if !inDict {
				continue
			}
			depth++
			if depth == 2 && value.Name.Local == "key" {
				var key string
				if err := decoder.DecodeElement(&key, &value); err != nil {
					return "", err
				}
				depth--
				wantValue = key == "Label"
				continue
			}
			if depth == 2 && wantValue {
				if value.Name.Local != "string" {
					return "", errors.New("Label value is not a string")
				}
				if found {
					return "", errors.New("duplicate Label key")
				}
				if err := decoder.DecodeElement(&label, &value); err != nil {
					return "", err
				}
				depth--
				wantValue, found = false, true
			}
		case xml.EndElement:
			if inDict {
				depth--
				if depth == 0 {
					inDict = false
				}
			}
		}
	}
	if wantValue {
		return "", errors.New("Label has no value")
	}
	if !found || label == "" {
		return "", errors.New("missing Label string")
	}
	return label, nil
}

func (m *Manager) printLoaded(ctx context.Context, d domain, id string) (bool, error) {
	target := m.target(d, id)
	output, err := m.runner().Run(ctx, false, launchctlPath, "print", target)
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, commandError("print "+target, output, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, commandError("print "+target, output, err)
	}
	if serviceNotFound(output, err, Label(id)) {
		return false, nil
	}
	return false, commandError("print "+target, output, err)
}

func serviceNotFound(output []byte, err error, label string) bool {
	diagnostic := string(output)
	if err != nil {
		diagnostic += "\n" + err.Error()
	}
	return strings.Contains(diagnostic, `Could not find service "`+label+`" in domain for `)
}

func (m *Manager) lint(ctx context.Context, path string) error {
	output, err := m.runner().Run(ctx, false, "/usr/bin/plutil", "-lint", path)
	if err != nil {
		return commandError("lint "+path, output, err)
	}
	return nil
}

func (m *Manager) bootstrap(ctx context.Context, d domain, path string) error {
	output, err := m.runner().Run(ctx, d == domainSystem, launchctlPath, "bootstrap", m.domainTarget(d), path)
	if err != nil {
		return commandError("bootstrap "+path, output, err)
	}
	return nil
}

func (m *Manager) bootout(ctx context.Context, d domain, id string) error {
	output, err := m.runner().Run(ctx, d == domainSystem, launchctlPath, "bootout", m.target(d, id))
	if err != nil {
		return commandError("bootout "+m.target(d, id), output, err)
	}
	return nil
}

func (m *Manager) enableJob(ctx context.Context, d domain, id string) error {
	output, err := m.runner().Run(ctx, d == domainSystem, launchctlPath, "enable", m.target(d, id))
	if err != nil {
		return commandError("enable "+m.target(d, id), output, err)
	}
	return nil
}

func (m *Manager) domainTarget(d domain) string {
	if d == domainSystem {
		return "system"
	}
	return "gui/" + strconv.Itoa(m.UID)
}

func (m *Manager) installCandidate(ctx context.Context, d domain, candidate, destination string) error {
	if d == domainSystem {
		output, err := m.runner().Run(ctx, true, "/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "0644", candidate, destination)
		if err != nil {
			return commandError("install "+destination, output, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fmt.Errorf("launchd: create LaunchAgents: %w", err)
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		return fmt.Errorf("launchd: read candidate: %w", err)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("launchd: install %s: %w", destination, err)
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Chmod(0600)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("launchd: install %s: %w", destination, writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("launchd: install %s: %w", destination, closeErr)
	}
	return nil
}

func (m *Manager) removePlist(ctx context.Context, d domain, path string) error {
	if d == domainSystem {
		output, err := m.runner().Run(ctx, true, "/bin/rm", "-f", "--", path)
		if err != nil {
			return commandError("remove "+path, output, err)
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("launchd: remove %s: %w", path, err)
	}
	return nil
}

func commandError(action string, output []byte, err error) error {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return fmt.Errorf("launchd: %s: %w", action, err)
	}
	return fmt.Errorf("launchd: %s: %w: %s", action, err, text)
}

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bradsjm/qemu-manage/internal/launchd"
	"github.com/jedib0t/go-pretty/v6/table"
)

func TestWriteTableUsesOneTrailingNewlineAndNoTabs(t *testing.T) {
	var output bytes.Buffer
	if err := writeTable(&output, table.Row{"NAME", "STATE"}, []table.Row{{"vm", "running"}}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("table newline contract failed: %q", got)
	}
	if strings.Contains(got, "\t") || !strings.Contains(got, "│ NAME │ STATE") {
		t.Fatalf("table format=%q", got)
	}
}

func TestHumanReportTablesShareHeadersAndValues(t *testing.T) {
	var status bytes.Buffer
	if err := writeRows([]StatusRow{{Name: "vm", State: "running", RestartRequired: true, Error: "boom"}}, false, &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "RESTART REQUIRED") || !strings.Contains(status.String(), "boom") || strings.Contains(status.String(), "\t") {
		t.Fatalf("status table=%q", status.String())
	}

	var autostart bytes.Buffer
	report := launchd.StatusReport{
		ConfiguredScope: "boot",
		Login:           launchd.DomainStatus{FilePresent: true, FileMatch: false, Loaded: true, Error: "login failed"},
	}
	if err := writeAutostartStatus(&autostart, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SETTING", "configured_scope", "DOMAIN", "FILE PRESENT", `"login failed"`} {
		if !strings.Contains(autostart.String(), want) {
			t.Errorf("autostart table missing %q: %q", want, autostart.String())
		}
	}
	if strings.Contains(autostart.String(), "\t") {
		t.Fatalf("autostart table contains tabs: %q", autostart.String())
	}

	var version bytes.Buffer
	if err := writeVersion(&version); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FIELD", "version", "revision", "commit time", "modified", "go version", "repository"} {
		if !strings.Contains(version.String(), want) {
			t.Errorf("version table missing %q: %q", want, version.String())
		}
	}
}

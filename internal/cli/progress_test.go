package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jedib0t/go-pretty/v6/progress"
)

func TestWithProgressDisabledDoesNotWriteOrCreateTracker(t *testing.T) {
	var output bytes.Buffer
	wantErr := errors.New("operation failed")
	gotErr := withProgress(&output, false, true, "Downloading image", 10, progress.UnitsBytes, func(tracker *progress.Tracker) error {
		if tracker != nil {
			t.Fatal("disabled progress created a tracker")
		}
		return wantErr
	})
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error=%v, want %v", gotErr, wantErr)
	}
	if output.Len() != 0 {
		t.Fatalf("disabled progress wrote %q", output.String())
	}
}

func TestWithProgressRedirectedReportsStartAndFinish(t *testing.T) {
	var output bytes.Buffer
	if err := withProgress(&output, true, false, "Creating primary disk", 0, progress.UnitsDefault, func(tracker *progress.Tracker) error {
		if tracker != nil {
			t.Fatal("redirected progress unexpectedly created a tracker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "Creating primary disk...\nCreating primary disk done\n" {
		t.Fatalf("output=%q", got)
	}
}

func TestWithProgressInteractiveCompletesDeterminateAndIndeterminate(t *testing.T) {

	for _, test := range []struct {
		name    string
		message string
		total   int64
		units   progress.Units
		work    func(*progress.Tracker) error
		want    []string
	}{
		{
			name:    "determinate",
			message: "Copying firmware code",
			total:   4,
			units:   progress.UnitsBytes,
			work: func(tracker *progress.Tracker) error {
				tracker.Increment(4)
				return nil
			},
			want: []string{"Copying firmware code", "4B", "done!"},
		},
		{
			name:    "indeterminate",
			message: "Starting VM",
			total:   0,
			units:   progress.UnitsDefault,
			work: func(tracker *progress.Tracker) error {
				tracker.Increment(1)
				return nil
			},
			want: []string{"Starting VM"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := withProgress(&output, true, true, test.message, test.total, test.units, func(tracker *progress.Tracker) error {
				if tracker == nil {
					t.Fatal("interactive progress did not create a tracker")
				}
				return test.work(tracker)
			}); err != nil {
				t.Fatal(err)
			}
			got := output.String()
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("output=%q does not contain %q", got, want)
				}
			}
		})
	}
}
func TestWithProgressReportsFailureAndPreservesError(t *testing.T) {
	var output bytes.Buffer
	wantErr := errors.New("disk failed")
	gotErr := withWaitingProgress(&output, true, false, "Creating primary disk", func() error {
		return wantErr
	})
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error=%v, want %v", gotErr, wantErr)
	}
	if got := output.String(); got != "Creating primary disk...\nCreating primary disk failed: disk failed\n" {
		t.Fatalf("failure output=%q", got)
	}
}

func TestCopyWithProgressCountsBytesWritten(t *testing.T) {
	var output bytes.Buffer
	tracker := &progress.Tracker{Total: 4}
	if err := copyWithProgress(strings.NewReader("abcd"), &output, 4, tracker); err != nil {
		t.Fatal(err)
	}
	if output.String() != "abcd" {
		t.Fatalf("output=%q", output.String())
	}
	if tracker.Value() != 4 {
		t.Fatalf("tracker value=%d, want 4", tracker.Value())
	}
}

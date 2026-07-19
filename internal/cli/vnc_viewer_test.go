package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"qemu-manage/internal/backend"
)

type recordedVNCCommand struct {
	path  string
	args  []string
	stdin string
}

func TestOpenVNCWithRunnerCopiesPasswordThenOpensURL(t *testing.T) {
	calls := make([]recordedVNCCommand, 0, 2)
	runner := func(_ context.Context, path string, args []string, stdin io.Reader) error {
		command := recordedVNCCommand{path: path, args: append([]string(nil), args...)}
		if stdin != nil {
			data, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatalf("read stdin: %v", err)
			}
			command.stdin = string(data)
		}
		calls = append(calls, command)
		return nil
	}

	endpoint := backend.VNCEndpoint{Host: "127.0.0.1", Port: 5907}
	if err := openVNCWithRunner(context.Background(), endpoint, "secret", runner); err != nil {
		t.Fatalf("openVNCWithRunner failed: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("call count = %d, want 2", len(calls))
	}
	if calls[0].path != "/usr/bin/pbcopy" || calls[0].stdin != "secret" {
		t.Fatalf("first call = %+v, want pbcopy with password on stdin", calls[0])
	}
	if len(calls[0].args) != 0 {
		t.Fatalf("pbcopy args = %q, want none", calls[0].args)
	}
	if calls[1].path != "/usr/bin/open" {
		t.Fatalf("second call path = %q, want /usr/bin/open", calls[1].path)
	}
	if got, want := calls[1].args, []string{"vnc://127.0.0.1:5907"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("open args = %q, want %q", got, want)
	}
	if calls[1].stdin != "" {
		t.Fatalf("open stdin = %q, want empty", calls[1].stdin)
	}
	for _, arg := range calls[1].args {
		if strings.Contains(arg, "secret") {
			t.Fatalf("open args leaked password: %q", calls[1].args)
		}
	}
}

func TestOpenVNCWithRunnerShortCircuitsOnCopyError(t *testing.T) {
	copyErr := errors.New("pbcopy failed")
	calls := 0
	runner := func(_ context.Context, path string, _ []string, _ io.Reader) error {
		calls++
		if path != "/usr/bin/pbcopy" {
			t.Fatalf("unexpected path after copy failure: %q", path)
		}
		return copyErr
	}

	err := openVNCWithRunner(context.Background(), backend.VNCEndpoint{Host: "127.0.0.1", Port: 5900}, "secret", runner)
	if !errors.Is(err, copyErr) {
		t.Fatalf("error = %v, want %v", err, copyErr)
	}
	if calls != 1 {
		t.Fatalf("call count = %d, want 1", calls)
	}
}

func TestOpenVNCWithRunnerReturnsOpenError(t *testing.T) {
	openErr := errors.New("open failed")
	calls := 0
	runner := func(_ context.Context, path string, args []string, stdin io.Reader) error {
		calls++
		switch calls {
		case 1:
			if path != "/usr/bin/pbcopy" {
				t.Fatalf("first path = %q, want /usr/bin/pbcopy", path)
			}
			data, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatalf("read stdin: %v", err)
			}
			if string(data) != "secret" {
				t.Fatalf("pbcopy stdin = %q, want secret", string(data))
			}
			return nil
		case 2:
			if path != "/usr/bin/open" {
				t.Fatalf("second path = %q, want /usr/bin/open", path)
			}
			if stdin != nil {
				t.Fatal("open stdin was not nil")
			}
			if len(args) != 1 || args[0] != "vnc://127.0.0.1:5901" {
				t.Fatalf("open args = %q, want exact URL", args)
			}
			return openErr
		default:
			t.Fatalf("unexpected extra call %d", calls)
			return nil
		}
	}

	err := openVNCWithRunner(context.Background(), backend.VNCEndpoint{Host: "127.0.0.1", Port: 5901}, "secret", runner)
	if !errors.Is(err, openErr) {
		t.Fatalf("error = %v, want %v", err, openErr)
	}
	if calls != 2 {
		t.Fatalf("call count = %d, want 2", calls)
	}
}

func TestOpenVNCWithRunnerRejectsNilRunner(t *testing.T) {
	err := openVNCWithRunner(context.Background(), backend.VNCEndpoint{Host: "127.0.0.1", Port: 5901}, "secret", nil)
	if err == nil || err.Error() != "vnc: viewer is unavailable" {
		t.Fatalf("error = %v, want viewer unavailable", err)
	}
}

package main

import "testing"

func TestOwnsLifecycleSignals(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "no command"},
		{name: "create", args: []string{"create", "vm"}},
		{name: "detached start", args: []string{"start", "vm"}},
		{name: "foreground start", args: []string{"start", "vm", "--foreground"}, want: true},
		{name: "single-dash foreground start", args: []string{"start", "vm", "-foreground"}, want: true},
		{name: "explicit true foreground start", args: []string{"start", "vm", "--foreground=true"}, want: true},
		{name: "numeric true foreground start", args: []string{"start", "vm", "-foreground=1"}, want: true},
		{name: "explicit false foreground start", args: []string{"start", "vm", "--foreground=false"}},
		{name: "last foreground true wins", args: []string{"start", "vm", "--foreground=false", "--foreground"}, want: true},
		{name: "last foreground false wins", args: []string{"start", "vm", "--foreground", "--foreground=false"}},
		{name: "supervisor", args: []string{"supervise", "vm", "--ready-fd", "3"}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ownsLifecycleSignals(test.args); got != test.want {
				t.Fatalf("ownsLifecycleSignals(%q) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

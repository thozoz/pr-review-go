package main

import (
	"strings"
	"testing"
)

// TestParseLandlockRunArgs pins the landlock-run argument contract that the
// server and the rank probe both emit.
func TestParseLandlockRunArgs(t *testing.T) {
	t.Run("canonical-shape", func(t *testing.T) {
		opts, err := parseLandlockRunArgs([]string{"--ro", "/", "--rw", "/a", "--rw", "/b", "--", "/bin/true"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if strings.Join(opts.ro, ",") != "/" {
			t.Errorf("ro = %q, want [/]", opts.ro)
		}
		if strings.Join(opts.rw, ",") != "/a,/b" {
			t.Errorf("rw = %q, want [/a /b] in order", opts.rw)
		}
		if strings.Join(opts.command, " ") != "/bin/true" {
			t.Errorf("command = %q, want [/bin/true]", opts.command)
		}
	})

	t.Run("single-dash-and-repeated-roots", func(t *testing.T) {
		opts, err := parseLandlockRunArgs([]string{"-ro", "/", "-rw", "/a", "-rw", "/b", "--", "go", "build", "./..."})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(opts.ro) != 1 || opts.ro[0] != "/" {
			t.Errorf("ro = %q", opts.ro)
		}
		if len(opts.rw) != 2 || opts.rw[0] != "/a" || opts.rw[1] != "/b" {
			t.Errorf("rw = %q", opts.rw)
		}
		if strings.Join(opts.command, " ") != "go build ./..." {
			t.Errorf("command = %q", opts.command)
		}
	})

	t.Run("separator-allows-dash-command", func(t *testing.T) {
		opts, err := parseLandlockRunArgs([]string{"--rw", "/a", "--", "-weird-cmd", "arg"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if strings.Join(opts.command, " ") != "-weird-cmd arg" {
			t.Errorf("command = %q", opts.command)
		}
	})

	reject := []struct {
		name string
		args []string
	}{
		{"empty-command", []string{"--rw", "/a"}},
		{"zero-rw", []string{"--ro", "/", "--", "/bin/true"}},
		{"relative-ro", []string{"--ro", "rel", "--rw", "/a", "--", "/bin/true"}},
		{"relative-rw", []string{"--rw", "rel", "--", "/bin/true"}},
		{"empty-ro", []string{"--ro", "", "--rw", "/a", "--", "/bin/true"}},
		{"unknown-flag", []string{"--rw", "/a", "--nope", "--", "/bin/true"}},
		{"dash-command-without-separator", []string{"--rw", "/a", "-weird-cmd"}},
		{"same-root-both", []string{"--ro", "/a", "--rw", "/a", "--", "/bin/true"}},
		{"rw-nested-in-ro", []string{"--ro", "/a", "--rw", "/a/b", "--", "/bin/true"}},
		{"ro-nested-in-rw", []string{"--rw", "/a", "--ro", "/a/b", "--", "/bin/true"}},
	}
	for _, tc := range reject {
		t.Run("reject-"+tc.name, func(t *testing.T) {
			if _, err := parseLandlockRunArgs(tc.args); err == nil {
				t.Fatalf("expected a usage error for %v", tc.args)
			} else if !strings.Contains(err.Error(), "landlock-run") {
				t.Errorf("usage error must name landlock-run: %v", err)
			}
		})
	}

	if !strings.Contains(landlockRunUsage, "landlock-run") ||
		!strings.Contains(landlockRunUsage, "[--ro") ||
		!strings.Contains(landlockRunUsage, "[--rw") ||
		!strings.Contains(landlockRunUsage, "-- <command>") {
		t.Errorf("usage text must name the command and the flag contract: %q", landlockRunUsage)
	}
}

// TestLandlockUnsupportedExit pins the non-Linux typed unavailable result: the
// code is 70 and the reason names Linux without claiming a specific errno it
// cannot observe.
func TestLandlockUnsupportedExit(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		code, reason := landlockUnsupportedExit(goos)
		if code != 70 {
			t.Errorf("%s: code = %d, want 70", goos, code)
		}
		if reason == "" {
			t.Fatalf("%s: reason must be non-empty", goos)
		}
		if !strings.Contains(reason, "Linux") {
			t.Errorf("%s: reason must name Linux: %q", goos, reason)
		}
		if !strings.Contains(reason, goos) {
			t.Errorf("%s: reason must name the observed GOOS: %q", goos, reason)
		}
		if strings.Contains(reason, "panic") {
			t.Errorf("%s: reason must not mention panic: %q", goos, reason)
		}
		for _, errno := range []string{"EOPNOTSUPP", "ENOSYS", "EPERM", "EINVAL"} {
			if strings.Contains(reason, errno) {
				t.Errorf("%s: reason must not claim an unobserved errno %s: %q", goos, errno, reason)
			}
		}
	}
}

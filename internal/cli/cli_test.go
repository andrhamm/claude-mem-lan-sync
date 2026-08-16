package cli

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, Env{Stdout: &out, Stderr: &errOut})
	return code, out.String(), errOut.String()
}

func TestNoArgsPrintsUsage(t *testing.T) {
	code, _, stderr := run(t)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr missing usage, got %q", stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "wat")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "wat"`) {
		t.Fatalf("stderr = %q, want it to name the command", stderr)
	}
}

func TestVersion(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout, "cmemlan ") {
		t.Fatalf("stdout = %q, want it to start with the binary name", stdout)
	}
	if !strings.Contains(stdout, "claude-mem") {
		t.Fatalf("stdout = %q, want the verified claude-mem version", stdout)
	}
}

// Flags and positionals must parse in either order. Go's flag package stops at
// the first positional, so the documented `connect <url> --code <code>` form
// would otherwise drop --code and pair against nothing.
func TestParseFlagsAcceptsInterleavedPositionals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flag first", []string{"--code", "431-982", "http://hub.local:8787"}},
		{"positional first", []string{"http://hub.local:8787", "--code", "431-982"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("connect", flag.ContinueOnError)
			fs.SetOutput(new(bytes.Buffer))
			code := fs.String("code", "", "pairing code")

			pos, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if *code != "431-982" {
				t.Errorf("--code = %q, want 431-982", *code)
			}
			if len(pos) != 1 || pos[0] != "http://hub.local:8787" {
				t.Errorf("positionals = %v, want [http://hub.local:8787]", pos)
			}
		})
	}
}

func TestParseFlagsMultiplePositionals(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(new(bytes.Buffer))
	verbose := fs.Bool("verbose", false, "")

	pos, err := parseFlags(fs, []string{"a", "--verbose", "b"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !*verbose {
		t.Error("--verbose not set")
	}
	if len(pos) != 2 || pos[0] != "a" || pos[1] != "b" {
		t.Errorf("positionals = %v, want [a b]", pos)
	}
}

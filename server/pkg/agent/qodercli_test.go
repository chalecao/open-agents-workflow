package agent

import (
	"testing"
)

func TestQodercliNewReturnsBackend(t *testing.T) {
	t.Parallel()
	b, err := New("qodercli", Config{ExecutablePath: "/nonexistent/qodercli"})
	if err != nil {
		t.Fatalf("New(qodercli) error: %v", err)
	}
	if _, ok := b.(*qodercliBackend); !ok {
		t.Fatalf("expected *qodercliBackend, got %T", b)
	}
}

func TestQodercliBlockedArgsCoverHardcodedFlags(t *testing.T) {
	t.Parallel()
	// Every flag the daemon hardcodes MUST be in the blocked map; a
	// regression here would let a user's custom_args silently override a
	// flag the daemon needs to control (print mode, --yolo, resume tokens,
	// workdir). Spot-check the highest-risk entries by name; the
	// exhaustive set is asserted via filterCustomArgs elsewhere.
	want := []string{
		"--print",
		"-p",
		"--prompt",
		"-i",
		"--yolo",
		"-c",
		"--continue",
		"-r",
		"--resume",
		"--worktree",
		"-w",
		"--workspace",
	}
	for _, arg := range want {
		mode, ok := qodercliBlockedArgs[arg]
		if !ok {
			t.Errorf("qodercliBlockedArgs missing %q — user custom_args could override the daemon's %s", arg, arg)
		}
		if mode != blockedStandalone && mode != blockedWithValue {
			t.Errorf("qodercliBlockedArgs[%q] = %v, want blockedStandalone or blockedWithValue", arg, mode)
		}
	}
}

func TestBuildQodercliArgsHasBaseShape(t *testing.T) {
	t.Parallel()
	args := buildQodercliArgs("hello", ExecOptions{}, nil)
	// The first three flags must always be --print, -p <prompt>, --yolo
	// so the daemon's launch contract is preserved regardless of
	// custom_args. We assert by substring match because filterCustomArgs
	// appends after the base set; the order within filterCustomArgs
	// (ExtraArgs then CustomArgs) is not part of this contract.
	want := []string{"--print", "-p", "hello", "--yolo", "--output-format", "text"}
	if len(args) < len(want) {
		t.Fatalf("buildQodercliArgs returned %d args, want at least %d: %v", len(args), len(want), args)
	}
	for i, expected := range want {
		if args[i] != expected {
			t.Errorf("buildQodercliArgs[%d] = %q, want %q (full: %v)", i, args[i], expected, args)
		}
	}
}

func TestBuildQodercliArgsIncludesResumeFlag(t *testing.T) {
	t.Parallel()
	args := buildQodercliArgs("hello", ExecOptions{ResumeSessionID: "abc-123"}, nil)
	// Look for the -r <id> pair anywhere in the argv. The exact
	// position is not part of the contract (custom_args may interleave
	// with daemon-managed flags), so we just confirm the id made it in.
	found := false
	for i, a := range args {
		if a == "-r" && i+1 < len(args) && args[i+1] == "abc-123" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("buildQodercliArgs did not include -r <ResumeSessionID>; got %v", args)
	}
}

package workload

import (
	"strings"
	"testing"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

func TestSplitInBoxRecoversDurationAndKeepsStderr(t *testing.T) {
	stderr := "warning: something\n" + inBoxMarker + "1500000\nmore stderr"
	d, rest := splitInBox(stderr)
	if d != 1500*time.Microsecond {
		t.Errorf("duration = %v, want 1.5ms", d)
	}
	if strings.Contains(rest, inBoxMarker) {
		t.Errorf("marker left in stderr: %q", rest)
	}
	for _, want := range []string{"warning: something", "more stderr"} {
		if !strings.Contains(rest, want) {
			t.Errorf("stderr lost %q: %q", want, rest)
		}
	}
}

func TestSplitInBoxUnparseableMarkerYieldsZero(t *testing.T) {
	// A BSD `date` does not understand %N, so the marker carries junk. The
	// result must be "not measured", not a bogus duration.
	d, _ := splitInBox(inBoxMarker + "1760000000N")
	if d != 0 {
		t.Errorf("duration = %v, want 0", d)
	}
}

func TestSplitInBoxNoMarker(t *testing.T) {
	d, rest := splitInBox("plain stderr")
	if d != 0 {
		t.Errorf("duration = %v, want 0", d)
	}
	if rest != "plain stderr" {
		t.Errorf("stderr changed: %q", rest)
	}
}

func TestTimedShellWrapsTheLine(t *testing.T) {
	c := TimedShell("echo hi")
	if len(c.Args) != 2 || c.Args[0] != "-lc" {
		t.Fatalf("unexpected command shape: %+v", c)
	}
	if !strings.Contains(c.Args[1], "echo hi") {
		t.Errorf("wrapper dropped the command: %q", c.Args[1])
	}
	if !strings.Contains(c.Args[1], inBoxMarker) {
		t.Errorf("wrapper does not emit the marker: %q", c.Args[1])
	}
	if !strings.Contains(c.Args[1], "exit $__sbb_rc") {
		t.Errorf("wrapper does not propagate the exit code: %q", c.Args[1])
	}
}

func TestBestPrefersInBox(t *testing.T) {
	d, src := best(execResult(5*time.Millisecond, 50*time.Millisecond))
	if d != 5*time.Millisecond || src != "in_box" {
		t.Errorf("got %v/%s, want 5ms/in_box", d, src)
	}
	d, src = best(execResult(0, 50*time.Millisecond))
	if d != 50*time.Millisecond || src != "wall" {
		t.Errorf("got %v/%s, want 50ms/wall", d, src)
	}
}

func TestRequirementsUnmetNamesTheMissingCapability(t *testing.T) {
	r := Requirements{Suspend: true, Resume: true}
	if got := r.Unmet(caps(false, false)); got != "suspend" {
		t.Errorf("got %q, want suspend", got)
	}
	if got := r.Unmet(caps(true, false)); got != "resume" {
		t.Errorf("got %q, want resume", got)
	}
	if got := r.Unmet(caps(true, true)); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func execResult(inBox, wall time.Duration) sandbox.ExecResult {
	return sandbox.ExecResult{InBox: inBox, Wall: wall}
}

func caps(suspend, resume bool) sandbox.Capabilities {
	return sandbox.Capabilities{Suspend: suspend, Resume: resume}
}

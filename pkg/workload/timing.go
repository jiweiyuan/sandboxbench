package workload

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// inBoxMarker is the line the instrumented shell wrapper writes to stderr so
// the runner can recover the duration measured inside the sandbox.
const inBoxMarker = "__SBB_NS="

// TimedShell wraps a shell line so that the sandbox itself times the work and
// reports the result on stderr.
//
// This separation is the whole reason the wrapper exists. A client-side
// stopwatch around Exec measures the command plus one control-plane round
// trip, and that round trip differs by an order of magnitude between a
// per-command REST call and a multiplexed SSH session. Comparing compute speed
// across providers on client-side timing would mostly compare their exec
// transports.
func TimedShell(line string) sandbox.Command {
	wrapped := "__sbb_s=$(date +%s%N); { " + line + " ; }; __sbb_rc=$?; " +
		"__sbb_e=$(date +%s%N); echo \"" + inBoxMarker + "$((__sbb_e-__sbb_s))\" >&2; exit $__sbb_rc"
	return sandbox.Shell(wrapped)
}

// ExecTimed runs a shell line through TimedShell and fills in ExecResult.InBox.
//
// A sandbox whose shell has no nanosecond date leaves InBox at zero; callers
// must treat a zero InBox as "not measured" and fall back to Wall explicitly
// rather than silently reporting a zero duration.
func ExecTimed(ctx context.Context, box sandbox.Sandbox, line string) (sandbox.ExecResult, error) {
	res, err := box.Exec(ctx, TimedShell(line))
	if err != nil {
		return res, err
	}
	res.InBox, res.Stderr = splitInBox(res.Stderr)
	return res, nil
}

// splitInBox pulls the marker line out of stderr and returns the duration it
// carried along with the remaining stderr.
func splitInBox(stderr string) (time.Duration, string) {
	lines := strings.Split(stderr, "\n")
	var d time.Duration
	kept := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(t, inBoxMarker); ok {
			if ns, err := strconv.ParseInt(rest, 10, 64); err == nil && ns > 0 {
				d = time.Duration(ns)
			}
			continue
		}
		kept = append(kept, l)
	}
	return d, strings.Join(kept, "\n")
}

// best returns the in-sandbox duration when it was measured, and the wall
// duration otherwise. The second return value says which one it picked, so a
// workload can record that choice in the result rather than hide it.
func best(res sandbox.ExecResult) (time.Duration, string) {
	if res.InBox > 0 {
		return res.InBox, "in_box"
	}
	return res.Wall, "wall"
}

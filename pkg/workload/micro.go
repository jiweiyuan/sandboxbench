package workload

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// ShortCommand is B04. It measures the per-command overhead of the exec path
// on an already running sandbox.
//
// The command itself does nothing, so what the series measures is the exec
// transport: request setup, process spawn and output collection. It is
// reported alongside the transport name, because a per-command REST call and
// a multiplexed SSH session are not the same product decision.
type ShortCommand struct{ base }

// NewShortCommand builds B04.
func NewShortCommand() *ShortCommand {
	return &ShortCommand{base{
		name: "execution.short-command",
		dim:  DimExecution,
		sc:   ScaleSingle,
		desc: "Run a no-op command repeatedly on a live sandbox; reports exec-path overhead.",
	}}
}

// Run issues env.Repetitions no-op commands in series.
func (w *ShortCommand) Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error) {
	r := newResult(w, env)
	r.Profile = ProfilePersistent
	wall := metrics.NewSeries("exec_wall", "ms")
	inBox := metrics.NewSeries("exec_in_box", "ms")

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		res, err := ExecTimed(ctx, box, "true")
		if err != nil {
			r.Count("exec_errors", 1)
			continue
		}
		if !res.OK() {
			r.Count("nonzero_exit", 1)
			continue
		}
		wall.AddDuration(res.Wall)
		if res.InBox > 0 {
			inBox.AddDuration(res.InBox)
		}
	}

	r.AddSeries(wall)
	r.AddSeries(inBox)
	r.Count("ok", wall.Len())
	r.Elapsed = time.Since(r.Started)
	r.SetScalar("commands_per_second", metrics.Throughput{Count: wall.Len(), Elapsed: r.Elapsed}.PerSecond())
	if wall.Len() == 0 {
		return fail(r, errors.New("no command completed"))
	}
	return r, nil
}

// CPUBuild is B06. It measures compute with a fixed, self-contained C
// compilation and run, so the number does not depend on a package registry or
// on a toolchain version that differs between images.
type CPUBuild struct{ base }

// NewCPUBuild builds B06.
func NewCPUBuild() *CPUBuild {
	return &CPUBuild{base{
		name: "compute.cpu-build",
		dim:  DimCompute,
		sc:   ScaleSingle,
		desc: "Compile and run a fixed CPU-bound C program; reports in-sandbox completion time.",
	}}
}

// cpuSource is a fixed integer workload. It is written by the benchmark rather
// than fetched, so the measurement never includes a network download and
// never changes when an upstream package does.
const cpuSource = `
#include <stdio.h>
#include <stdint.h>
int main(void) {
  uint64_t h = 1469598103934665603ULL;
  for (uint64_t i = 0; i < 40000000ULL; i++) {
    h ^= i;
    h *= 1099511628211ULL;
  }
  printf("%llu\n", (unsigned long long)h);
  return 0;
}
`

// Run compiles and executes cpuSource once per repetition.
func (w *CPUBuild) Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error) {
	r := newResult(w, env)
	const src = "/tmp/sandboxbench-cpu.c"
	if err := box.WriteFile(ctx, src, []byte(cpuSource)); err != nil {
		return fail(r, fmt.Errorf("write %s: %w", src, err))
	}
	probe, err := box.Exec(ctx, sandbox.Shell("command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1"))
	if err != nil {
		return fail(r, err)
	}
	if !probe.OK() {
		r.Status = sandbox.StatusUnsupported
		r.Reason = "image has no C compiler on PATH"
		r.Elapsed = time.Since(r.Started)
		return r, nil
	}

	compile := metrics.NewSeries("compile", "ms")
	run := metrics.NewSeries("run", "ms")
	var source string

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		res, err := ExecTimed(ctx, box, "CC=$(command -v cc || command -v gcc); \"$CC\" -O2 -o /tmp/sandboxbench-cpu "+src)
		if err != nil {
			r.Count("compile_errors", 1)
			continue
		}
		if !res.OK() {
			return fail(r, errExit("compile", res))
		}
		d, src2 := best(res)
		source = src2
		compile.AddDuration(d)

		res, err = ExecTimed(ctx, box, "/tmp/sandboxbench-cpu >/dev/null")
		if err != nil {
			r.Count("run_errors", 1)
			continue
		}
		if !res.OK() {
			return fail(r, errExit("run", res))
		}
		d, _ = best(res)
		run.AddDuration(d)
		r.Count("ok", 1)
	}

	r.AddSeries(compile)
	r.AddSeries(run)
	r.Config = map[string]any{"timing_source": source}
	r.Elapsed = time.Since(r.Started)
	if run.Len() == 0 {
		return fail(r, errors.New("no repetition completed the CPU workload"))
	}
	return r, nil
}

// SmallFiles is B09. It measures filesystem metadata throughput, which is what
// an unpacked dependency tree actually costs, rather than sequential bandwidth.
type SmallFiles struct{ base }

// NewSmallFiles builds B09.
func NewSmallFiles() *SmallFiles {
	return &SmallFiles{base{
		name: "storage.small-files",
		dim:  DimStorage,
		sc:   ScaleSingle,
		desc: "Create, stat and delete 5,000 small files; reports metadata operations per second.",
	}}
}

// smallFileCount is fixed so the number is comparable across providers.
const smallFileCount = 5000

// Run performs one create/stat/delete pass per repetition.
func (w *SmallFiles) Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error) {
	r := newResult(w, env)
	create := metrics.NewSeries("create_5k_files", "ms")
	stat := metrics.NewSeries("stat_5k_files", "ms")
	del := metrics.NewSeries("delete_5k_files", "ms")

	const dir = "/tmp/sandboxbench-files"
	n := strconv.Itoa(smallFileCount)

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		steps := []struct {
			line   string
			series *metrics.Series
		}{
			{"rm -rf " + dir + " && mkdir -p " + dir + " && i=0; while [ $i -lt " + n + " ]; do printf 'x' > " + dir + "/f$i; i=$((i+1)); done", create},
			{"find " + dir + " -type f | while read -r f; do test -f \"$f\"; done", stat},
			{"rm -rf " + dir, del},
		}
		failed := false
		for _, s := range steps {
			res, err := ExecTimed(ctx, box, s.line)
			if err != nil {
				r.Count("exec_errors", 1)
				failed = true
				break
			}
			if !res.OK() {
				return fail(r, errExit("small-files step", res))
			}
			d, _ := best(res)
			s.series.AddDuration(d)
		}
		if !failed {
			r.Count("ok", 1)
		}
	}

	r.AddSeries(create)
	r.AddSeries(stat)
	r.AddSeries(del)
	if create.Len() > 0 {
		s := create.Summarize()
		r.SetScalar("create_ops_per_second", float64(smallFileCount)/(s.P50/1000))
	}
	r.Elapsed = time.Since(r.Started)
	if create.Len() == 0 {
		return fail(r, errors.New("no repetition completed the filesystem workload"))
	}
	return r, nil
}

// NetworkEgress is B12. It measures egress round-trip time and throughput from
// inside the sandbox to a fixed endpoint.
//
// The endpoint is a run parameter and must be reported, because the result
// depends on where the sandbox is and where the endpoint is. Comparing two
// providers on this number is only meaningful when both reach the same host.
type NetworkEgress struct {
	base
	Endpoint string
}

// NewNetworkEgress builds B12 against a fixed endpoint.
func NewNetworkEgress(endpoint string) *NetworkEgress {
	return &NetworkEgress{
		base: base{
			name: "network.egress",
			dim:  DimNetwork,
			sc:   ScaleSingle,
			desc: "Fetch a fixed endpoint from inside the sandbox; reports egress RTT and throughput.",
			req:  Requirements{Network: true},
		},
		Endpoint: endpoint,
	}
}

// Run issues env.Repetitions fetches of the configured endpoint.
func (w *NetworkEgress) Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error) {
	r := newResult(w, env)
	r.Config = map[string]any{"endpoint": w.Endpoint}

	probe, err := box.Exec(ctx, sandbox.Shell("command -v curl >/dev/null 2>&1"))
	if err != nil {
		return fail(r, err)
	}
	if !probe.OK() {
		r.Status = sandbox.StatusUnsupported
		r.Reason = "image has no curl on PATH"
		r.Elapsed = time.Since(r.Started)
		return r, nil
	}

	rtt := metrics.NewSeries("egress_rtt", "ms")
	thr := metrics.NewSeries("egress_throughput", "MB/s")

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		// curl reports its own timings, which excludes the shell and the exec
		// transport from the network measurement.
		line := "curl -sS -o /dev/null -w '%{time_total} %{speed_download}' --max-time 30 " + shellQuote(w.Endpoint)
		res, err := box.Exec(ctx, sandbox.Shell(line))
		if err != nil {
			r.Count("exec_errors", 1)
			continue
		}
		if !res.OK() {
			r.Count("fetch_errors", 1)
			continue
		}
		fields := strings.Fields(strings.TrimSpace(res.Stdout))
		if len(fields) != 2 {
			r.Count("parse_errors", 1)
			continue
		}
		secs, err1 := strconv.ParseFloat(fields[0], 64)
		bps, err2 := strconv.ParseFloat(fields[1], 64)
		if err1 != nil || err2 != nil {
			r.Count("parse_errors", 1)
			continue
		}
		rtt.Add(secs * 1000)
		thr.Add(bps / (1024 * 1024))
		r.Count("ok", 1)
	}

	r.AddSeries(rtt)
	r.AddSeries(thr)
	r.Elapsed = time.Since(r.Started)
	if rtt.Len() == 0 {
		return fail(r, errors.New("no fetch completed"))
	}
	return r, nil
}

// shellQuote wraps a value in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

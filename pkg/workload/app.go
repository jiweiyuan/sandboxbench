package workload

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// Application workloads are end-to-end: they describe what a developer or a
// coding agent waits for, and they deliberately mix network, CPU and
// filesystem. Their numbers belong in their own table and must never be folded
// into a per-dimension score, because a slow `npm ci` cannot be attributed to
// one resource.
//
// None of them call a model. A real LLM in the loop would add its own latency
// and non-determinism to a measurement of infrastructure.

// Step is one measured phase of an application workload.
type Step struct {
	Label string `json:"label" yaml:"label"`
	Line  string `json:"line" yaml:"line"`
}

// AppSpec declares an application workload from configuration.
//
// There is no default repository on purpose. A benchmark that pins somebody
// else's repository stops being reproducible the moment that repository moves,
// so the user pins a repository and a commit themselves and the pin is
// recorded in every result file.
type AppSpec struct {
	Name  string `json:"name" yaml:"name"`
	Repo  string `json:"repo" yaml:"repo"`
	Ref   string `json:"ref" yaml:"ref"`
	Dir   string `json:"dir" yaml:"dir"`
	Steps []Step `json:"steps" yaml:"steps"`
}

// Validate reports whether the spec can run.
func (s AppSpec) Validate() error {
	if s.Name == "" {
		return errors.New("application workload needs a name")
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("application workload %q has no steps", s.Name)
	}
	if s.Repo != "" && s.Ref == "" {
		return fmt.Errorf("application workload %q names a repo but no ref; pin a commit", s.Name)
	}
	return nil
}

// Application runs an AppSpec's steps in order and times each one.
type Application struct {
	base
	Spec AppSpec
}

// NewApplication builds an application workload from a spec.
func NewApplication(spec AppSpec) *Application {
	return &Application{
		base: base{
			name: "app." + spec.Name,
			dim:  DimApplication,
			sc:   ScaleSingle,
			desc: "End-to-end application workload: " + spec.Name,
			req:  Requirements{Network: spec.Repo != ""},
		},
		Spec: spec,
	}
}

// Run clones the pinned tree once per repetition, then runs each step.
func (w *Application) Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error) {
	r := newResult(w, env)
	if err := w.Spec.Validate(); err != nil {
		return fail(r, err)
	}
	r.Config = map[string]any{"repo": w.Spec.Repo, "ref": w.Spec.Ref, "dir": w.Spec.Dir}

	dir := w.Spec.Dir
	if dir == "" {
		dir = "/tmp/sandboxbench-app"
	}

	series := map[string]*metrics.Series{}
	order := make([]string, 0, len(w.Spec.Steps)+1)
	get := func(label string) *metrics.Series {
		if s, ok := series[label]; ok {
			return s
		}
		s := metrics.NewSeries(label, "ms")
		series[label] = s
		order = append(order, label)
		return s
	}
	total := metrics.NewSeries("total", "ms")

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		if w.Spec.Repo != "" {
			// A shallow clone at a pinned ref: the fetch is part of the
			// measurement, the repository's whole history is not.
			clone := fmt.Sprintf(
				"rm -rf %s && git init -q %s && cd %s && git remote add origin %s && git fetch -q --depth 1 origin %s && git checkout -q FETCH_HEAD",
				dir, dir, dir, shellQuote(w.Spec.Repo), shellQuote(w.Spec.Ref))
			res, err := ExecTimed(ctx, box, clone)
			if err != nil {
				r.Count("clone_errors", 1)
				continue
			}
			if !res.OK() {
				return fail(r, errExit("clone", res))
			}
			d, _ := best(res)
			get("clone").AddDuration(d)
		}

		var sum time.Duration
		broke := false
		for _, st := range w.Spec.Steps {
			res, err := ExecTimed(ctx, box, "cd "+dir+" && "+st.Line)
			if err != nil {
				r.Count("step_errors", 1)
				broke = true
				break
			}
			if !res.OK() {
				return fail(r, errExit("step "+st.Label, res))
			}
			d, _ := best(res)
			get(st.Label).AddDuration(d)
			sum += d
		}
		if broke {
			continue
		}
		total.AddDuration(sum)
		r.Count("ok", 1)
	}

	for _, label := range order {
		r.AddSeries(series[label])
	}
	r.AddSeries(total)
	r.Elapsed = time.Since(r.Started)
	if total.Len() == 0 {
		return fail(r, errors.New("no repetition completed every step"))
	}
	if env.Repetitions > 0 {
		r.SetScalar("completion_rate", float64(total.Len())/float64(env.Repetitions))
	}
	return r, nil
}

// StatefulContinue is application workload D. It writes work, suspends,
// resumes and checks that the work survived.
//
// It is the one application workload with a correctness result as well as a
// latency: a fast resume that lost the filesystem is not a fast resume.
type StatefulContinue struct{ base }

// NewStatefulContinue builds application workload D.
func NewStatefulContinue() *StatefulContinue {
	return &StatefulContinue{base{
		name: "app.stateful-continue",
		dim:  DimApplication,
		sc:   ScaleSingle,
		desc: "Write work, suspend, resume, and verify the work survived; reports resume latency and correctness.",
		req:  Requirements{Suspend: true, Resume: true, PersistFS: true},
	}}
}

// workTree is the state the workload expects to find again after the resume.
const workTree = "/tmp/sandboxbench-state"

// RunProvider runs one write-suspend-resume-verify cycle per repetition.
func (w *StatefulContinue) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	r.Profile = ProfileRecoverable
	resume := metrics.NewSeries("resume_to_ready", "ms")
	firstCmd := metrics.NewSeries("first_command_after_resume", "ms")

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		box, err := p.Create(ctx, env.Create)
		if err != nil {
			r.Count("create_errors", 1)
			continue
		}

		// 64 files and a running counter: enough state that a cold boot
		// masquerading as a resume would be caught.
		setup := "rm -rf " + workTree + " && mkdir -p " + workTree +
			" && i=0; while [ $i -lt 64 ]; do echo \"line $i\" > " + workTree + "/f$i; i=$((i+1)); done" +
			" && cat " + workTree + "/f* | cksum | cut -d' ' -f1 > " + workTree + "/checksum"
		if res, err := box.Exec(ctx, sandbox.Shell(setup)); err != nil || !res.OK() {
			r.Count("setup_errors", 1)
			destroyQuietly(ctx, box)
			continue
		}
		want, err := box.ReadFile(ctx, workTree+"/checksum")
		if err != nil {
			r.Count("setup_errors", 1)
			destroyQuietly(ctx, box)
			continue
		}

		if err := box.Suspend(ctx); err != nil {
			destroyQuietly(ctx, box)
			if errors.Is(err, sandbox.ErrUnsupported) {
				r.Status = sandbox.StatusUnsupported
				r.Reason = "provider reported suspend unsupported at run time"
				r.Elapsed = time.Since(r.Started)
				return r, nil
			}
			r.Count("suspend_errors", 1)
			continue
		}

		t0 := time.Now()
		if err := box.Resume(ctx); err != nil {
			r.Count("resume_errors", 1)
			destroyQuietly(ctx, box)
			continue
		}
		t1 := time.Now()
		res, err := box.Exec(ctx, sandbox.Shell("cat "+workTree+"/f* | cksum | cut -d' ' -f1"))
		if err != nil {
			r.Count("verify_errors", 1)
			destroyQuietly(ctx, box)
			continue
		}
		resume.AddDuration(t1.Sub(t0))
		firstCmd.AddDuration(time.Since(t1))

		if res.OK() && trimLine(res.Stdout) == trimLine(string(want)) {
			r.Count("state_preserved", 1)
		} else {
			r.Count("state_mismatch", 1)
		}
		destroyQuietly(ctx, box)
	}

	r.AddSeries(resume)
	r.AddSeries(firstCmd)
	if n := resume.Len(); n > 0 {
		r.SetScalar("state_correctness_rate", float64(r.Counters["state_preserved"])/float64(n))
	}
	r.Elapsed = time.Since(r.Started)
	if resume.Len() == 0 {
		return fail(r, errors.New("no cycle completed a resume"))
	}
	return r, nil
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

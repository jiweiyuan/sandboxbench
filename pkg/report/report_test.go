package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/runner"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/pkg/workload"
)

func sampleRun() *runner.Run {
	series := metrics.NewSeries("create_to_ready", "ms")
	for i := 1; i <= 12; i++ {
		series.Add(float64(i) * 10)
	}
	completed := workload.Result{
		Workload:  "lifecycle.cold-create",
		Dimension: workload.DimLifecycle,
		Scale:     workload.ScaleSingle,
		Status:    sandbox.StatusCompleted,
	}
	completed.AddSeries(series)

	return &runner.Run{
		SchemaVersion: runner.SchemaVersion,
		ID:            "unit",
		StartedAt:     time.Now().UTC(),
		FinishedAt:    time.Now().UTC(),
		Providers: []runner.ProviderRun{
			{
				Provider:     "alpha",
				Capabilities: sandbox.Capabilities{Suspend: true, Resume: true, ExecTransport: sandbox.TransportSSH},
				Results: []workload.Result{
					completed,
					{Workload: "lifecycle.resume", Status: sandbox.StatusUnsupported, Reason: "provider does not support suspend"},
				},
			},
			{
				Provider: "beta_x",
				Results: []workload.Result{
					{Workload: "lifecycle.cold-create", Status: sandbox.StatusFailed, Reason: "create: HTTP 500"},
				},
			},
		},
	}
}

func TestMarkdownPrintsEveryStatus(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, sampleRun()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"unsupported", "failed", "ssh", "lifecycle.cold-create", "## Failures", "HTTP 500"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown is missing %q:\n%s", want, out)
		}
	}
	// An unsupported cell must never be rendered as a number.
	if strings.Contains(out, "| 0.0 ms |") {
		t.Error("an unsupported result was rendered as a zero")
	}
}

func TestLaTeXEscapesProviderNames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteLaTeX(&buf, sampleRun()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `beta\_x`) {
		t.Errorf("underscore not escaped:\n%s", out)
	}
	if strings.Contains(out, "beta_x") {
		t.Errorf("raw underscore survived into the tabular:\n%s", out)
	}
	for _, want := range []string{`\toprule`, `\bottomrule`, `\textit{unsupported}`, `\textit{failed}`} {
		if !strings.Contains(out, want) {
			t.Errorf("latex is missing %q:\n%s", want, out)
		}
	}
}

func TestHeadlinePrefersTheNamedSeries(t *testing.T) {
	r := sampleRun().Providers[0].Results[0]
	if got := headline(r); got != "65.0 ms" {
		t.Errorf("headline = %q, want the median 65.0 ms", got)
	}
}

func TestHeadlineIsEmptyForUnsupported(t *testing.T) {
	if got := headline(workload.Result{Status: sandbox.StatusUnsupported}); got != "" {
		t.Errorf("headline = %q, want empty", got)
	}
}

func TestJSONRoundTripsRawSamples(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleRun()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"raw"`) {
		t.Error("result file does not archive raw samples, so no statistic can be recomputed")
	}
}

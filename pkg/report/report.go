// Package report renders a run into the three forms the project needs: JSON to
// archive, Markdown to read, and LaTeX to paste into the paper.
//
// Every renderer prints unsupported and failed benchmarks as such. A reader
// must be able to tell "this provider has no suspend" from "this provider
// resumes slowly", and a blank cell does not do that.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jiweiyuan/sandboxbench/pkg/runner"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/pkg/workload"
)

// WriteJSON writes the archival form, indented so that a diff between two runs
// is readable.
func WriteJSON(w io.Writer, run *runner.Run) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(run)
}

// row is one provider's cell for one benchmark.
type row struct {
	provider string
	status   sandbox.Status
	reason   string
	headline string
}

// headline picks the one number that represents a benchmark. Preferring the
// median over the mean is deliberate: sandbox create latency is long-tailed,
// and a mean of ten creates where one hit a cold pool describes neither case.
func headline(res workload.Result) string {
	if res.Status != sandbox.StatusCompleted {
		return ""
	}
	for _, name := range []string{
		"create_to_ready", "resume_to_ready", "exec_wall", "run",
		"create_5k_files", "egress_rtt", "burst_ready", "sustained_ready",
		"step_latency", "reset_latency", "total", "create_destroy_cycle",
	} {
		for _, s := range res.Series {
			if s.Name == name {
				return fmt.Sprintf("%.1f %s", s.P50, s.Unit)
			}
		}
	}
	for _, key := range []string{"slowdown", "ready_per_second", "transitions_per_second", "cycles_per_second"} {
		if v, ok := res.Scalars[key]; ok {
			return fmt.Sprintf("%.3f (%s)", v, key)
		}
	}
	if len(res.Series) > 0 {
		s := res.Series[0]
		return fmt.Sprintf("%.1f %s", s.P50, s.Unit)
	}
	return "-"
}

// cell renders one row for a table.
func (r row) cell() string {
	switch r.status {
	case sandbox.StatusCompleted:
		return r.headline
	case sandbox.StatusUnsupported:
		return "unsupported"
	case sandbox.StatusSkipped:
		return "skipped"
	default:
		return "failed"
	}
}

// matrix groups a run into benchmark rows and provider columns.
func matrix(run *runner.Run) (benchmarks []string, providers []string, cells map[string]map[string]row) {
	cells = map[string]map[string]row{}
	seenBench := map[string]bool{}
	for _, pr := range run.Providers {
		providers = append(providers, pr.Provider)
		for _, res := range pr.Results {
			if !seenBench[res.Workload] {
				seenBench[res.Workload] = true
				benchmarks = append(benchmarks, res.Workload)
			}
			if cells[res.Workload] == nil {
				cells[res.Workload] = map[string]row{}
			}
			cells[res.Workload][pr.Provider] = row{
				provider: pr.Provider,
				status:   res.Status,
				reason:   res.Reason,
				headline: headline(res),
			}
		}
	}
	sort.Strings(benchmarks)
	return benchmarks, providers, cells
}

// WriteMarkdown renders the leaderboard plus the capability matrix.
func WriteMarkdown(w io.Writer, run *runner.Run) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# SandboxBench run `%s`\n\n", run.ID)
	fmt.Fprintf(&b, "Started %s, finished %s.\n\n", run.StartedAt.Format("2006-01-02 15:04:05 MST"), run.FinishedAt.Format("15:04:05 MST"))
	fmt.Fprintf(&b, "Client: %s/%s, %d CPUs", run.Client.OS, run.Client.Arch, run.Client.NumCPU)
	if run.Client.Region != "" {
		fmt.Fprintf(&b, ", region %s", run.Client.Region)
	}
	b.WriteString(".\n")
	if run.Notes != "" {
		fmt.Fprintf(&b, "\nNotes: %s\n", run.Notes)
	}

	b.WriteString("\n## Capabilities\n\n")
	b.WriteString("| provider | exec transport | suspend | resume | snapshot | persistent FS | ports |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, pr := range run.Providers {
		c := pr.Capabilities
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			pr.Provider, c.ExecTransport, yesno(c.Suspend), yesno(c.Resume),
			yesno(c.Snapshot), yesno(c.PersistentFilesystem), yesno(c.PortForwarding))
	}

	benchmarks, providers, cells := matrix(run)
	b.WriteString("\n## Results\n\nMedian of the benchmark's headline series. ")
	b.WriteString("`unsupported` means the provider has no such capability, not a score of zero.\n\n")
	b.WriteString("| benchmark |")
	for _, p := range providers {
		fmt.Fprintf(&b, " %s |", p)
	}
	b.WriteString("\n|---|")
	for range providers {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, bench := range benchmarks {
		fmt.Fprintf(&b, "| `%s` |", bench)
		for _, p := range providers {
			r, ok := cells[bench][p]
			if !ok {
				b.WriteString(" - |")
				continue
			}
			fmt.Fprintf(&b, " %s |", r.cell())
		}
		b.WriteString("\n")
	}

	// Failures get their reasons printed in full. A table cell that says
	// "failed" without a reason is a dead end for whoever reads the run.
	var failures []string
	for _, pr := range run.Providers {
		if pr.Error != "" {
			failures = append(failures, fmt.Sprintf("- `%s`: provider could not be constructed: %s", pr.Provider, pr.Error))
		}
		for _, res := range pr.Results {
			if res.Status == sandbox.StatusFailed {
				failures = append(failures, fmt.Sprintf("- `%s` / `%s`: %s", pr.Provider, res.Workload, res.Reason))
			}
		}
	}
	if len(failures) > 0 {
		b.WriteString("\n## Failures\n\n")
		b.WriteString(strings.Join(failures, "\n"))
		b.WriteString("\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteLaTeX renders the results table as a booktabs tabular for paper/.
// It emits the table body only, so the paper controls the caption and label.
func WriteLaTeX(w io.Writer, run *runner.Run) error {
	benchmarks, providers, cells := matrix(run)
	var b strings.Builder

	fmt.Fprintf(&b, "%% Generated by sandboxbench report --format=latex from run %s.\n", run.ID)
	fmt.Fprintf(&b, "%% Do not edit by hand; regenerate instead.\n")
	b.WriteString("\\begin{tabular}{l" + strings.Repeat("r", len(providers)) + "}\n")
	b.WriteString("\\toprule\nBenchmark")
	for _, p := range providers {
		fmt.Fprintf(&b, " & %s", texEscape(p))
	}
	b.WriteString(" \\\\\n\\midrule\n")
	for _, bench := range benchmarks {
		fmt.Fprintf(&b, "\\texttt{%s}", texEscape(bench))
		for _, p := range providers {
			r, ok := cells[bench][p]
			if !ok {
				b.WriteString(" & ---")
				continue
			}
			if r.status == sandbox.StatusCompleted {
				fmt.Fprintf(&b, " & %s", texEscape(r.headline))
			} else {
				fmt.Fprintf(&b, " & \\textit{%s}", texEscape(string(r.status)))
			}
		}
		b.WriteString(" \\\\\n")
	}
	b.WriteString("\\bottomrule\n\\end{tabular}\n")

	_, err := io.WriteString(w, b.String())
	return err
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// texEscape protects the characters that would otherwise end the compile.
func texEscape(s string) string {
	r := strings.NewReplacer(
		"\\", "\\textbackslash{}",
		"&", "\\&",
		"%", "\\%",
		"$", "\\$",
		"#", "\\#",
		"_", "\\_",
		"{", "\\{",
		"}", "\\}",
		"~", "\\textasciitilde{}",
		"^", "\\textasciicircum{}",
	)
	return r.Replace(s)
}

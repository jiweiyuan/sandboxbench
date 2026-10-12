// Command sandboxbench runs the benchmark suite and renders its results.
//
//	sandboxbench list
//	sandboxbench run    --config configs/v01-single.yaml --out results/
//	sandboxbench report --in results/v01-single.json --format markdown
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jiweiyuan/sandboxbench/pkg/report"
	"github.com/jiweiyuan/sandboxbench/pkg/runner"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/pkg/workload"

	// Provider adapters register themselves. This import block is the only
	// place the binary decides which providers exist.
	_ "github.com/jiweiyuan/sandboxbench/providers/daytona"
	_ "github.com/jiweiyuan/sandboxbench/providers/e2b"
	_ "github.com/jiweiyuan/sandboxbench/providers/local"
	_ "github.com/jiweiyuan/sandboxbench/providers/runta"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "list":
		err = cmdList(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("sandboxbench", version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxbench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sandboxbench - a benchmark suite for cloud-based code execution sandboxes

usage:
  sandboxbench list
  sandboxbench run    --config FILE [--out DIR] [--region TEXT] [--quiet]
  sandboxbench report --in FILE [--format markdown|latex|json] [--out FILE]
  sandboxbench version
`)
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println("providers:")
	for _, name := range sandbox.Registered() {
		fmt.Printf("  %s\n", name)
	}
	fmt.Println("\nbenchmarks:")
	for _, name := range workload.Names() {
		w, err := workload.Lookup(name, nil)
		if err != nil {
			// A benchmark whose definition lives entirely in the config file,
			// such as app.custom, cannot be built without one.
			fmt.Printf("  %-28s %s\n", name, "(needs a with: block; see configs/)")
			continue
		}
		fmt.Printf("  %-28s %-12s %-8s %s\n", w.Name(), w.Dimension(), w.Scale(), w.Describe())
	}
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "run configuration file (required)")
	outDir := fs.String("out", "results", "directory for the result file")
	region := fs.String("region", "", "where the load is generated from, recorded with the results")
	quiet := fs.Bool("quiet", false, "suppress progress output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		fs.Usage()
		return fmt.Errorf("--config is required")
	}

	cfg, err := runner.Load(*cfgPath)
	if err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	if *quiet {
		logf = nil
	}

	// Interrupt cancels the run, and every adapter tears its sandboxes down on
	// a context that outlives the cancellation, so Ctrl-C does not leave
	// sandboxes billing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := &runner.Runner{Cfg: cfg, Logf: logf, Region: *region}
	run, err := r.Execute(ctx)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.json", cfg.ID, run.StartedAt.Format("20060102-150405"))
	path := filepath.Join(*outDir, name)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := report.WriteJSON(f, run); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "wrote", path)
	return report.WriteMarkdown(os.Stdout, run)
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "", "result file written by `sandboxbench run` (required)")
	format := fs.String("format", "markdown", "markdown, latex or json")
	out := fs.String("out", "", "write here instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		fs.Usage()
		return fmt.Errorf("--in is required")
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	var run runner.Run
	if err := json.Unmarshal(data, &run); err != nil {
		return fmt.Errorf("%s: %w", *in, err)
	}
	if run.SchemaVersion != runner.SchemaVersion {
		fmt.Fprintf(os.Stderr,
			"sandboxbench: warning: %s has schema version %d, this binary writes %d\n",
			*in, run.SchemaVersion, runner.SchemaVersion)
	}

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}

	switch *format {
	case "markdown", "md":
		return report.WriteMarkdown(w, &run)
	case "latex", "tex":
		return report.WriteLaTeX(w, &run)
	case "json":
		return report.WriteJSON(w, &run)
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
}

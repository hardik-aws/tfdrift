// Command tfdrift scans a path for Terraform/Terragrunt modules and
// reports which ones have drifted from their recorded state.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/hardik-aws/tfdrift/internal/discover"
	"github.com/hardik-aws/tfdrift/internal/model"
	"github.com/hardik-aws/tfdrift/internal/report"
	"github.com/hardik-aws/tfdrift/internal/runner"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("tfdrift", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pathFlag := fs.String("path", "", "directory to scan (overrides positional PATH arg)")
	tool := fs.String("tool", "terraform", "terraform | terragrunt")
	parallelism := fs.Int("parallelism", 4, "concurrent workers")
	detailed := fs.Bool("detailed", false, "parse plan JSON for per-resource drift")
	format := fs.String("format", "console", "console | json")
	reportMode := fs.String("report", "html", "file report: none | html | pdf | both")
	reportDir := fs.String("report-dir", "report", "directory for report files")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-directory plan timeout")
	logLevel := fs.String("log-level", "info", "log verbosity: debug | info | warn | error")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: tfdrift [PATH] [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *showVersion {
		info, _ := debug.ReadBuildInfo()
		fmt.Fprintln(stdout, versionString(effectiveVersion(version, info), commit, date))
		return 0
	}

	if *tool != "terraform" && *tool != "terragrunt" {
		fmt.Fprintf(stderr, "invalid --tool %q: want terraform or terragrunt\n", *tool)
		return 1
	}
	if *format != "console" && *format != "json" {
		fmt.Fprintf(stderr, "invalid --format %q: want console or json\n", *format)
		return 1
	}
	switch *reportMode {
	case "none", "html", "pdf", "both":
	default:
		fmt.Fprintf(stderr, "invalid --report %q: want none, html, pdf, or both\n", *reportMode)
		return 1
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	log := newLogger(stderr, level)

	path := resolvePath(*pathFlag, fs.Args())

	log.Info("scanning for modules", "path", path, "tool", *tool)
	mods, err := discover.Find(path, *tool)
	if err != nil {
		log.Error("discovery failed", "path", path, "error", err)
		return 1
	}
	if len(mods) == 0 {
		log.Warn("no modules found", "path", path, "tool", *tool)
		return 0
	}
	log.Info("discovered modules", "count", len(mods), "parallelism", *parallelism)

	detail := effectiveDetailed(*detailed, *reportMode)
	if detail && !*detailed {
		log.Debug("enabling per-resource detail for file report", "report", *reportMode)
	}
	results := runner.Run(context.Background(), mods, runner.Options{
		Commander:   runner.ExecCommander{},
		Parallelism: *parallelism,
		Detailed:    detail,
		Timeout:     *timeout,
		Logger:      log,
	})

	sum := summarize(results)
	log.Info("scan complete", "total", len(results), "clean", sum.clean, "drift", sum.drift, "error", sum.err)

	switch *format {
	case "json":
		if err := report.JSON(stdout, results); err != nil {
			fmt.Fprintf(stderr, "report: %v\n", err)
			return 1
		}
	default:
		report.Console(stdout, results)
	}

	paths, err := report.WriteReports(*reportDir, *reportMode, results, time.Now())
	if err != nil {
		log.Error("writing report failed", "error", err)
		return 1
	}
	for _, p := range paths {
		log.Info("wrote report", "path", p)
	}

	return model.ExitCode(results)
}

// summary counts results by status for a final log line.
type summary struct{ clean, drift, err int }

func summarize(results []model.Result) summary {
	var s summary
	for _, r := range results {
		switch r.Status {
		case model.StatusClean:
			s.clean++
		case model.StatusDrift:
			s.drift++
		case model.StatusError:
			s.err++
		}
	}
	return s
}

// effectiveDetailed forces per-resource detail whenever a file report is
// requested, since HTML/PDF reports are organised around individual resources.
func effectiveDetailed(detailed bool, reportMode string) bool {
	return detailed || reportMode != "none"
}

// resolvePath picks the scan directory: --path flag wins, then the first
// positional arg, then the current directory.
func resolvePath(flag string, posArgs []string) string {
	if flag != "" {
		return flag
	}
	if len(posArgs) > 0 {
		return posArgs[0]
	}
	return "."
}

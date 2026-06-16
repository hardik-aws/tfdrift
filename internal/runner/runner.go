// Package runner executes init+plan against modules in a bounded worker pool.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hardik-aws/tfdrift/internal/model"
)

// Commander runs a command in dir and returns stdout, stderr, exit code, err.
// err is non-nil only for failures to start/execute (not for non-zero exits).
type Commander interface {
	Run(ctx context.Context, dir, name string, args ...string) (stdout, stderr []byte, exitCode int, err error)
}

// Options configures a Run.
type Options struct {
	Commander   Commander
	Parallelism int
	Detailed    bool
	Timeout     time.Duration
	Logger      *slog.Logger // nil disables logging
}

// Run evaluates every module and returns one Result per module.
func Run(ctx context.Context, mods []model.Module, opts Options) []model.Result {
	if opts.Parallelism < 1 {
		opts.Parallelism = 1
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	work := make(chan model.Module)
	out := make(chan model.Result)

	var wg sync.WaitGroup
	for i := 0; i < opts.Parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range work {
				out <- evaluate(ctx, m, opts)
			}
		}()
	}
	go func() {
		for _, m := range mods {
			work <- m
		}
		close(work)
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	var results []model.Result
	for r := range out {
		results = append(results, r)
	}
	return results
}

func evaluate(ctx context.Context, m model.Module, opts Options) model.Result {
	start := time.Now()
	r := model.Result{Dir: m.Dir, Tool: m.Tool}
	opts.Logger.Debug("evaluating module", "dir", m.Dir, "tool", m.Tool)
	defer func() {
		r.Duration = time.Since(start)
		logResult(opts.Logger, r)
	}()

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// init
	if _, stderr, code, err := opts.Commander.Run(ctx, m.Dir, m.Tool, "init", "-input=false"); err != nil || code != 0 {
		r.Status = model.StatusError
		r.Err = errMsg("init", err, stderr, code)
		return r
	}

	// plan. In detailed mode write a plan file so `show -json` can read it.
	planArgs := []string{"plan", "-detailed-exitcode", "-input=false", "-lock=false"}
	if opts.Detailed {
		planArgs = append(planArgs, "-out=tfplan")
	}
	_, stderr, code, err := opts.Commander.Run(ctx, m.Dir, m.Tool, planArgs...)
	switch {
	case err != nil:
		r.Status = model.StatusError
		r.Err = errMsg("plan", err, stderr, code)
		return r
	case code == 0:
		r.Status = model.StatusClean
		return r
	case code == 2:
		r.Status = model.StatusDrift
	default:
		r.Status = model.StatusError
		r.Err = errMsg("plan", nil, stderr, code)
		return r
	}

	if opts.Detailed {
		if drifted, derr := detailed(ctx, m, opts); derr != nil {
			r.Err = derr.Error()
		} else {
			r.Drifted = drifted
		}
	}
	return r
}

// detailed re-runs plan to JSON and extracts per-resource changes.
func detailed(ctx context.Context, m model.Module, opts Options) ([]model.ResourceChange, error) {
	stdout, stderr, code, err := opts.Commander.Run(ctx, m.Dir, m.Tool, "show", "-json", "tfplan")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("%s", errMsg("show", err, stderr, code))
	}
	var parsed struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string       `json:"actions"`
				Before  map[string]any `json:"before"`
				After   map[string]any `json:"after"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(stdout, &parsed); err != nil {
		return nil, fmt.Errorf("parse plan json: %w", err)
	}
	var out []model.ResourceChange
	for _, rc := range parsed.ResourceChanges {
		action := classifyAction(rc.Change.Actions)
		if action == "" {
			continue // no-op
		}
		out = append(out, model.ResourceChange{
			Address: rc.Address,
			Action:  action,
			Changed: changedKeys(rc.Change.Before, rc.Change.After),
		})
	}

	// attach the human-readable plan diff for each resource, best-effort
	if text, _, code, err := opts.Commander.Run(ctx, m.Dir, m.Tool, "show", "-no-color", "tfplan"); err == nil && code == 0 {
		details := parsePlanDetails(string(text))
		for i := range out {
			out[i].Detail = details[out[i].Address]
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// classifyAction maps terraform plan actions to a human verb. "" means no-op.
func classifyAction(actions []string) string {
	switch {
	case len(actions) == 0, len(actions) == 1 && actions[0] == "no-op":
		return ""
	case len(actions) == 2: // ["create","delete"] or ["delete","create"]
		return "replace"
	default:
		switch actions[0] {
		case "create":
			return "create"
		case "delete":
			return "delete"
		case "update":
			return "update"
		case "read":
			return "read"
		default:
			return actions[0]
		}
	}
}

// changedKeys returns top-level attribute names whose values differ.
func changedKeys(before, after map[string]any) []string {
	seen := map[string]bool{}
	for k, bv := range before {
		if !reflect.DeepEqual(bv, after[k]) {
			seen[k] = true
		}
	}
	for k, av := range after {
		if _, ok := before[k]; !ok && av != nil {
			seen[k] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// logResult emits one line per module at a level matching its status.
func logResult(l *slog.Logger, r model.Result) {
	attrs := []any{"dir", r.Dir, "tool", r.Tool, "status", string(r.Status), "duration", r.Duration}
	switch r.Status {
	case model.StatusDrift:
		if len(r.Drifted) > 0 {
			attrs = append(attrs, "resources", len(r.Drifted))
		}
		l.Warn("drift detected", attrs...)
	case model.StatusError:
		l.Error("module failed", append(attrs, "error", r.Err)...)
	default:
		l.Info("module clean", attrs...)
	}
}

func errMsg(phase string, err error, stderr []byte, code int) string {
	s := strings.TrimSpace(string(stderr))
	switch {
	case err != nil && s != "":
		return fmt.Sprintf("%s: %v: %s", phase, err, s)
	case err != nil:
		return fmt.Sprintf("%s: %v", phase, err)
	case s != "":
		return fmt.Sprintf("%s exit %d: %s", phase, code, s)
	default:
		return fmt.Sprintf("%s exit %d", phase, code)
	}
}

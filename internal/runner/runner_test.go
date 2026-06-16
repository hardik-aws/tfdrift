package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hardik-aws/tfdrift/internal/model"
)

// fakeCommander returns scripted exit codes/output keyed by "dir:subcommand".
type fakeCommander struct {
	mu      sync.Mutex
	active  int32
	maxSeen int32
	exit    map[string]int    // key "dir/plan" etc -> exit code
	stdout  map[string]string // key -> stdout
	stderr  map[string]string // key -> stderr
}

func (f *fakeCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, int, error) {
	n := atomic.AddInt32(&f.active, 1)
	for {
		m := atomic.LoadInt32(&f.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxSeen, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.active, -1)

	sub := args[0]
	key := dir + "/" + sub
	if sub == "show" {
		key = dir + "/show" // human plan text
		for _, a := range args {
			if a == "-json" {
				key = dir + "/show-json"
				break
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.stdout[key]), []byte(f.stderr[key]), f.exit[key], nil
}

func resultByDir(rs []model.Result, dir string) model.Result {
	for _, r := range rs {
		if r.Dir == dir {
			return r
		}
	}
	return model.Result{}
}

func TestRunMapsExitCodes(t *testing.T) {
	mods := []model.Module{
		{Dir: "clean", Tool: "terraform"},
		{Dir: "drift", Tool: "terraform"},
		{Dir: "broken", Tool: "terraform"},
	}
	fc := &fakeCommander{
		exit: map[string]int{
			"clean/init": 0, "clean/plan": 0,
			"drift/init": 0, "drift/plan": 2,
			"broken/init": 0, "broken/plan": 1,
		},
		stderr: map[string]string{"broken/plan": "boom"},
	}
	rs := Run(context.Background(), mods, Options{Commander: fc, Parallelism: 2})

	if got := resultByDir(rs, "clean").Status; got != model.StatusClean {
		t.Errorf("clean status = %q", got)
	}
	if got := resultByDir(rs, "drift").Status; got != model.StatusDrift {
		t.Errorf("drift status = %q", got)
	}
	br := resultByDir(rs, "broken")
	if br.Status != model.StatusError {
		t.Errorf("broken status = %q", br.Status)
	}
	if !strings.Contains(br.Err, "boom") {
		t.Errorf("broken err = %q, want contains boom", br.Err)
	}
}

func TestRunInitFailureIsError(t *testing.T) {
	mods := []model.Module{{Dir: "d", Tool: "terraform"}}
	fc := &fakeCommander{
		exit:   map[string]int{"d/init": 1, "d/plan": 0},
		stderr: map[string]string{"d/init": "init failed"},
	}
	rs := Run(context.Background(), mods, Options{Commander: fc, Parallelism: 1})
	if rs[0].Status != model.StatusError {
		t.Fatalf("status = %q, want error", rs[0].Status)
	}
	if !strings.Contains(rs[0].Err, "init failed") {
		t.Errorf("err = %q", rs[0].Err)
	}
}

func TestRunDetailedParsesResources(t *testing.T) {
	planJSON := `{"resource_changes":[
		{"address":"aws_s3_bucket.a","change":{"actions":["update"],
			"before":{"acl":"private","tags":{"a":"1"}},
			"after":{"acl":"public","tags":{"a":"1"}}}},
		{"address":"aws_iam_role.b","change":{"actions":["no-op"]}},
		{"address":"aws_instance.c","change":{"actions":["delete","create"]}},
		{"address":"aws_bucket.d","change":{"actions":["create"]}}
	]}`
	planText := `Terraform will perform the following actions:

  # aws_s3_bucket.a will be updated in-place
  ~ resource "aws_s3_bucket" "a" {
      ~ acl = "private" -> "public"
    }

  # aws_bucket.d will be created
  + resource "aws_bucket" "d" {}

Plan: 1 to add, 1 to change, 0 to destroy.
`
	mods := []model.Module{{Dir: "d", Tool: "terraform"}}
	fc := &fakeCommander{
		exit:   map[string]int{"d/init": 0, "d/plan": 2, "d/show-json": 0, "d/show": 0},
		stdout: map[string]string{"d/show-json": planJSON, "d/show": planText},
	}
	rs := Run(context.Background(), mods, Options{Commander: fc, Parallelism: 1, Detailed: true})
	got := rs[0].Drifted // sorted by address, no-op excluded
	want := []model.ResourceChange{
		{Address: "aws_bucket.d", Action: "create"},
		{Address: "aws_instance.c", Action: "replace"},
		{Address: "aws_s3_bucket.a", Action: "update", Changed: []string{"acl"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Address != want[i].Address || got[i].Action != want[i].Action {
			t.Errorf("change[%d] = %+v, want %+v", i, got[i], want[i])
		}
		if strings.Join(got[i].Changed, ",") != strings.Join(want[i].Changed, ",") {
			t.Errorf("change[%d].Changed = %v, want %v", i, got[i].Changed, want[i].Changed)
		}
	}
	// per-resource human diff text attached where the plan provided it
	if d := got[2].Detail; !strings.Contains(d, `~ acl = "private" -> "public"`) {
		t.Errorf("aws_s3_bucket.a Detail missing diff body:\n%s", d)
	}
	if d := got[1].Detail; d != "" {
		t.Errorf("aws_instance.c had no plan-text block, Detail should be empty, got:\n%s", d)
	}
}

func TestRunRespectsParallelism(t *testing.T) {
	var mods []model.Module
	exit := map[string]int{}
	for i := 0; i < 20; i++ {
		d := fmt.Sprintf("d%d", i)
		mods = append(mods, model.Module{Dir: d, Tool: "terraform"})
		exit[d+"/init"] = 0
		exit[d+"/plan"] = 0
	}
	fc := &fakeCommander{exit: exit}
	Run(context.Background(), mods, Options{Commander: fc, Parallelism: 3})
	if fc.maxSeen > 3 {
		t.Errorf("max concurrent = %d, want <= 3", fc.maxSeen)
	}
}

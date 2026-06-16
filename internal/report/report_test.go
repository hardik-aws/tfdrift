package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hardik-aws/tfdrift/internal/model"
)

func sample() []model.Result {
	return []model.Result{
		{Dir: "svc-a", Tool: "terraform", Status: model.StatusClean},
		{Dir: "svc-b", Tool: "terragrunt", Status: model.StatusDrift, Drifted: []model.ResourceChange{
			{Address: "aws_s3_bucket.x", Action: "update", Changed: []string{"acl", "tags"},
				Detail: "# aws_s3_bucket.x will be updated in-place\n  ~ resource \"aws_s3_bucket\" \"x\" {\n      ~ acl = \"private\" -> \"public\"\n    }"},
			{Address: "aws_instance.y", Action: "replace"},
		}},
		{Dir: "svc-c", Tool: "terraform", Status: model.StatusError, Err: "init exit 1: boom"},
	}
}

func TestConsoleContainsRows(t *testing.T) {
	var buf bytes.Buffer
	Console(&buf, sample())
	out := buf.String()
	for _, want := range []string{"svc-a", "clean", "svc-b", "drift", "aws_s3_bucket.x", "svc-c", "error"} {
		if !strings.Contains(out, want) {
			t.Errorf("console output missing %q\n%s", want, out)
		}
	}
}

func TestJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	var got []model.Result
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(got) != 3 || got[1].Status != model.StatusDrift {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

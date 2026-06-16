# tfdrift

Recursively scan a path for Terraform or Terragrunt root modules, run
`init` + `plan` across them in parallel, and report which directories have
**drifted** from their recorded state.

Built for CI gating and local audits.

## Install

**Download a prebuilt binary** from the [Releases page](https://github.com/hardik-aws/tfdrift/releases).
Each release ships archives for linux / darwin / windows × amd64 / arm64 plus a
`checksums.txt`:

```bash
# example: linux amd64, v1.0.0
curl -sSL https://github.com/hardik-aws/tfdrift/releases/download/v1.0.0/tfdrift_1.0.0_linux_amd64.tar.gz | tar xz
sudo mv tfdrift /usr/local/bin/
tfdrift --version
```

Or with `go install`:

```bash
go install github.com/hardik-aws/tfdrift/cmd/tfdrift@latest
```

Or build from source:

```bash
go build -o tfdrift ./cmd/tfdrift
```

Requires the `terraform` and/or `terragrunt` binary on `PATH`.

## Usage

```
tfdrift [PATH] [flags]
```

`PATH` is the root directory to scan recursively (default `.`). It can be given
as a positional arg or with `--path`; if both are set, `--path` wins.

```bash
tfdrift ./infra          # positional
tfdrift --path=./infra   # flag (equivalent)
```

| Flag | Default | Description |
|------|---------|-------------|
| `--path` | _(cwd)_ | Directory to scan; overrides positional `PATH` |
| `--tool` | `terraform` | `terraform` or `terragrunt` (global; applies to whole run) |
| `--parallelism` | `4` | Concurrent workers |
| `--detailed` | `false` | Parse plan JSON to list each drifted resource |
| `--format` | `console` | stdout format: `console` or `json` |
| `--report` | `html` | file report: `none`, `html`, `pdf`, or `both` |
| `--report-dir` | `report` | directory for report files |
| `--log-level` | `info` | log verbosity: `debug`, `info`, `warn`, `error` |
| `--timeout` | `10m` | Per-directory init+plan timeout |
| `--version` | | Print version (`tfdrift <ver> (<commit>) built <date>`) and exit |

### Logging

Structured logs (`log/slog`, text format) go to **stderr** — stdout stays clean
for `--format=json`. Default `info` shows progress: scan start, modules
discovered, one line per module (clean=INFO, drift=WARN, error=ERROR), and a
final summary. `--log-level=debug` adds per-module "evaluating" lines; `warn`
or `error` quiet the run down to problems only.

```bash
tfdrift --log-level=debug ./infra        # verbose
tfdrift --log-level=error --format=json  # quiet, only failures logged
```

### Reports

`--format` controls **stdout**; `--report` writes a styled **file report** to
`--report-dir` (default `report/`), independently. An HTML report is generated
by default; pass `--report=none` to disable, `--report=pdf`, or `--report=both`.

| Mode | Files written to `report/` |
|------|-----------------------------|
| `none` | _(none)_ |
| `html` | `drift-report.html` |
| `pdf` | `drift-report.pdf` |
| `both` | `drift-report.html` + `drift-report.pdf` |

The HTML report has a client-side **search box** that filters at the
**individual resource** level — type `aws_iam_policy` or `s3_access` to show only
matching resource rows (matched against address, action, and plan diff), hiding
non-matching rows and any module left empty; clean/error modules match on their
directory and message. Plus **status filter** buttons
(All / Drift / Error / Clean) — no server, all in-page JS. The PDF is produced
by a pure-Go engine ([go-pdf/fpdf](https://github.com/go-pdf/fpdf)) — no external
binary required, but it has no search/filter. Existing files are overwritten each run.

### Exit codes

Mirrors Terraform's `-detailed-exitcode`:

| Code | Meaning |
|------|---------|
| `0` | All modules clean |
| `2` | At least one module drifted (no errors) |
| `1` | At least one module errored (or bad flags / unreadable path) |

Aggregation precedence: any error → `1`, else any drift → `2`, else `0`.

## How it works

1. **Discover** — walk `PATH`; a directory is a module when it contains
   `*.tf` (terraform) or `terragrunt.hcl` (terragrunt). Hidden directories
   and `.terraform/` / `.git/` are skipped.
2. **Run** — a bounded worker pool (`--parallelism`) runs per module:
   - `init -input=false`
   - `plan -detailed-exitcode -input=false -lock=false`
     (exit `0` → clean, `2` → drift, other → error)
   - with `--detailed`: re-plan to `tfplan`, `show -json tfplan` to collect
     every resource whose `change.actions` is not `["no-op"]`, plus
     `show -no-color tfplan` to capture each resource's human-readable diff
     block (matched to its address).
   One module failing never aborts the run.
3. **Report** — console table or JSON, plus optional HTML/PDF files, then set
   the process exit code. HTML/PDF group output into **one section per module**:
   a header band (directory, tool, status badge, drifted-resource count) above a
   per-resource table with columns **Action, Resource, Plan detail** — Plan
   detail being the raw `terraform plan` diff for that resource. Clean modules
   show a "No drift detected" note; errored modules show their error message.
   When a file report is requested, per-resource detail is collected
   automatically (no `--detailed` needed).

## Examples

Scan current dir, human-readable table:

```bash
tfdrift
```

Scan a Terragrunt repo with per-resource detail, 8 workers:

```bash
tfdrift --tool=terragrunt --detailed --parallelism=8 ./live
```

CI gate — fail the build on any drift, machine-readable output:

```bash
tfdrift --format=json ./infra > drift.json
# exit 2 if drift, 1 on error
```

Sample console output:

```
DIR            TOOL        STATUS  DETAIL
.              terraform   clean
svc-a          terraform   drift   update aws_s3_bucket.logs (acl, tags); replace aws_iam_role.app
svc-b/nested   terraform   error   init exit 1: backend init failed
```

With `--detailed`, the console renders each drifted resource compactly as
`<action> <address> (<changed attributes>)`. The HTML and PDF reports instead
group resources into **per-module sections**: each module gets a header band
(directory, tool, status, count) and, for drifted modules, a table with one row
per resource (Action, Resource, Plan detail) using a color-coded action badge
(create / update / replace / delete / read) and the full raw plan diff. Clean
modules render a "No drift detected" note and errored modules an error box. PDF
is landscape A4 to fit the diff. JSON's `drifted[]` carries the same `detail`
text per resource. Requesting any file report (`--report=html|pdf|both`)
auto-enables per-resource detail collection.

## Development

```bash
go test ./...     # unit tests (fake Commander; no real terraform needed)
go vet ./...
gofmt -l .
```

Design spec: [docs/superpowers/specs/2026-06-16-terraform-drift-detect-design.md](docs/superpowers/specs/2026-06-16-terraform-drift-detect-design.md)

### Releasing

Versioning follows [SemVer](https://semver.org), starting at `1.0.0`. Releases are
cut by pushing a `v*` tag **whose commit is on `main`** — the
[`release` workflow](.github/workflows/release.yml) verifies the tagged commit is an
ancestor of `origin/main` and fails fast otherwise, then runs tests and
[GoReleaser](https://goreleaser.com), which cross-compiles all targets, builds archives
+ `checksums.txt`, and publishes a GitHub Release. Tags on feature branches are rejected.

```bash
git checkout main
git tag v1.0.0
git push origin v1.0.0   # triggers .github/workflows/release.yml
```

Version metadata is injected at link time (`-X main.version/commit/date`); a plain
`go build` reports `dev`. Config lives in [.goreleaser.yaml](.goreleaser.yaml).

### Layout

```
cmd/tfdrift/    CLI entrypoint, flag parsing, exit code
internal/discover/   recursive module discovery
internal/runner/     worker pool, init+plan, exec wrapper (injectable Commander)
internal/report/     console table + JSON + HTML/PDF rendering
internal/model/      shared types + exit-code aggregation
```

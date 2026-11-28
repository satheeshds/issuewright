package checks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satheeshds/issuewright/internal/config"
	"gopkg.in/yaml.v3"
)

func TestMatchGlobs(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"services/api/**/*.go", "services/api/internal/auth/handler.go", true},
		{"services/api/**/*.go", "services/api/main.go", true},
		{"services/api/**/*.go", "web/src/App.tsx", false},
		{"**/*.dart", "mobile/lib/main.dart", true},
		{"**/*.dart", "main.dart", true},
		{"db/*.go", "db/users.go", true},
		{"db/*.go", "db/nested/users.go", false},
		{".env", ".env", true},
		{".env.*", ".env.local", true},
		{".env.*", "config/.env.local", false},
		{"services/api/internal/*/handler.go", "services/api/internal/cart/handler.go", true},
		{"services/api/internal/*/handler.go", "services/api/internal/a/b/handler.go", false},
		{"contracts/**", "contracts/generated/client/lib/api.dart", true},
		{"web/**", "web", false},
		{"**", "anything/at/all.txt", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+" "+tc.path, func(t *testing.T) {
			if got := Match(tc.pattern, tc.path); got != tc.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestSelectPicksChecksForChangedPaths(t *testing.T) {
	prof := &config.Profile{
		Version: 1,
		Checks: []config.Check{
			{Name: "go", Paths: []string{"services/api/**/*.go"}, Run: []string{"true"}},
			{Name: "web", Paths: []string{"web/**"}, Run: []string{"true"}},
			{Name: "mobile", Paths: []string{"**/*.dart"}, Run: []string{"true"}},
		},
	}
	got := Select(prof, []string{"web/src/App.tsx", "mobile/lib/a.dart"})
	if len(got) != 2 || got[0].Name != "web" || got[1].Name != "mobile" {
		t.Fatalf("selected = %+v", got)
	}
	if n := len(Select(prof, []string{"README.md"})); n != 0 {
		t.Fatalf("docs-only change selected %d checks", n)
	}
	if n := len(Select(nil, []string{"a.go"})); n != 0 {
		t.Fatalf("nil profile selected %d checks", n)
	}
}

// newGitRepo makes a worktree with one committed file so drift can be tested.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db", "gen.go"), []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	return dir
}

func TestRunCheckPassesAndFails(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{Dir: dir, Timeout: 30 * time.Second})
	ok := r.RunCheck(context.Background(), config.Check{Name: "ok", Run: []string{"true"}})
	if !ok.OK {
		t.Fatalf("true failed: %+v", ok)
	}
	bad := r.RunCheck(context.Background(), config.Check{Name: "bad", Run: []string{"echo boom; exit 3"}})
	if bad.OK || len(bad.Failures) == 0 {
		t.Fatalf("failure not reported: %+v", bad)
	}
	if !strings.Contains(bad.Failures[0], "boom") {
		t.Errorf("failure should carry output: %q", bad.Failures[0])
	}
}

func TestRunCheckTimeout(t *testing.T) {
	r := New(Options{Dir: t.TempDir(), Timeout: 200 * time.Millisecond})
	res := r.RunCheck(context.Background(), config.Check{Name: "slow", Run: []string{"sleep 5"}})
	if res.OK || !strings.Contains(res.Failures[0], "timed out") {
		t.Fatalf("timeout not reported: %+v", res)
	}
}

func TestDriftGateCatchesHandEdits(t *testing.T) {
	dir := newGitRepo(t)
	r := New(Options{Dir: dir})
	check := config.Check{
		Name:  "codegen",
		Paths: []string{"**/*.go"},
		Run:   []string{"true"}, // regeneration is a no-op here
		Drift: []string{"db/*.go"},
	}
	// Clean tree: passes.
	if res := r.RunCheck(context.Background(), check); !res.OK {
		t.Fatalf("clean tree failed the drift gate: %+v", res)
	}
	// Hand edit the generated file: fails.
	if err := os.WriteFile(filepath.Join(dir, "db", "gen.go"), []byte("edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := r.RunCheck(context.Background(), check)
	if res.OK {
		t.Fatal("hand-edited generated file passed the drift gate")
	}
	if !strings.Contains(res.Failures[0], "edited by hand") || !strings.Contains(res.Failures[0], "db/gen.go") {
		t.Errorf("unhelpful drift failure: %q", res.Failures[0])
	}
}

func TestDriftGateAllowsRegeneration(t *testing.T) {
	dir := newGitRepo(t)
	r := New(Options{Dir: dir})
	check := config.Check{
		Name:  "codegen",
		Run:   []string{"echo regenerated > db/gen.go"},
		Drift: []string{"db/*.go"},
	}
	// The command itself rewrites the file and leaves it dirty: that is also a
	// failure, because the committed content does not match a regeneration.
	res := r.RunCheck(context.Background(), check)
	if res.OK {
		t.Fatal("a dirty generated file after regeneration must fail: it means the branch did not commit the output")
	}
}

func TestSecretAndDeniedWrites(t *testing.T) {
	changed := []string{".env", ".env.local", "db/gen.go", "internal/a.go"}
	if got := SecretFiles([]string{".env", ".env.*"}, changed); len(got) != 2 {
		t.Errorf("secrets = %v", got)
	}
	if got := DeniedWrites([]string{"db/*.go"}, changed); len(got) != 1 || got[0] != "db/gen.go" {
		t.Errorf("denied = %v", got)
	}
}

func TestForbiddenCommand(t *testing.T) {
	never := []string{"make db-reset", "make db-push"}
	cases := map[string]bool{
		"make db-reset":                    true,
		"make db-reset && echo done":       true,
		"cd services/api && make db-reset": true,
		"make   db-reset":                  true,
		"make migrations":                  false,
		"make db-push":                     true,
		"make test":                        false,
	}
	for cmd, want := range cases {
		_, got := ForbiddenCommand(never, cmd)
		if got != want {
			t.Errorf("ForbiddenCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestScanToolCallsEnforcesNeverRun(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.jsonl")
	body := strings.Join([]string{
		`{"type":"message_update","usage":{"totalTokens":5}}`,
		`{"type":"tool_execution_start","toolCallId":"1","toolName":"bash","args":{"command":"go test ./..."}}`,
		`{"type":"tool_execution_start","toolCallId":"2","toolName":"bash","args":{"command":"make db-reset && echo ok"}}`,
		`{"type":"tool_execution_end","toolCallId":"2","toolName":"bash","isError":false}`,
	}, "\n")
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	calls, err := ScanToolCalls(logPath)
	if err != nil {
		t.Fatalf("ScanToolCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	violations := ForbiddenCalls(calls, []string{"make db-reset"})
	if len(violations) != 1 || !strings.Contains(violations[0], "make db-reset") {
		t.Fatalf("violations = %v", violations)
	}
}

func TestSummaryAndFailed(t *testing.T) {
	results := []Result{
		{Name: "go", Commands: []string{"go test"}, OK: true},
		{Name: "admin", OK: false, Failures: []string{"npm test: exit 1"}},
	}
	if !Failed(results) {
		t.Error("Failed() missed a failure")
	}
	s := Summary(results)
	if !strings.Contains(s, "✅ `go`") || !strings.Contains(s, "❌ `admin`") {
		t.Errorf("summary = %q", s)
	}
	if Summary(nil) == "" {
		t.Error("empty summary should say so")
	}
}

func TestAdvisoryChecksAreReportedNotBlocking(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{Dir: dir})
	advisory := r.RunCheck(context.Background(), config.Check{
		Name: "baseline-red", Advisory: true, Run: []string{"echo 33 tests failed; exit 1"},
	})
	blocking := r.RunCheck(context.Background(), config.Check{
		Name: "must-pass", Run: []string{"true"},
	})
	results := []Result{advisory, blocking}
	if Failed(results) {
		t.Fatal("an advisory failure must not fail the run")
	}
	if advisory.OK {
		t.Error("an advisory check that failed must still report OK=false")
	}
	summary := Summary(results)
	if !strings.Contains(summary, "⚠️ `baseline-red` (advisory") {
		t.Errorf("summary should mark the advisory failure: %q", summary)
	}
	if !strings.Contains(summary, "✅ `must-pass`") {
		t.Errorf("summary lost the passing check: %q", summary)
	}

	// A real failure still blocks.
	if !Failed([]Result{blocking, {Name: "hard", Failures: []string{"boom"}}}) {
		t.Error("a non-advisory failure must block")
	}
}

func TestAdvisorySurvivesProfileParsing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(`
version: 1
checks:
  - name: mobile
    paths: ["**/*.dart"]
    run: ["flutter test"]
    advisory: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	prof := &config.Profile{}
	raw, _ := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err := yamlUnmarshal(raw, prof); err != nil {
		t.Fatal(err)
	}
	if len(prof.Checks) != 1 || !prof.Checks[0].Advisory {
		t.Fatalf("advisory not parsed: %+v", prof.Checks)
	}
}

func yamlUnmarshal(raw []byte, v any) error { return yaml.Unmarshal(raw, v) }

// `.env.example` matches `.env.*` but is a template: blocking it stops a run that
// meant no harm, which is exactly what happened on a live issue.
func TestSecretFilesExceptTemplates(t *testing.T) {
	secrets := []string{".env", ".env.*", "**/.env", "**/.env.*"}
	except := []string{"**/*.example", "**/*.sample", "**/*.template"}
	changed := []string{
		".env", ".env.local", "apps/api/.env", "apps/api/.env.local",
		".env.example", "apps/api/.env.example", "config/app.template",
	}
	got := SecretFilesExcept(secrets, except, changed)
	want := []string{".env", ".env.local", "apps/api/.env", "apps/api/.env.local"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("secrets = %v, want %v", got, want)
	}
	// Without an exception list every secret-looking path is reported again
	// (six of the seven: `config/app.template` is not secret-shaped).
	if n := len(SecretFilesExcept(secrets, nil, changed)); n != 6 {
		t.Errorf("no exceptions = %d paths, want 6", n)
	}
}

// A live check failed with `sh: 1: go: not found` and the detail read like a test
// failure. It is a broken PATH on the machine, and the message should say so.
func TestMissingToolIsNamedWithThePath(t *testing.T) {
	dir := t.TempDir()
	// A PATH with nothing on it: /usr/bin may well have go on the machine running
	// the tests, and the point is what happens when it does not.
	r := New(Options{Dir: dir, Path: "/nonexistent-bin"})
	res := r.RunCheck(context.Background(), config.Check{
		Name: "go", Run: []string{"cd . && go test ./..."},
	})
	if res.OK {
		t.Fatal("expected a failure")
	}
	f := res.Failures[0]
	if !strings.Contains(f, "go is not on this run's PATH") {
		t.Errorf("the failure should name the missing tool:\n%s", f)
	}
	if !strings.Contains(f, "/nonexistent-bin") {
		t.Errorf("the failure should print the PATH it ran with:\n%s", f)
	}
	if !strings.Contains(f, "agent.env") {
		t.Errorf("the failure should say where to fix it:\n%s", f)
	}
}

// A test that merely prints "not found" is a real failure, not a PATH problem.
func TestMissingToolOnlyMatchesShellErrors(t *testing.T) {
	if _, ok := missingTool("FAIL: TestX: user not found"); ok {
		t.Error("ordinary output was read as a missing tool")
	}
	if tool, ok := missingTool("sh: 1: flutter: not found"); !ok || tool != "flutter" {
		t.Errorf("sh error = %q %v", tool, ok)
	}
	if tool, ok := missingTool("bash: line 1: dart: command not found"); !ok || tool != "dart" {
		t.Errorf("bash error = %q %v", tool, ok)
	}
}

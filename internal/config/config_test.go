package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFleetConfigAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeFile(t, path, `
[agent]
model = "example/model-a"

[[repo]]
name = "demo"
path = "/srv/agent/repos/demo"
slug = "acme/demo"
bot = "demo-bot[bot]"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Workroot != DefaultWorkroot {
		t.Errorf("workroot = %q, want default", cfg.Agent.Workroot)
	}
	if cfg.Agent.Brain != DefaultBrain || cfg.Agent.Thinking != DefaultThinking {
		t.Errorf("brain defaults = %+v", cfg.Agent)
	}
	if cfg.Agent.MaxRunMinutes != DefaultMaxRunMinutes || cfg.Agent.PollMinutes != DefaultPollMinutes {
		t.Errorf("timeouts = %d/%d", cfg.Agent.MaxRunMinutes, cfg.Agent.PollMinutes)
	}
	if cfg.Agent.Model != "example/model-a" {
		t.Errorf("explicit model lost: %q", cfg.Agent.Model)
	}
}

func TestLoadFleetConfigRejectsBadRepos(t *testing.T) {
	cases := map[string]string{
		"no repos":       `[agent]`,
		"missing slug":   "[[repo]]\nname='a'\npath='/x'\nbot='b'\n",
		"slug not owner": "[[repo]]\nname='a'\npath='/x'\nslug='noslash'\nbot='b'\n",
		"missing bot":    "[[repo]]\nname='a'\npath='/x'\nslug='o/r'\n",
		"duplicate name": "[[repo]]\nname='a'\npath='/x'\nslug='o/r'\nbot='b'\n[[repo]]\nname='a'\npath='/y'\nslug='o/r2'\nbot='b'\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, body)
			if _, err := Load(path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPickRepo(t *testing.T) {
	one := &Config{Repos: []Repo{{Name: "solo"}}}
	got, err := one.PickRepo("")
	if err != nil || got.Name != "solo" {
		t.Fatalf("single repo pick: %v %v", got, err)
	}

	two := &Config{Repos: []Repo{{Name: "a"}, {Name: "b"}}}
	if _, err := two.PickRepo(""); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("ambiguous pick error = %v", err)
	}
	if _, err := two.PickRepo("c"); err == nil {
		t.Fatal("unknown repo accepted")
	}
	if got, err := two.PickRepo("b"); err != nil || got.Name != "b" {
		t.Fatalf("named pick: %v %v", got, err)
	}
}

func TestResolveBrainPrecedence(t *testing.T) {
	agent := AgentConfig{Brain: "pi", Model: "fleet-model", Thinking: "medium"}
	repo := Repo{Brain: "opencode", Model: "repo-model"}
	prof := &Profile{Brain: BrainSettings{Name: "exec", Thinking: "max"}}

	cases := []struct {
		name string
		ov   Overrides
		prof *Profile
		repo Repo
		want BrainChoice
	}{
		{
			name: "flag wins over everything",
			ov:   Overrides{Brain: "stub", Model: "flag-model", Thinking: "off"},
			prof: prof, repo: repo,
			want: BrainChoice{Name: "stub", Model: "flag-model", Thinking: "off"},
		},
		{
			name: "profile beats repo and fleet",
			prof: prof, repo: repo,
			want: BrainChoice{Name: "exec", Model: "repo-model", Thinking: "max"},
		},
		{
			name: "repo beats fleet",
			prof: &Profile{}, repo: repo,
			want: BrainChoice{Name: "opencode", Model: "repo-model", Thinking: "medium"},
		},
		{
			name: "fleet is the floor",
			prof: &Profile{}, repo: Repo{},
			want: BrainChoice{Name: "pi", Model: "fleet-model", Thinking: "medium"},
		},
		{
			name: "no profile at all",
			repo: Repo{},
			want: BrainChoice{Name: "pi", Model: "fleet-model", Thinking: "medium"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveBrain(agent, tc.repo, tc.prof, tc.ov)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProfileVersionGate(t *testing.T) {
	p := Defaults()
	p.Version = SupportedProfileMajor + 1
	err := p.Validate()
	if err == nil {
		t.Fatal("newer profile accepted")
	}
	if !strings.Contains(err.Error(), "install a newer issuewright first") {
		t.Errorf("unhelpful error: %v", err)
	}
	if err := Defaults().Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestLoadProfileMissingIsDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/x\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"build":"tsc","test":"vitest"}}`)
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: x\n")

	p, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	names := map[string]bool{}
	for _, c := range p.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"go", "npm", "flutter"} {
		if !names[want] {
			t.Errorf("detected checks missing %q: %+v", want, names)
		}
	}
	// A Makefile must not be guessed at.
	if names["make"] {
		t.Error("Makefile should not be inferred")
	}
}

func TestLoadProfileYAMLAndDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ProfileDir, ProfileFile), `
version: 1
checks:
  - name: go
    paths: ["**/*.go"]
    run: ["go test ./..."]
unknown_field: ignored
`)
	p, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if p.BaseBranch != DefaultBaseBranch || p.BranchPrefix != DefaultBranchPrefix {
		t.Errorf("defaults not applied: %+v", p)
	}
	if p.PickupMode("implement") != "assignee" {
		t.Errorf("pickup = %q", p.PickupMode("implement"))
	}
}

func TestProfileValidation(t *testing.T) {
	cases := map[string]*Profile{
		"check without paths": {Version: 1, Checks: []Check{{Name: "x", Run: []string{"true"}}}},
		"check without run":   {Version: 1, Checks: []Check{{Name: "x", Paths: []string{"a"}}}},
		"bad pickup":          {Version: 1, Issue: IssueSettings{Pickup: "magic"}},
		"bad trigger":         {Version: 1, Workflows: map[string]WorkflowSettings{"implement": {On: []string{"on_full_moon"}}}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := p.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestMarshalYAMLRoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	p := Detect(dir)
	body, err := p.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	writeFile(t, filepath.Join(dir, ProfileDir, ProfileFile), string(body))
	got, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, body)
	}
	if len(got.Checks) != len(p.Checks) {
		t.Fatalf("checks lost: %+v vs %+v", got.Checks, p.Checks)
	}
	if got.Checks[0].Run[0] != p.Checks[0].Run[0] {
		t.Errorf("run command mangled: %q", got.Checks[0].Run[0])
	}
}

// A live repo had issue.pickup: label (the trigger the user wanted) plus
// workflows.implement.pickup: assignee, which silently won: the label never
// started anything. The conflict must be detectable.
func TestPickupConflict(t *testing.T) {
	p := Defaults()
	p.Issue.Pickup = "label"
	p.Workflows["implement"] = WorkflowSettings{Pickup: "assignee"}
	msg := p.PickupConflict("implement")
	if !strings.Contains(msg, "workflows.implement.pickup") || !strings.Contains(msg, "issue.pickup") {
		t.Errorf("conflict not reported: %q", msg)
	}
	if got := p.PickupMode("implement"); got != "assignee" {
		t.Errorf("workflow pickup must win (and be reported), got %q", got)
	}

	same := Defaults()
	same.Issue.Pickup = "label"
	same.Workflows["implement"] = WorkflowSettings{Pickup: "label"}
	if msg := same.PickupConflict("implement"); msg != "" {
		t.Errorf("identical settings are not a conflict: %q", msg)
	}
	oneOnly := Defaults()
	oneOnly.Issue.Pickup = "label"
	if msg := oneOnly.PickupConflict("implement"); msg != "" {
		t.Errorf("a single setting is not a conflict: %q", msg)
	}
}

func TestLabelOverrides(t *testing.T) {
	p := Defaults()
	cases := []struct {
		name   string
		labels []string
		want   Overrides
	}{
		{"none", []string{"bug", "agent:go"}, Overrides{}},
		{"model", []string{"model:provider/fast"}, Overrides{Model: "provider/fast"}},
		{"thinking", []string{"thinking:HIGH"}, Overrides{Thinking: "high"}},
		{"brain", []string{"brain:opencode"}, Overrides{Brain: "opencode"}},
		{"all three", []string{"agent:go", "model:m1", "thinking:low", "brain:pi"},
			Overrides{Brain: "pi", Model: "m1", Thinking: "low"}},
		{"case-insensitive prefix", []string{"MODEL:m2"}, Overrides{Model: "m2"}},
		{"empty value ignored", []string{"model:"}, Overrides{}},
		{"unprefixed lookalike", []string{"modelled: thing"}, Overrides{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := p.LabelOverrides(tc.labels)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if len(warns) != 0 {
				t.Errorf("unexpected warnings: %v", warns)
			}
		})
	}
}

func TestLabelOverridesConflictsAndDisable(t *testing.T) {
	p := Defaults()
	got, warns := p.LabelOverrides([]string{"model:first", "model:second"})
	if got.Model != "first" {
		t.Errorf("the first label should win: %+v", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "two model labels") {
		t.Errorf("warnings = %v", warns)
	}

	off := false
	p.Labels.Override = &off
	if got, warns := p.LabelOverrides([]string{"model:x", "brain:y"}); got != (Overrides{}) || warns != nil {
		t.Errorf("override=false must ignore labels: %+v %v", got, warns)
	}

	// A custom prefix, and disabling one channel by emptying its prefix.
	custom := Defaults()
	custom.Labels.ModelPrefix = "llm="
	custom.Labels.BrainPrefix = ""
	got, _ = custom.LabelOverrides([]string{"llm=big", "brain:pi", "model:ignored"})
	if got.Model != "big" || got.Brain != "" {
		t.Errorf("custom prefixes = %+v", got)
	}
}

func TestMergeOverridesFlagsWin(t *testing.T) {
	flags := Overrides{Model: "from-flag"}
	labels := Overrides{Brain: "opencode", Model: "from-label", Thinking: "low"}
	got := MergeOverrides(flags, labels)
	if got.Model != "from-flag" {
		t.Errorf("flag should win: %+v", got)
	}
	if got.Brain != "opencode" || got.Thinking != "low" {
		t.Errorf("label should fill the gaps: %+v", got)
	}
}

// A reviewer that shares the author's weights shares its blind spots. The
// workflow may name its own brain, model and thinking level.
func TestWorkflowBrainOverridesRepoBrain(t *testing.T) {
	agent := AgentConfig{Brain: "pi", Model: "fleet/model", Thinking: "high"}
	prof := Defaults()
	prof.Brain = BrainSettings{Model: "repo/model"}
	prof.Workflows["review"] = WorkflowSettings{Brain: BrainSettings{Model: "review/model", Thinking: "max"}}

	impl := ResolveBrainFor(agent, Repo{}, prof, "implement", Overrides{})
	if impl.Model != "repo/model" || impl.Thinking != "high" {
		t.Errorf("implement should use the repo brain: %+v", impl)
	}
	rev := ResolveBrainFor(agent, Repo{}, prof, "review", Overrides{})
	if rev.Model != "review/model" || rev.Thinking != "max" {
		t.Errorf("review should use its own brain: %+v", rev)
	}
	if rev.Name != "pi" {
		t.Errorf("unset fields should fall through: %+v", rev)
	}

	// Flags and labels still beat a workflow brain.
	flagged := ResolveBrainFor(agent, Repo{}, prof, "review", Overrides{Model: "flag/model"})
	if flagged.Model != "flag/model" {
		t.Errorf("flag must win: %+v", flagged)
	}
	labelled := MergeOverrides(Overrides{}, Overrides{Model: "label/model"})
	if got := ResolveBrainFor(agent, Repo{}, prof, "review", labelled); got.Model != "label/model" {
		t.Errorf("label must beat the workflow brain: %+v", got)
	}
}

func TestWorkflowBrainCanSwitchDriver(t *testing.T) {
	prof := Defaults()
	prof.Brain = BrainSettings{Name: "pi"}
	prof.Workflows["review"] = WorkflowSettings{Brain: BrainSettings{Name: "opencode"}}
	if got := ResolveBrainFor(AgentConfig{}, Repo{}, prof, "review", Overrides{}); got.Name != "opencode" {
		t.Errorf("review driver = %q", got.Name)
	}
	if got := ResolveBrainFor(AgentConfig{}, Repo{}, prof, "implement", Overrides{}); got.Name != "pi" {
		t.Errorf("implement driver = %q", got.Name)
	}
}

// A human types "@name", GitHub stores "@name[bot]": both must address the bot.
func TestMentionHandles(t *testing.T) {
	p := Defaults()
	p.Issue.Mentions = []string{"agent"} // a short alias, without the @
	handles := p.MentionHandles("demo-bot[bot]")
	joined := strings.Join(handles, " ")
	for _, want := range []string{"@demo-bot[bot]", "@demo-bot", "@agent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("handles %v missing %q", handles, want)
		}
	}
	cases := map[string]bool{
		"@demo-bot can you look?":          true,
		"hey @demo-bot[bot] this is wrong": true,
		"@agent please fix":                true,
		"unrelated: the bot is slow":       false,
		"@demobot is a different account":  false,
		"":                                 false,
	}
	for body, want := range cases {
		if got := Mentions(body, handles); got != want {
			t.Errorf("Mentions(%q) = %v, want %v", body, got, want)
		}
	}
}

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
model = "opencode-go/kimi"

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
	if cfg.Agent.Model != "opencode-go/kimi" {
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

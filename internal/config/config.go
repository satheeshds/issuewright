// Package config loads the fleet configuration (which repos, which bots) and the
// per-repo profile (which checks, what not to touch). Neither file contains repo
// logic: everything repo-specific lives in the profile committed in that repo.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// Fleet defaults. These are intentionally not per-repo: a repo that needs a
// different value says so in its [[repo]] block or its profile.
const (
	DefaultWorkroot              = "/srv/agent/work"
	DefaultLogdir                = "/srv/agent/logs"
	DefaultReposRoot             = "/srv/agent/repos"
	DefaultBrain                 = "pi"
	DefaultThinking              = "high"
	DefaultMaxRunMinutes         = 90
	DefaultPollMinutes           = 10
	DefaultGhBin                 = "gh"
	DefaultBaseBranch            = "main"
	DefaultBranchPrefix          = "agent/"
	DefaultInProgress            = "agent:in-progress"
	DefaultBlocked               = "agent:blocked"
	DefaultDone                  = "agent:done"
	DefaultForceLabel            = "agent:go"
	DefaultPickupMode            = "assignee"
	DefaultReviewLabel           = "review:go"
	DefaultReviewInProgressLabel = "review:in-progress"
	DefaultReviewDoneLabel       = "review:done"
	DefaultReviewBlockedLabel    = "review:blocked"
	SupportedProfileMajor        = 1
)

// Config is the fleet file: one machine, N repos.
type Config struct {
	Agent AgentConfig `toml:"agent" yaml:"agent"`
	Repos []Repo      `toml:"repo" yaml:"repo"`
}

// AgentConfig holds machine-wide defaults. Repo blocks override the brain
// fields; nothing else.
type AgentConfig struct {
	Workroot      string `toml:"workroot" yaml:"workroot"`
	Logdir        string `toml:"logdir" yaml:"logdir"`
	ReposRoot     string `toml:"repos_root" yaml:"repos_root"`
	Brain         string `toml:"brain" yaml:"brain"`
	Model         string `toml:"model" yaml:"model"`
	Thinking      string `toml:"thinking" yaml:"thinking"`
	MaxRunMinutes int    `toml:"max_run_minutes" yaml:"max_run_minutes"`
	PollMinutes   int    `toml:"poll_minutes" yaml:"poll_minutes"`
	GhBin         string `toml:"gh_bin" yaml:"gh_bin"`
	// Mentions are extra ways to address the bot in a comment, fleet-wide. The
	// bot login is always included; this is for shorthands like "@agent".
	Mentions []string `toml:"mentions" yaml:"mentions"`
}

// Repo is one entry of the fleet file.
type Repo struct {
	Name     string `toml:"name" yaml:"name"`
	Path     string `toml:"path" yaml:"path"`
	Slug     string `toml:"slug" yaml:"slug"`
	Bot      string `toml:"bot" yaml:"bot"`
	TokenEnv string `toml:"token_env" yaml:"token_env"`
	Brain    string `toml:"brain" yaml:"brain"`
	Model    string `toml:"model" yaml:"model"`
	Thinking string `toml:"thinking" yaml:"thinking"`
	// Mentions extends [agent].mentions for this repo.
	Mentions []string `toml:"mentions" yaml:"mentions"`
}

// Overrides are the most specific layer: command-line flags and env vars.
type Overrides struct {
	Brain    string
	Model    string
	Thinking string
}

// BrainChoice is the resolved brain and its settings.
type BrainChoice struct {
	Name     string
	Model    string
	Thinking string
}

// Load reads the fleet config, applying defaults. TOML or YAML by extension.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	default:
		if err := toml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg.applyDefaults()
	return cfg, cfg.validate()
}

func (c *Config) applyDefaults() {
	a := &c.Agent
	setIfEmpty(&a.Workroot, DefaultWorkroot)
	setIfEmpty(&a.Logdir, DefaultLogdir)
	setIfEmpty(&a.ReposRoot, DefaultReposRoot)
	setIfEmpty(&a.Brain, DefaultBrain)
	setIfEmpty(&a.Thinking, DefaultThinking)
	setIfEmpty(&a.GhBin, DefaultGhBin)
	if a.MaxRunMinutes == 0 {
		a.MaxRunMinutes = DefaultMaxRunMinutes
	}
	if a.PollMinutes == 0 {
		a.PollMinutes = DefaultPollMinutes
	}
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return errors.New("config has no [[repo]] blocks")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		switch {
		case r.Name == "":
			return fmt.Errorf("repo #%d: name is required", i+1)
		case seen[r.Name]:
			return fmt.Errorf("repo %q: duplicate name", r.Name)
		case r.Slug == "":
			return fmt.Errorf("repo %q: slug is required (owner/name)", r.Name)
		case !strings.Contains(r.Slug, "/"):
			return fmt.Errorf("repo %q: slug %q must be owner/name", r.Name, r.Slug)
		case r.Path == "":
			return fmt.Errorf("repo %q: path is required", r.Name)
		case r.Bot == "":
			return fmt.Errorf("repo %q: bot is required (the login issues are assigned to)", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// PickRepo resolves --repo, or the only repo when a single one is configured.
func (c *Config) PickRepo(name string) (Repo, error) {
	if name == "" {
		if len(c.Repos) == 1 {
			return c.Repos[0], nil
		}
		names := make([]string, 0, len(c.Repos))
		for _, r := range c.Repos {
			names = append(names, r.Name)
		}
		return Repo{}, fmt.Errorf("--repo is required (configured: %s)", strings.Join(names, ", "))
	}
	for _, r := range c.Repos {
		if r.Name == name {
			return r, nil
		}
	}
	return Repo{}, fmt.Errorf("repo %q is not in the fleet config", name)
}

// ResolveBrain applies the documented precedence, most specific first:
// flags → profile → [[repo]] → [agent] → driver default.
func ResolveBrain(agent AgentConfig, repo Repo, prof *Profile, ov Overrides) BrainChoice {
	var p BrainSettings
	if prof != nil {
		p = prof.Brain
	}
	return BrainChoice{
		Name:     firstNonEmpty(ov.Brain, p.Name, repo.Brain, agent.Brain, DefaultBrain),
		Model:    firstNonEmpty(ov.Model, p.Model, repo.Model, agent.Model),
		Thinking: firstNonEmpty(ov.Thinking, p.Thinking, repo.Thinking, agent.Thinking, DefaultThinking),
	}
}

// WorkDir is where per-repo worktrees live.
func (c *Config) WorkDir(repo Repo) string {
	return filepath.Join(c.Agent.Workroot, repo.Name)
}

// LogDir is where per-repo run logs live.
func (c *Config) LogDir(repo Repo) string {
	return filepath.Join(c.Agent.Logdir, repo.Name)
}

// StateDir is where lock files and claim records live.
func (c *Config) StateDir() string {
	return filepath.Join(filepath.Dir(c.Agent.Workroot), "state")
}

// CloneDir is the agent's own clone of the repo (never a deploy checkout).
func (c *Config) CloneDir(repo Repo) string {
	if repo.Path != "" {
		return repo.Path
	}
	return filepath.Join(c.Agent.ReposRoot, repo.Name)
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

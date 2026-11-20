package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProfileDir and ProfileFile locate the per-repo profile.
const (
	ProfileDir  = ".agent"
	ProfileFile = "agent.yaml"
)

// Profile is the repo-side plug: what to verify, what never to touch, which
// workflows to run. Every field is optional; Defaults() supplies the rest.
type Profile struct {
	Version int `yaml:"version"`

	Issue IssueSettings `yaml:"issue"`

	BranchPrefix string `yaml:"branch_prefix"`
	BaseBranch   string `yaml:"base_branch"`

	Checks  []Check        `yaml:"checks"`
	Protect Protect        `yaml:"protect"`
	Prompt  PromptSettings `yaml:"prompt"`
	Brain   BrainSettings  `yaml:"brain"`
	// Labels lets a label on the issue pick the brain, model or thinking level
	// for that run, e.g. `model:provider/fast` on one issue and nothing on the
	// rest: the same mechanism as brain:, one level more specific.
	Labels    LabelSettings               `yaml:"labels"`
	Workflows map[string]WorkflowSettings `yaml:"workflows"`
}

// IssueSettings controls how work is picked up and marked.
type IssueSettings struct {
	Pickup          string `yaml:"pickup"`
	ForceLabel      string `yaml:"force_label"`
	InProgressLabel string `yaml:"in_progress_label"`
	BlockedLabel    string `yaml:"blocked_label"`
	DoneLabel       string `yaml:"done_label"`
	// Review lifecycle labels. The trigger label is the only state the runner
	// reads: adding it asks for a review, and the runner swaps it for the
	// in-progress label, then the done (or blocked) label. That makes the PR's
	// labels the state machine, so "review this again" is just re-adding the
	// trigger.
	ReviewLabel           string `yaml:"review_label"`
	ReviewInProgressLabel string `yaml:"review_in_progress_label"`
	ReviewDoneLabel       string `yaml:"review_done_label"`
	ReviewBlockedLabel    string `yaml:"review_blocked_label"`
	// Mentions are the strings that ask the bot to answer a comment. The bot login
	// is always included; extra aliases go here.
	Mentions []string `yaml:"mentions"`
}

// Check is one verification rule: when the paths match, run the commands.
type Check struct {
	Name  string   `yaml:"name"`
	Paths []string `yaml:"paths"`
	Run   []string `yaml:"run"`
	// Drift lists paths that only tooling may change. After a check's Run
	// commands execute, any hand change under Drift fails the check.
	Drift []string `yaml:"drift"`
	// Advisory marks a check whose failure is reported but does not block the
	// run. Use it for a suite with a red baseline you have not fixed yet: an
	// agent should not be blocked by failures it did not cause, and hiding the
	// suite entirely would lose the signal.
	Advisory bool `yaml:"advisory"`
}

// Protect is the set of hard rules the runner enforces rather than asks for.
type Protect struct {
	// WriteDenylist: paths the agent must never modify directly.
	WriteDenylist []string `yaml:"write_denylist"`
	// NeverRun: command prefixes the agent must not execute.
	NeverRun []string `yaml:"never_run"`
	// SecretPaths: files that must never appear in a pushed diff.
	SecretPaths []string `yaml:"secret_paths"`
}

// PromptSettings point at the repo's own rules and PR-body notes.
type PromptSettings struct {
	PreambleFile string   `yaml:"preamble_file"`
	Notes        []string `yaml:"notes"`
}

// LabelSettings control label-driven overrides on a per-run basis.
type LabelSettings struct {
	// Override defaults to true. Set false to ignore these labels entirely.
	Override *bool `yaml:"override"`
	// Prefixes; set one to "" to disable that particular override.
	ModelPrefix    string `yaml:"model_prefix"`
	ThinkingPrefix string `yaml:"thinking_prefix"`
	BrainPrefix    string `yaml:"brain_prefix"`
}

// Default label prefixes.
const (
	DefaultModelLabelPrefix    = "model:"
	DefaultThinkingLabelPrefix = "thinking:"
	DefaultBrainLabelPrefix    = "brain:"
)

// EnabledOr reports whether label overrides are allowed.
func (l LabelSettings) EnabledOr(def bool) bool {
	if l.Override == nil {
		return def
	}
	return *l.Override
}

// BrainSettings override the fleet brain for this repo.
type BrainSettings struct {
	Name     string `yaml:"name"`
	Model    string `yaml:"model"`
	Thinking string `yaml:"thinking"`
}

// WorkflowSettings enables and parameterises one workflow in this repo.
type WorkflowSettings struct {
	Enabled *bool    `yaml:"enabled"`
	Pickup  string   `yaml:"pickup"`
	On      []string `yaml:"on"`
	Prompt  string   `yaml:"prompt"`
	// Brain, when set, overrides the repo's brain for this workflow only. That is
	// how writing and reviewing stop being the same model: a reviewer sharing the
	// author's weights shares its blind spots.
	Brain BrainSettings `yaml:"brain"`
}

// EnabledOr reports whether the workflow is enabled, defaulting when unset.
func (w WorkflowSettings) EnabledOr(def bool) bool {
	if w.Enabled == nil {
		return def
	}
	return *w.Enabled
}

// ProfilePath returns the profile path for a repo root.
func ProfilePath(repoRoot string) string {
	return filepath.Join(repoRoot, ProfileDir, ProfileFile)
}

// LoadProfile reads the profile from a repo checkout. A missing profile is not
// an error: the repo runs on detected defaults, which is what makes `init`
// an upgrade rather than an entry fee.
func LoadProfile(repoRoot string) (*Profile, error) {
	path := ProfilePath(repoRoot)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Detect(repoRoot), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	p := &Profile{}
	if err := yaml.Unmarshal(raw, p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	p.applyDefaults()
	return p, p.Validate()
}

// Validate enforces the one cross-repo contract: the profile version.
func (p *Profile) Validate() error {
	if p == nil {
		return errors.New("nil profile")
	}
	if p.Version > SupportedProfileMajor {
		return fmt.Errorf(
			"profile version %d is newer than this binary supports (%d): install a newer issuewright first, then bump the profile",
			p.Version, SupportedProfileMajor)
	}
	if p.Version < 0 {
		return fmt.Errorf("profile version %d is invalid", p.Version)
	}
	if p.Issue.Pickup != "" && p.Issue.Pickup != "assignee" && p.Issue.Pickup != "label" {
		return fmt.Errorf("issue.pickup must be assignee or label, got %q", p.Issue.Pickup)
	}
	for i, c := range p.Checks {
		if c.Name == "" {
			return fmt.Errorf("checks[%d]: name is required", i)
		}
		if len(c.Paths) == 0 {
			return fmt.Errorf("check %q: paths is required", c.Name)
		}
		if len(c.Run) == 0 {
			return fmt.Errorf("check %q: run is required", c.Name)
		}
	}
	for name, w := range p.Workflows {
		for _, on := range w.On {
			switch on {
			case "issue_assigned", "issue_label", "pr_opened", "comment_mention":
			default:
				return fmt.Errorf("workflow %q: unknown trigger %q", name, on)
			}
		}
	}
	return nil
}

// Defaults returns the profile used when a repo declares nothing.
func Defaults() *Profile {
	return &Profile{
		Version: SupportedProfileMajor,
		Issue: IssueSettings{Pickup: DefaultPickupMode, ForceLabel: DefaultForceLabel,
			InProgressLabel: DefaultInProgress, BlockedLabel: DefaultBlocked, DoneLabel: DefaultDone,
			ReviewLabel: DefaultReviewLabel, ReviewInProgressLabel: DefaultReviewInProgressLabel,
			ReviewDoneLabel: DefaultReviewDoneLabel, ReviewBlockedLabel: DefaultReviewBlockedLabel},
		BranchPrefix: DefaultBranchPrefix,
		BaseBranch:   DefaultBaseBranch,
		Protect:      Protect{SecretPaths: []string{".env", ".env.*"}},
		Prompt:       PromptSettings{PreambleFile: filepath.Join(ProfileDir, "rules.md")},
		Labels: LabelSettings{
			ModelPrefix:    DefaultModelLabelPrefix,
			ThinkingPrefix: DefaultThinkingLabelPrefix,
			BrainPrefix:    DefaultBrainLabelPrefix,
		},
		Workflows: map[string]WorkflowSettings{},
	}
}

func (p *Profile) applyDefaults() {
	d := Defaults()
	if p.Version == 0 {
		p.Version = d.Version
	}
	if p.Issue.Pickup == "" {
		p.Issue.Pickup = d.Issue.Pickup
	}
	if p.Issue.ForceLabel == "" {
		p.Issue.ForceLabel = d.Issue.ForceLabel
	}
	if p.Issue.InProgressLabel == "" {
		p.Issue.InProgressLabel = d.Issue.InProgressLabel
	}
	if p.Issue.BlockedLabel == "" {
		p.Issue.BlockedLabel = d.Issue.BlockedLabel
	}
	if p.Issue.DoneLabel == "" {
		p.Issue.DoneLabel = d.Issue.DoneLabel
	}
	if p.Issue.ReviewLabel == "" {
		p.Issue.ReviewLabel = DefaultReviewLabel
	}
	if p.Issue.ReviewInProgressLabel == "" {
		p.Issue.ReviewInProgressLabel = DefaultReviewInProgressLabel
	}
	if p.Issue.ReviewDoneLabel == "" {
		p.Issue.ReviewDoneLabel = DefaultReviewDoneLabel
	}
	if p.Issue.ReviewBlockedLabel == "" {
		p.Issue.ReviewBlockedLabel = DefaultReviewBlockedLabel
	}
	if p.BranchPrefix == "" {
		p.BranchPrefix = d.BranchPrefix
	}
	if p.BaseBranch == "" {
		p.BaseBranch = d.BaseBranch
	}
	if len(p.Protect.SecretPaths) == 0 {
		p.Protect.SecretPaths = d.Protect.SecretPaths
	}
	if p.Workflows == nil {
		p.Workflows = map[string]WorkflowSettings{}
	}
	if p.Labels.ModelPrefix == "" && p.Labels.ThinkingPrefix == "" && p.Labels.BrainPrefix == "" {
		// Nothing configured: take all three defaults. A profile that sets one
		// prefix and empties another means it, so leave it alone.
		p.Labels = d.Labels
	}
}

// Workflow returns the settings for a workflow name, or zero value.
func (p *Profile) Workflow(name string) WorkflowSettings {
	if p == nil {
		return WorkflowSettings{}
	}
	return p.Workflows[name]
}

// BrainFor merges the workflow's brain block over the profile's.
func (p *Profile) BrainFor(workflow string) BrainSettings {
	var out BrainSettings
	if p != nil {
		out = p.Brain
	}
	w := p.Workflow(workflow)
	if w.Brain.Name != "" {
		out.Name = w.Brain.Name
	}
	if w.Brain.Model != "" {
		out.Model = w.Brain.Model
	}
	if w.Brain.Thinking != "" {
		out.Thinking = w.Brain.Thinking
	}
	return out
}

// ResolveBrainFor applies the full precedence for one workflow: flags → label →
// workflows.<name>.brain → brain → [[repo]] → [agent] → driver default.
func ResolveBrainFor(agent AgentConfig, repo Repo, prof *Profile, workflow string, ov Overrides) BrainChoice {
	b := BrainSettings{}
	if prof != nil {
		if workflow != "" {
			b = prof.BrainFor(workflow)
		} else {
			b = prof.Brain
		}
	}
	return BrainChoice{
		Name:     firstNonEmpty(ov.Brain, b.Name, repo.Brain, agent.Brain, DefaultBrain),
		Model:    firstNonEmpty(ov.Model, b.Model, repo.Model, agent.Model),
		Thinking: firstNonEmpty(ov.Thinking, b.Thinking, repo.Thinking, agent.Thinking, DefaultThinking),
	}
}

// PickupConflict reports a workflow-level pickup that silently overrides the
// issue-level one. Two settings that disagree is how a label trigger stops
// working without anyone noticing.
func (p *Profile) PickupConflict(workflow string) string {
	if p == nil {
		return ""
	}
	w := p.Workflow(workflow)
	if w.Pickup == "" || p.Issue.Pickup == "" || w.Pickup == p.Issue.Pickup {
		return ""
	}
	return fmt.Sprintf("workflows.%s.pickup=%q overrides issue.pickup=%q; remove one of them",
		workflow, w.Pickup, p.Issue.Pickup)
}

// MentionHandles returns the strings that address the bot in a comment: the fixed
// ones plus anything configured. Matching is case-insensitive and needs the "@".
func (p *Profile) MentionHandles(botLogin string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if !strings.HasPrefix(s, "@") {
			s = "@" + s
		}
		if !seen[strings.ToLower(s)] {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	add(botLogin)                              // the App login…
	add(strings.TrimSuffix(botLogin, "[bot]")) // …and how a human actually types it
	if p != nil {
		for _, m := range p.Issue.Mentions {
			add(m)
		}
	}
	return out
}

// Mentions reports whether a comment body addresses the bot.
func Mentions(body string, handles []string) bool {
	lower := strings.ToLower(body)
	for _, h := range handles {
		if h != "" && strings.Contains(lower, strings.ToLower(h)) {
			return true
		}
	}
	return false
}

// LabelOverrides reads brain/model/thinking out of a label list. Labels are the
// most specific place to say "run this one on a different model", without
// changing the repo's defaults for everything else.
//
// Unknown or duplicated values are reported as warnings rather than failures: a
// mistyped label should not stop work, it should be visible in the log.
func (p *Profile) LabelOverrides(labels []string) (Overrides, []string) {
	if p == nil || !p.Labels.EnabledOr(true) {
		return Overrides{}, nil
	}
	var ov Overrides
	var warnings []string
	for _, label := range labels {
		name := strings.TrimSpace(label)
		lower := strings.ToLower(name)
		switch {
		case p.Labels.BrainPrefix != "" && strings.HasPrefix(lower, strings.ToLower(p.Labels.BrainPrefix)):
			v := strings.TrimSpace(name[len(p.Labels.BrainPrefix):])
			if ov.Brain != "" && ov.Brain != v {
				warnings = append(warnings, fmt.Sprintf("two brain labels (%q, %q): using %q", ov.Brain, v, ov.Brain))
				continue
			}
			ov.Brain = v
		case p.Labels.ModelPrefix != "" && strings.HasPrefix(lower, strings.ToLower(p.Labels.ModelPrefix)):
			v := strings.TrimSpace(name[len(p.Labels.ModelPrefix):])
			if ov.Model != "" && ov.Model != v {
				warnings = append(warnings, fmt.Sprintf("two model labels (%q, %q): using %q", ov.Model, v, ov.Model))
				continue
			}
			ov.Model = v
		case p.Labels.ThinkingPrefix != "" && strings.HasPrefix(lower, strings.ToLower(p.Labels.ThinkingPrefix)):
			v := strings.ToLower(strings.TrimSpace(name[len(p.Labels.ThinkingPrefix):]))
			if ov.Thinking != "" && ov.Thinking != v {
				warnings = append(warnings, fmt.Sprintf("two thinking labels (%q, %q): using %q", ov.Thinking, v, ov.Thinking))
				continue
			}
			ov.Thinking = v
		}
	}
	return ov, warnings
}

// MergeOverrides returns primary, filled in from secondary where primary is
// empty. Command-line flags (primary) therefore beat labels (secondary).
func MergeOverrides(primary, secondary Overrides) Overrides {
	out := primary
	if out.Brain == "" {
		out.Brain = secondary.Brain
	}
	if out.Model == "" {
		out.Model = secondary.Model
	}
	if out.Thinking == "" {
		out.Thinking = secondary.Thinking
	}
	return out
}

// PickupMode resolves the effective pickup mode for a workflow.
func (p *Profile) PickupMode(workflow string) string {
	if w := p.Workflow(workflow); w.Pickup != "" {
		return w.Pickup
	}
	if p != nil && p.Issue.Pickup != "" {
		return p.Issue.Pickup
	}
	return DefaultPickupMode
}

// Detect sniffs a checkout and returns a starter profile. This is what makes
// "works on any repo" true before anyone writes a profile.
func Detect(repoRoot string) *Profile {
	p := Defaults()
	p.Checks = detectChecks(repoRoot)
	return p
}

func detectChecks(root string) []Check {
	var checks []Check
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	if has("go.mod") {
		checks = append(checks, Check{
			Name:  "go",
			Paths: []string{"**/*.go", "go.mod", "go.sum"},
			Run:   []string{"go test ./... && go build ./..."},
		})
	}
	if has("package.json") {
		run := []string{"npm ci"}
		if pkgHasScript(root, "build") {
			run = append(run, "npm run build")
		}
		if pkgHasScript(root, "test") {
			run = append(run, "npm test")
		}
		checks = append(checks, Check{
			Name:  "npm",
			Paths: []string{"**/*.ts", "**/*.tsx", "**/*.js", "**/*.jsx", "package.json", "package-lock.json"},
			Run:   run,
		})
	}
	if has("pubspec.yaml") {
		checks = append(checks, Check{
			Name:  "flutter",
			Paths: []string{"**/*.dart", "pubspec.yaml"},
			Run:   []string{"flutter pub get", "flutter analyze", "flutter test"},
		})
	}
	// A Makefile is deliberately not inferred: guessing `make test` is worse
	// than silence.
	return checks
}

func pkgHasScript(root, script string) bool {
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), `"`+script+`"`)
}

// MarshalYAML renders a commented starter profile.
func (p *Profile) MarshalYAML() ([]byte, error) {
	var b strings.Builder
	b.WriteString("# .agent/agent.yaml — how issuewright verifies this repo.\n")
	b.WriteString("# Generated by `issuewright init`; every field is optional.\n")
	b.WriteString("# Docs: https://github.com/satheeshds/issuewright/blob/main/docs/README.md\n\n")
	b.WriteString("version: 1\n\n")

	b.WriteString("issue:\n")
	fmt.Fprintf(&b, "  pickup: %s            # assignee | label\n", p.Issue.Pickup)
	fmt.Fprintf(&b, "  force_label: %s\n", p.Issue.ForceLabel)
	fmt.Fprintf(&b, "  in_progress_label: %s\n", p.Issue.InProgressLabel)
	fmt.Fprintf(&b, "  blocked_label: %s\n", p.Issue.BlockedLabel)
	fmt.Fprintf(&b, "  done_label: %s\n\n", p.Issue.DoneLabel)

	fmt.Fprintf(&b, "branch_prefix: %s\n", p.BranchPrefix)
	fmt.Fprintf(&b, "base_branch: %s\n\n", p.BaseBranch)

	b.WriteString("# Path globs → commands. Every matching check runs; a check whose\n")
	b.WriteString("# `drift` paths changed by hand fails.\n")
	if len(p.Checks) == 0 {
		b.WriteString("checks: []\n")
	} else {
		b.WriteString("checks:\n")
		for _, c := range p.Checks {
			fmt.Fprintf(&b, "  - name: %s\n", c.Name)
			fmt.Fprintf(&b, "    paths: [%s]\n", quoteList(c.Paths))
			b.WriteString("    run:\n")
			for _, cmd := range c.Run {
				fmt.Fprintf(&b, "      - %s\n", cmd)
			}
			if len(c.Drift) > 0 {
				fmt.Fprintf(&b, "    drift: [%s]\n", quoteList(c.Drift))
			}
			if c.Advisory {
				b.WriteString("    advisory: true          # failures are reported, not blocking\n")
			}
		}
	}
	b.WriteString("\n# Hard rules the runner enforces, not just asks for.\n")
	b.WriteString("protect:\n")
	b.WriteString("  write_denylist: []      # files the agent must never edit by hand\n")
	b.WriteString("  never_run: []           # command prefixes the agent must not execute\n")
	fmt.Fprintf(&b, "  secret_paths: [%s]\n", quoteList(p.Protect.SecretPaths))

	b.WriteString("\nprompt:\n")
	fmt.Fprintf(&b, "  preamble_file: %s\n", p.Prompt.PreambleFile)

	b.WriteString("\n# brain / model / thinking resolve most-specific-first:\n")
	b.WriteString("#   flags → this file → [[repo]] → [agent] → driver default\n")
	b.WriteString("brain:\n")
	b.WriteString("  name: \"\"\n  model: \"\"\n  thinking: \"\"\n")

	b.WriteString("\nworkflows:\n")
	b.WriteString("  implement:\n    enabled: true\n")
	b.WriteString("  review:\n    enabled: true\n    on: [pr_opened, comment_mention]\n")
	return []byte(b.String()), nil
}

func quoteList(vals []string) string {
	quoted := make([]string, 0, len(vals))
	for _, v := range vals {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return strings.Join(quoted, ", ")
}

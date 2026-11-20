// Command issuewright turns GitHub events into agent runs: an assigned issue or
// a requested review becomes a worktree, a brain run, a check pass, and either a
// draft PR or a review comment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/satheeshds/issuewright/deploy"
	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/config"
	"github.com/satheeshds/issuewright/internal/engine"
	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/scaffold"
	"github.com/satheeshds/issuewright/internal/version"
	"github.com/satheeshds/issuewright/internal/workflow"

	// Brain drivers register themselves. Adding one is a package plus this line.
	_ "github.com/satheeshds/issuewright/internal/brain/execbrain"
	_ "github.com/satheeshds/issuewright/internal/brain/opencode"
	_ "github.com/satheeshds/issuewright/internal/brain/pi"
	_ "github.com/satheeshds/issuewright/internal/brain/stub"

	// Workflow engines register themselves, the same way.
	_ "github.com/satheeshds/issuewright/internal/workflow/implement"
	_ "github.com/satheeshds/issuewright/internal/workflow/respond"
	_ "github.com/satheeshds/issuewright/internal/workflow/review"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "issuewright: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a subcommand is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "tick":
		return cmdTick(ctx, args[1:])
	case "run":
		return cmdRun(ctx, args[1:])
	case "status":
		return cmdStatus(ctx, args[1:])
	case "init":
		return cmdInit(args[1:])
	case "install":
		return cmdInstall(args[1:])
	case "new":
		return cmdNew(args[1:])
	case "list":
		return cmdList(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `issuewright — GitHub events in, agent work out

  tick     [--repo NAME] [--dry-run]     run one pass over pending events
  run      --repo NAME (--issue N | --pr N) [--workflow NAME] [--dry-run]
  status   [--repo NAME] [--json]        what is pending, in flight, blocked
  init     [--dir PATH] [--force]        write a starter .agent/agent.yaml
  install  [--prefix DIR] [--dry-run]    install systemd units
  new      workflow|brain NAME           scaffold a plugin
  list     workflows|brains              show registered plugins
  version

Global flags: --config PATH (default $AGENT_CONFIG or /srv/agent/config.toml)
Brain flags:  --brain NAME --model ID --thinking LEVEL  (env: AGENT_BRAIN, AGENT_MODEL, AGENT_THINKING)
`)
}

// common holds flags shared by tick/run/status.
type common struct {
	config  string
	repo    string
	dryRun  bool
	brain   string
	model   string
	think   string
	logOut  io.Writer
	verbose bool
}

func addCommon(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.config, "config", defaultConfigPath(), "fleet config file")
	fs.StringVar(&c.repo, "repo", "", "repo name from the fleet config (one repo: optional)")
	fs.BoolVar(&c.dryRun, "dry-run", false, "resolve everything, change nothing")
	fs.StringVar(&c.brain, "brain", envOr("AGENT_BRAIN", ""), "brain driver override")
	fs.StringVar(&c.model, "model", envOr("AGENT_MODEL", ""), "model override")
	fs.StringVar(&c.think, "thinking", envOr("AGENT_THINKING", ""), "thinking level override")
	fs.BoolVar(&c.verbose, "verbose", false, "more detail on stderr")
	c.logOut = os.Stderr
	return c
}

func defaultConfigPath() string {
	if p := os.Getenv("AGENT_CONFIG"); p != "" {
		return p
	}
	return "/srv/agent/config.toml"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolve loads the fleet config and picks the repo the command operates on.
func (c *common) resolve() (*config.Config, config.Repo, error) {
	cfg, err := config.Load(c.config)
	if err != nil {
		return nil, config.Repo{}, err
	}
	repo, err := cfg.PickRepo(c.repo)
	if err != nil {
		return nil, config.Repo{}, err
	}
	return cfg, repo, nil
}

func (c *common) overrides() config.Overrides {
	return config.Overrides{Brain: c.brain, Model: c.model, Thinking: c.think}
}

func (c *common) newEngine(cfg *config.Config, repo config.Repo) (*engine.Engine, error) {
	return engine.New(engine.Options{
		Agent:    cfg.Agent,
		Repo:     repo,
		DryRun:   c.dryRun,
		Override: c.overrides(),
		GitHub:   github.NewCLI(cfg.Agent.GhBin),
		Log:      c.logOut,
		Verbose:  c.verbose,
	})
}

func cmdTick(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tick", flag.ExitOnError)
	c := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, repo, err := c.resolve()
	if err != nil {
		return err
	}
	eng, err := c.newEngine(cfg, repo)
	if err != nil {
		return err
	}
	results, err := eng.Tick(ctx)
	if err != nil {
		return err
	}
	for _, r := range results {
		fmt.Fprintf(c.logOut, "%-8s %s#%d %-12s %s\n", r.State, r.Kind, r.Number, r.Workflow, r.Detail)
	}
	if len(results) == 0 {
		fmt.Fprintln(c.logOut, "nothing pending")
	}
	// A blocked job is not a tick failure: the next tick moves on. Only a
	// harness error (above) fails the timer unit.
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := addCommon(fs)
	issue := fs.Int("issue", 0, "issue number")
	pr := fs.Int("pr", 0, "pull request number")
	force := fs.Bool("force", false, "run again even if this job or revision is already done")
	wf := fs.String("workflow", "", "workflow name (default: inferred from --issue/--pr)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*issue == 0) == (*pr == 0) {
		return errors.New("run needs exactly one of --issue or --pr")
	}
	cfg, repo, err := c.resolve()
	if err != nil {
		return err
	}
	eng, err := c.newEngine(cfg, repo)
	if err != nil {
		return err
	}
	if *force {
		eng.SetForce(true)
	}
	var res engine.Result
	if *issue != 0 {
		res, err = eng.RunIssue(ctx, *issue, *wf)
	} else {
		res, err = eng.RunPR(ctx, *pr, *wf)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.logOut, "%-8s %s#%d %-12s %s\n", res.State, res.Kind, res.Number, res.Workflow, res.Detail)
	if res.State == engine.StateBlocked {
		os.Exit(2)
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	c := addCommon(fs)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(c.config)
	if err != nil {
		return err
	}
	repos := cfg.Repos
	if c.repo != "" {
		one, err := cfg.PickRepo(c.repo)
		if err != nil {
			return err
		}
		repos = []config.Repo{one}
	}
	var rows []engine.StatusRow
	for _, repo := range repos {
		eng, err := c.newEngine(cfg, repo)
		if err != nil {
			return err
		}
		got, err := eng.Status(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", repo.Name, err)
		}
		rows = append(rows, got...)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("nothing pending")
		return nil
	}
	fmt.Printf("%-14s %-7s %-6s %-12s %-14s %s\n", "REPO", "KIND", "NUM", "WORKFLOW", "STATE", "DETAIL")
	for _, r := range rows {
		fmt.Printf("%-14s %-7s %-6d %-12s %-14s %s\n", r.Repo, r.Kind, r.Number, r.Workflow, r.State, r.Detail)
	}
	return nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "repository root")
	force := fs.Bool("force", false, "overwrite an existing profile")
	dry := fs.Bool("dry-run", false, "print the profile instead of writing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	prof := config.Detect(abs)
	if err := prof.Validate(); err != nil {
		return err
	}
	body, err := prof.MarshalYAML()
	if err != nil {
		return err
	}
	target := filepath.Join(abs, config.ProfileDir, config.ProfileFile)
	if *dry {
		fmt.Printf("# would write %s\n%s", target, body)
		return nil
	}
	if _, err := os.Stat(target); err == nil && !*force {
		return fmt.Errorf("%s exists (use --force)", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", target)
	if len(prof.Checks) == 0 {
		fmt.Println("no toolchain detected — add checks by hand before enabling workflows")
	}
	return nil
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	prefix := fs.String("prefix", "/usr/local/bin", "binary directory (informational)")
	systemdDir := fs.String("systemd-dir", "/etc/systemd/system", "systemd unit directory")
	dry := fs.Bool("dry-run", false, "print what would be written")
	if err := fs.Parse(args); err != nil {
		return err
	}
	units, err := deploy.Units()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(units))
	for n := range units {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		dst := filepath.Join(*systemdDir, name)
		if *dry {
			fmt.Printf("# would write %s (%d bytes)\n", dst, len(units[name]))
			continue
		}
		if err := os.WriteFile(dst, units[name], 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", dst)
	}
	fmt.Printf("\nbinary: %s/issuewright (this file: %s)\n", *prefix, exePath())
	fmt.Println("next: systemctl daemon-reload && systemctl enable --now 'issuewright@<repo>.timer'")
	return nil
}

func cmdNew(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: issuewright new workflow|brain NAME [--dir DIR]")
	}
	kind, name := args[0], args[1]
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	dir := fs.String("dir", ".", "module root to write into")
	dry := fs.Bool("dry-run", false, "print files instead of writing them")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	files, err := scaffold.New(kind, name, *dir)
	if err != nil {
		return err
	}
	for path, body := range files {
		if *dry {
			fmt.Printf("# %s\n%s\n", path, body)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s exists", path)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
	}
	fmt.Printf("\nnow: register it in cmd/issuewright/main.go with a blank import, then go test ./...\n")
	return nil
}

func cmdList(args []string) error {
	what := "all"
	if len(args) > 0 {
		what = args[0]
	}
	switch what {
	case "all":
		listBrains()
		fmt.Println()
		listWorkflows()
	case "workflows":
		listWorkflows()
	case "brains":
		listBrains()
	default:
		return fmt.Errorf("unknown list target %q (workflows|brains)", what)
	}
	return nil
}

func listWorkflows() {
	fmt.Println("workflows:")
	for _, name := range workflow.Names() {
		w, err := workflow.Get(name)
		if err != nil {
			continue
		}
		p := w.Perms()
		fmt.Printf("  %-12s write=%-5t comment=%-5t label=%-5t checks=%-5t triggers=%s\n",
			name, p.Write, p.Comment, p.Label, p.Checks, triggerNames(w.Triggers()))
	}
}

func listBrains() {
	fmt.Println("brains:")
	for _, name := range brain.Names() {
		d, err := brain.Get(name)
		if err != nil {
			continue
		}
		c := d.Caps()
		fmt.Printf("  %-12s resume=%-5t model=%-5t thinking=%-5t streaming=%t\n",
			name, c.Resume, c.Model, c.Thinking, c.Streaming)
	}
}

func triggerNames(ts []workflow.Trigger) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		s := string(t.Kind)
		if t.Label != "" {
			s += ":" + t.Label
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return "?"
	}
	return p
}

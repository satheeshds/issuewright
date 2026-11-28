package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/satheeshds/issuewright/internal/config"
	"github.com/satheeshds/issuewright/internal/state"
)

//go:embed templates/dashboard.html.tmpl
var templates embed.FS

// Options configure the server.
type Options struct {
	// Config is the fleet configuration, already loaded (tests inject it).
	Config *config.Config
	// Refresh is the page's meta-refresh interval.
	Refresh time.Duration
	// Now is injectable for tests.
	Now func() time.Time
	// Run executes systemctl and git; defaults to ExecRunner.
	Run Runner
	// Version is the running binary's version.
	Version string
}

// Server renders the status page and its JSON endpoints. Read-only by design: the
// only way to change anything is a label on GitHub, which the tick already reads.
type Server struct {
	opt  Options
	tpl  *template.Template
	now  func() time.Time
	run  Runner
	root string // state root, e.g. /srv/agent
}

// New builds a Server. The state root is derived from the fleet config, so the page
// and the runner agree on where state lives.
func New(opt Options) (*Server, error) {
	if opt.Config == nil {
		return nil, fmt.Errorf("web: a loaded config is required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Run == nil {
		opt.Run = ExecRunner
	}
	if opt.Refresh <= 0 {
		opt.Refresh = 15 * time.Second
	}
	tpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"since": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return HumanDuration(opt.Now().Sub(t))
		},
		"money": func(f float64) string {
			if f <= 0 {
				return "—"
			}
			return fmt.Sprintf("$%.4f", f)
		},
	}).ParseFS(templates, "templates/dashboard.html.tmpl")
	if err != nil {
		return nil, err
	}
	// ParseFS names the template after its file; execute that one.
	if parsed := tpl.Lookup("dashboard.html.tmpl"); parsed != nil {
		tpl = parsed
	}
	return &Server{
		opt:  opt,
		tpl:  tpl,
		now:  opt.Now,
		run:  opt.Run,
		root: filepath.Dir(opt.Config.Agent.Workroot),
	}, nil
}

// Handler returns the router. Only GET and HEAD are served.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleAPIStatus)
	mux.HandleFunc("GET /api/runs", s.handleAPIRuns)
	mux.HandleFunc("GET /api/log/{name}", s.handleAPILog)
	// Anything that would change state is refused, loudly: this page is read-only.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "read-only: change a label instead (see the page)", http.StatusMethodNotAllowed)
	})
	return mux
}

// --- view model ------------------------------------------------------------

// JobView is one claim, as the page shows it.
type JobView struct {
	Workflow  string    `json:"workflow"`
	Number    int       `json:"number"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Attempt   int       `json:"attempt"`
	Started   time.Time `json:"started"`
	Link      string    `json:"link,omitempty"`
	LabelName string    `json:"label,omitempty"`
	LabelDo   string    `json:"label_action,omitempty"`
	LogName   string    `json:"log,omitempty"`
	Tail      []string  `json:"tail,omitempty"`
	// Turns and CostUSD describe the conversation this job belongs to: the same
	// agent may have taken ten turns on one issue.
	Turns   int     `json:"turns,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// RepoView is one repo's slice of the page.
type RepoView struct {
	Name      string            `json:"name"`
	Slug      string            `json:"slug"`
	Running   []JobView         `json:"running"`
	Waiting   []JobView         `json:"waiting"`
	Blocked   []JobView         `json:"blocked"`
	Finished  []JobView         `json:"finished"`
	Recent    []RunSummary      `json:"recent"`
	Clone     CloneState        `json:"clone"`
	Labels    map[string]string `json:"labels"`
	Journal   string            `json:"journal"`
	LogDir    string            `json:"log_dir"`
	NextTimer string            `json:"next_timer,omitempty"`
}

// Dashboard is the whole page.
type Dashboard struct {
	Version    string     `json:"version"`
	Generated  time.Time  `json:"generated"`
	RefreshSec int        `json:"refresh_seconds"`
	Repos      []RepoView `json:"repos"`
	Timers     []Timer    `json:"timers"`
	Token      TokenState `json:"token"`
}

func (s *Server) build(ctx context.Context) Dashboard {
	d := Dashboard{
		Version:    VersionOrUnknown(s.opt.Version),
		Generated:  s.now(),
		RefreshSec: int(s.opt.Refresh.Seconds()),
	}
	units := []string{"issuewright-token.timer"}
	for _, repo := range s.opt.Config.Repos {
		units = append(units, "issuewright@"+repo.Name+".timer")
	}
	d.Timers = Timers(ctx, s.run, units)
	next := map[string]string{}
	for _, t := range d.Timers {
		name := strings.TrimSuffix(strings.TrimPrefix(t.Unit, "issuewright@"), ".timer")
		next[name] = t.NextIn
		if t.Unit == "issuewright-token.timer" {
			next["token"] = t.NextIn
		}
	}
	d.Token = ReadTokenState(ctx, filepath.Join(s.root, "token.expiry"), s.now())

	store := state.New(filepath.Join(s.root, "state"))
	for _, repo := range s.opt.Config.Repos {
		view := RepoView{
			Name:      repo.Name,
			Slug:      repo.Slug,
			Journal:   "journalctl -u issuewright@" + repo.Name + " -n 100",
			LogDir:    filepath.Join(s.opt.Config.Agent.Logdir, repo.Name),
			NextTimer: next[repo.Name],
			Labels:    s.labelNames(repo),
		}
		view.Clone = ReadClone(ctx, s.run, repo.Path)

		convs := map[string]state.Conversation{}
		if list, err := store.Conversations(repo.Name); err == nil {
			for _, c := range list {
				convs[fmt.Sprintf("%s#%d", c.Workflow, c.Number)] = c
			}
		}
		claims, err := store.Claims(repo.Name)
		if err == nil {
			sort.Slice(claims, func(i, j int) bool { return claims[i].Started.After(claims[j].Started) })
			for _, c := range claims {
				job := s.jobView(ctx, repo, c)
				if conv, ok := convs[fmt.Sprintf("%s#%d", c.Workflow, c.Number)]; ok {
					job.Turns, job.CostUSD = conv.Turns, conv.CostUSD
				}
				switch c.State {
				case "running":
					view.Running = append(view.Running, job)
				case "asking":
					view.Waiting = append(view.Waiting, job)
				case "blocked":
					view.Blocked = append(view.Blocked, job)
				default:
					view.Finished = append(view.Finished, job)
				}
			}
		}
		if len(view.Finished) > 5 {
			view.Finished = view.Finished[:5]
		}
		if runs, err := ListRuns(view.LogDir, 6); err == nil {
			view.Recent = runs
		}
		d.Repos = append(d.Repos, view)
	}
	return d
}

// labelNames reads the repo's own profile, so the page prints the labels that repo
// actually uses rather than the defaults.
func (s *Server) labelNames(repo config.Repo) map[string]string {
	prof, err := config.LoadProfile(repo.Path)
	if err != nil || prof == nil {
		def := config.Defaults()
		return map[string]string{
			"in_progress": def.Issue.InProgressLabel,
			"blocked":     def.Issue.BlockedLabel,
			"done":        def.Issue.DoneLabel,
			"force":       def.Issue.ForceLabel,
			"review":      def.Issue.ReviewLabel,
			"question":    def.Issue.QuestionLabel,
		}
	}
	return map[string]string{
		"in_progress": prof.Issue.InProgressLabel,
		"blocked":     prof.Issue.BlockedLabel,
		"done":        prof.Issue.DoneLabel,
		"force":       prof.Issue.ForceLabel,
		"review":      prof.Issue.ReviewLabel,
		"question":    prof.Issue.QuestionLabel,
	}
}

func (s *Server) jobView(ctx context.Context, repo config.Repo, c state.Claim) JobView {
	job := JobView{
		Workflow: c.Workflow, Number: c.Number, Kind: c.Kind, State: c.State,
		Detail: c.Detail, Branch: c.Branch, Attempt: c.Attempt, Started: c.Started,
	}
	// A PR-typed claim links to the pull request; everything else to the issue.
	if strings.HasPrefix(c.Kind, "pr_") || c.Kind == "review_comment" {
		job.Link = fmt.Sprintf("https://github.com/%s/pull/%d", repo.Slug, c.Number)
	} else {
		job.Link = fmt.Sprintf("https://github.com/%s/issues/%d", repo.Slug, c.Number)
	}
	labels := s.labelNames(repo)
	switch {
	case c.State == "blocked" && c.Workflow == "review":
		job.LabelName, job.LabelDo = labels["review"], "add this label to review again"
	case c.State == "blocked":
		job.LabelName, job.LabelDo = labels["blocked"], "remove this label to retry"
	case c.State == "asking":
		job.LabelName, job.LabelDo = labels["question"], "answer the question in a comment"
	case c.State == "done":
		job.LabelName = labels["done"] + " + " + labels["force"]
		job.LabelDo = "remove the first and add the second to redo it"
	case c.State == "running":
		job.LabelName = labels["in_progress"]
	}
	if run, ok := NewestRun(filepath.Join(s.opt.Config.Agent.Logdir, repo.Name), c.Workflow, c.Number); ok {
		job.LogName = run.File
		job.Tail = run.ToolCalls
		if len(job.Tail) > 4 {
			job.Tail = job.Tail[len(job.Tail)-4:]
		}
		if job.Detail == "" && run.LastError != "" {
			job.Detail = run.LastError
		}
	}
	return job
}

// --- handlers --------------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Render into a buffer first: a template error halfway through would otherwise
	// send a partial page and then a second WriteHeader ("superfluous response
	// WriteHeader call", seen once on a live box and impossible to diagnose after).
	var buf bytes.Buffer
	if err := s.tpl.Execute(&buf, s.build(r.Context())); err != nil {
		fmt.Fprintf(os.Stderr, "issuewright serve: dashboard render: %v\n", err)
		http.Error(w, "dashboard render failed; the error is in the service log", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

// itoa avoids pulling strconv in for one header.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Healthy means the state directory the page depends on is readable.
	info, err := os.Stat(filepath.Join(s.root, "state"))
	if err != nil || !info.IsDir() {
		http.Error(w, "state directory not readable: "+fmt.Sprint(err), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "ok %s\n", VersionOrUnknown(s.opt.Version))
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.build(r.Context()))
}

func (s *Server) handleAPIRuns(w http.ResponseWriter, r *http.Request) {
	repoName := r.URL.Query().Get("repo")
	if repoName == "" && len(s.opt.Config.Repos) == 1 {
		repoName = s.opt.Config.Repos[0].Name
	}
	repo, err := s.opt.Config.PickRepo(repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	runs, err := ListRuns(filepath.Join(s.opt.Config.Agent.Logdir, repo.Name), 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n := r.URL.Query().Get("n"); n != "" {
		var wanted int
		fmt.Sscanf(n, "%d", &wanted)
		filtered := runs[:0]
		for _, run := range runs {
			if run.Number == wanted {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
	writeJSON(w, runs)
}

// runLogName keeps /api/log inside the log directory: only a name the runner itself
// produces is accepted.
var runLogName = regexp.MustCompile(`^[a-z-]+-\d+-\d{8}T\d{6}-run\.jsonl$`)

func (s *Server) handleAPILog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !runLogName.MatchString(name) {
		http.Error(w, "not a run log name", http.StatusBadRequest)
		return
	}
	repoName := r.URL.Query().Get("repo")
	if repoName == "" && len(s.opt.Config.Repos) == 1 {
		repoName = s.opt.Config.Repos[0].Name
	}
	repo, err := s.opt.Config.PickRepo(repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.opt.Config.Agent.Logdir, repo.Name, name)
	sum, err := SummarizeFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		const tail = 64 * 1024
		if len(raw) > tail {
			raw = raw[len(raw)-tail:]
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(raw)
		return
	}
	writeJSON(w, sum)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

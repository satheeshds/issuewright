// Package web serves a read-only status page for the runner: what it is doing,
// what is stuck, and which label unblocks it.
//
// Everything here reads files the runner already writes — claims, cursors and JSONL
// run logs — so the page loads instantly and works with the network down.
package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RunSummary is what one JSONL run log says happened.
type RunSummary struct {
	File      string        `json:"file"`
	Workflow  string        `json:"workflow"`
	Number    int           `json:"number"`
	Started   time.Time     `json:"started"`
	Finished  time.Time     `json:"finished"`
	Duration  time.Duration `json:"-"`
	DurationS string        `json:"duration"`
	CostUSD   float64       `json:"cost_usd"`
	Tokens    int           `json:"tokens"`
	ToolCalls []string      `json:"tool_calls"`
	ToolCount int           `json:"tool_count"`
	LastError string        `json:"last_error,omitempty"`
	FinalText string        `json:"final_text,omitempty"`
	// Truncated means the file does not end with a newline: the process died
	// mid-write, which is exactly the interesting case on a box that suspends.
	Truncated bool  `json:"truncated"`
	Size      int64 `json:"size"`
}

// logName matches "<workflow>-<number>-<timestamp>-run.jsonl".
var logName = regexp.MustCompile(`^([a-z-]+)-(\d+)-\d{8}T\d{6}-run\.jsonl$`)

// maxToolCalls caps what a summary keeps; a run can execute hundreds of commands
// and the page only needs the tail.
const maxToolCalls = 12

// event is the subset of pi's JSONL event schema this page reads. The field names
// come from internal/brain/pi (usage.cost.total, message_end, tool_execution_start).
type event struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp"`
	Usage     *struct {
		TotalTokens int `json:"totalTokens"`
		Cost        struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage *struct {
			TotalTokens int `json:"totalTokens"`
			Cost        struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
		StopReason string `json:"stopReason"`
	} `json:"message"`
	ToolName     string `json:"toolName"`
	ErrorMessage string `json:"errorMessage"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`
	Args struct {
		Command string `json:"command"`
	} `json:"args"`
}

// SummarizeFile parses one run log. A malformed line is skipped, never fatal: a
// killed run leaves a half-written line, and the page still has to render it.
func SummarizeFile(path string) (RunSummary, error) {
	sum := RunSummary{File: filepath.Base(path)}
	if m := logName.FindStringSubmatch(sum.File); m != nil {
		sum.Workflow = m[1]
		sum.Number, _ = strconv.Atoi(m[2])
	}

	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		sum.Size = st.Size()
		sum.Started = st.ModTime()
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var pendingCost float64
	var pendingTokens int
	var lastTimestamp int64
	var sawNewline bool

	for scanner.Scan() {
		line := scanner.Text()
		if scanner.Err() != nil {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The runner writes one plain-text header line before the brain's events.
		if strings.HasPrefix(line, "# issuewright") {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Timestamp > 0 {
			later := time.UnixMilli(ev.Timestamp)
			if sum.Started.IsZero() || sum.Started.After(later) {
				sum.Started = later
			}
			if later.After(sum.Finished) {
				sum.Finished = later
			}
			lastTimestamp = ev.Timestamp
		}
		switch ev.Type {
		case "tool_execution_start":
			sum.ToolCount++
			if cmd := strings.TrimSpace(ev.Args.Command); cmd != "" {
				if len(cmd) > 160 {
					cmd = cmd[:160] + "…"
				}
				sum.ToolCalls = append(sum.ToolCalls, cmd)
				if len(sum.ToolCalls) > maxToolCalls {
					sum.ToolCalls = sum.ToolCalls[len(sum.ToolCalls)-maxToolCalls:]
				}
			}
		case "message_update":
			if ev.Usage != nil {
				// Cumulative *for this response*: keep the latest, add it at the end.
				pendingCost = ev.Usage.Cost.Total
				pendingTokens = ev.Usage.TotalTokens
			}
		case "message_end":
			if ev.Message != nil && ev.Message.Usage != nil {
				sum.CostUSD += ev.Message.Usage.Cost.Total
				sum.Tokens += ev.Message.Usage.TotalTokens
				pendingCost, pendingTokens = 0, 0
			} else {
				sum.CostUSD += pendingCost
				sum.Tokens += pendingTokens
				pendingCost, pendingTokens = 0, 0
			}
			if ev.Message != nil {
				if text := textOf(ev.Message.Content); text != "" {
					sum.FinalText = text
				}
				switch ev.Message.StopReason {
				case "error", "aborted":
					sum.LastError = "stop reason: " + ev.Message.StopReason
				}
			}
		case "error":
			if ev.ErrorMessage != "" {
				sum.LastError = ev.ErrorMessage
			} else if ev.Error != nil {
				sum.LastError = ev.Error.Message
			}
		}
	}
	// A response that never finished still cost something: count what we saw.
	sum.CostUSD += pendingCost
	sum.Tokens += pendingTokens

	if _, err := f.Seek(-1, 2); err == nil {
		var last [1]byte
		if _, err := f.Read(last[:]); err == nil {
			sawNewline = last[0] == '\n'
		}
	}
	sum.Truncated = !sawNewline

	if !sum.Finished.IsZero() && !sum.Started.IsZero() && sum.Finished.After(sum.Started) {
		sum.Duration = sum.Finished.Sub(sum.Started)
	} else if lastTimestamp > 0 && !sum.Started.IsZero() {
		sum.Duration = time.Since(sum.Started)
	}
	sum.DurationS = HumanDuration(sum.Duration)
	return sum, nil
}

// ListRuns returns the newest logs in a repo's log directory, newest first.
func ListRuns(logDir string, limit int) ([]RunSummary, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type fileInfo struct {
		path string
		mod  time.Time
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(logDir, e.Name()), info.ModTime()})
	}
	// Newest first, without sorting the whole directory by name.
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j].mod.After(files[j-1].mod); j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	out := make([]RunSummary, 0, len(files))
	for _, f := range files {
		if sum, err := SummarizeFile(f.path); err == nil {
			out = append(out, sum)
		}
	}
	return out, nil
}

// NewestRun finds the most recent log for one job, which is the "Now" block's tail.
func NewestRun(logDir, workflow string, number int) (RunSummary, bool) {
	runs, err := ListRuns(logDir, 40)
	if err != nil {
		return RunSummary{}, false
	}
	for _, r := range runs {
		if r.Workflow == workflow && r.Number == number {
			return r, true
		}
	}
	return RunSummary{}, false
}

// HumanDuration renders a duration the way a person reads it in a status line.
func HumanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func textOf(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var b strings.Builder
	for _, c := range content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

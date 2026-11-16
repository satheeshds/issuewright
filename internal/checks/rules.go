// Package checks turns the profile's path globs into verification runs, and
// enforces the profile's hard rules.
//
// The split matters: Select and RunCheck answer "what does this change need",
// while DeniedWrites, SecretFiles and ScanToolCalls answer "did the agent stay
// inside the lines". The engine refuses to publish on either.
package checks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
)

var globCache sync.Map // pattern → *regexp.Regexp

// Match reports whether a repo-relative path matches an issuewright glob.
//
// Globs support `*` (within a segment), `?` and `**` (any number of segments), so
// `apps/api_server/**/*.go` matches both nested and top-level files.
func Match(pattern, path string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	path = strings.TrimPrefix(strings.TrimSpace(path), "./")
	if pattern == "" || path == "" {
		return false
	}
	re := compile(pattern)
	return re.MatchString(path)
}

// MatchAny reports whether any pattern matches.
func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}

// Filter returns the paths matching any pattern, in input order.
func Filter(patterns []string, paths []string) []string {
	var out []string
	for _, p := range paths {
		if MatchAny(patterns, p) {
			out = append(out, p)
		}
	}
	return out
}

func compile(pattern string) *regexp.Regexp {
	if v, ok := globCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteString("^")
	// "**/" first: it may match zero segments, which plain "**" cannot express.
	p := strings.ReplaceAll(pattern, "**/", "\x00")
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\x00':
			b.WriteString("(?:.*/)?")
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				i++
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	b.WriteString("$")
	re := regexp.MustCompile(b.String())
	globCache.Store(pattern, re)
	return re
}

// SecretFiles returns changed paths that look like secrets. These must never
// appear in a pushed diff, whatever a prompt said.
func SecretFiles(patterns []string, changed []string) []string {
	return Filter(patterns, changed)
}

// DeniedWrites returns changed paths the agent was told never to touch by hand.
func DeniedWrites(patterns []string, changed []string) []string {
	return Filter(patterns, changed)
}

// ForbiddenCommand reports whether a command line starts with (or contains) one
// of the profile's never_run prefixes.
func ForbiddenCommand(neverRun []string, command string) (string, bool) {
	cmd := strings.Join(strings.Fields(command), " ")
	for _, bad := range neverRun {
		bad = strings.Join(strings.Fields(bad), " ")
		if bad == "" {
			continue
		}
		if cmd == bad || strings.HasPrefix(cmd, bad+" ") || strings.Contains(cmd, " "+bad+" ") ||
			strings.HasSuffix(cmd, " "+bad) || strings.Contains(cmd, bad+"\n") {
			return bad, true
		}
	}
	return "", false
}

// ToolCall is one command an agent ran, recovered from the run log.
type ToolCall struct {
	Tool    string
	Command string
}

// ScanToolCalls reads a JSONL run log (pi/opencode style events) and extracts the
// commands the agent executed, so never_run can be enforced rather than trusted.
func ScanToolCalls(path string) ([]ToolCall, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanToolCalls(f)
}

func scanToolCalls(r io.Reader) ([]ToolCall, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var calls []ToolCall
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			ToolName string `json:"toolName"`
			Args     struct {
				Command string `json:"command"`
			} `json:"args"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "tool_execution_start" || ev.Args.Command == "" {
			continue
		}
		calls = append(calls, ToolCall{Tool: ev.ToolName, Command: ev.Args.Command})
	}
	return calls, sc.Err()
}

// ForbiddenCalls returns the tool calls that violated never_run.
func ForbiddenCalls(calls []ToolCall, neverRun []string) []string {
	var out []string
	for _, c := range calls {
		if bad, found := ForbiddenCommand(neverRun, c.Command); found {
			out = append(out, fmt.Sprintf("%q (matches never_run %q)", c.Command, bad))
		}
	}
	return out
}

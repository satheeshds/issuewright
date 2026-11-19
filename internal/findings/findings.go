// Package findings parses a reviewer's structured output: the machine-readable
// half of a review, so its comments can land on the lines they talk about
// instead of in one wall of summary text.
//
// The format is YAML front matter on REVIEW.md, which keeps the prose body
// available for humans and for brains that only write markdown:
//
//	---
//	findings:
//	  - severity: blocking
//	    path: services/api/handler.go
//	    line: 42
//	    message: the tenant check is gone
//	---
//	## Summary
//	...
package findings

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Severity orders a finding. Anything unknown is kept as-is but treated as
// non-blocking; the reviewer's judgement matters more than its vocabulary.
type Severity string

// Known severities.
const (
	Blocking    Severity = "blocking"
	NonBlocking Severity = "non-blocking"
	Question    Severity = "question"
)

// Finding is one review comment.
type Finding struct {
	Severity Severity `yaml:"severity"`
	Path     string   `yaml:"path"`
	Line     int      `yaml:"line,omitempty"`
	Message  string   `yaml:"message"`
	// Suggestion is optional replacement code for a single line.
	Suggestion string `yaml:"suggestion,omitempty"`
}

// Review is a parsed review: findings plus the prose body.
type Review struct {
	Findings []Finding
	Body     string
	// Title, when set, is used as the review's headline.
	Title string
}

// Parse reads REVIEW.md. A file with no front matter is valid: it is all body,
// and the review is posted as a single comment (the pre-findings behaviour).
func Parse(raw string) (*Review, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	rev := &Review{Body: raw}
	if !strings.HasPrefix(raw, "---") {
		return rev, nil
	}

	// Front matter ends at the next line that is exactly "---".
	rest := raw[3:]
	trimmed := strings.TrimLeft(rest, "\n")
	idx := strings.Index(trimmed, "\n---")
	if idx < 0 {
		return rev, nil // not front matter after all
	}
	header := trimmed[:idx]
	body := strings.TrimPrefix(trimmed[idx:], "\n---")
	rev.Body = strings.TrimSpace(body)

	var front struct {
		Title    string    `yaml:"title"`
		Findings []Finding `yaml:"findings"`
	}
	if err := yaml.Unmarshal([]byte(header), &front); err != nil {
		return nil, fmt.Errorf("review front matter: %w", err)
	}
	rev.Title = front.Title
	for _, f := range front.Findings {
		f.Path = strings.TrimPrefix(strings.TrimSpace(f.Path), "./")
		f.Message = strings.TrimSpace(f.Message)
		if f.Path == "" || f.Message == "" {
			return nil, fmt.Errorf("finding needs a path and a message (got path=%q)", f.Path)
		}
		if f.Severity == "" {
			f.Severity = NonBlocking
		}
		rev.Findings = append(rev.Findings, f)
	}
	return rev, nil
}

// Counts reports how many findings there are per severity, for the summary line.
func Counts(fs []Finding) map[Severity]int {
	out := map[Severity]int{}
	for _, f := range fs {
		out[f.Severity]++
	}
	return out
}

// HasBlocking reports whether anything blocks the merge.
func HasBlocking(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == Blocking {
			return true
		}
	}
	return false
}

// CommentBody is the text of an inline comment: the severity, the message, and
// an optional suggested replacement.
func (f Finding) CommentBody() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s]** %s\n", f.Severity, f.Message)
	if f.Suggestion != "" {
		b.WriteString("\n```suggestion\n" + strings.TrimRight(f.Suggestion, "\n") + "\n```\n")
	}
	return b.String()
}

// SummaryLine renders "1 blocking, 2 non-blocking" for a review body.
func SummaryLine(fs []Finding) string {
	if len(fs) == 0 {
		return "no findings"
	}
	c := Counts(fs)
	parts := make([]string, 0, 3)
	for _, s := range []Severity{Blocking, NonBlocking, Question} {
		if n := c[s]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+string(s))
		}
	}
	return strings.Join(parts, ", ")
}

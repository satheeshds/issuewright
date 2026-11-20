package findings

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reply is one answer to one comment.
type Reply struct {
	InReplyTo int    `yaml:"in_reply_to"`
	Body      string `yaml:"body"`
}

// Change is one file the run touched, so its reply can name the commit.
type Change struct {
	Path    string `yaml:"path"`
	Summary string `yaml:"summary"`
}

// Response is a parsed answer to review comments.
type Response struct {
	Replies []Reply
	Changes []Change
	Body    string
}

// ParseResponse reads the respond workflow's output. Like Parse, prose without
// front matter is valid and becomes the body.
func ParseResponse(raw string) (*Response, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	resp := &Response{Body: raw}
	if !strings.HasPrefix(raw, "---") {
		return resp, nil
	}
	rest := strings.TrimLeft(raw[3:], "\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return resp, nil
	}
	header := rest[:idx]
	resp.Body = strings.TrimSpace(strings.TrimPrefix(rest[idx:], "\n---"))

	var front struct {
		Replies []Reply  `yaml:"replies"`
		Changes []Change `yaml:"changes"`
	}
	if err := yaml.Unmarshal([]byte(header), &front); err != nil {
		return nil, fmt.Errorf("reply front matter: %w", err)
	}
	for _, r := range front.Replies {
		r.Body = strings.TrimSpace(r.Body)
		if r.InReplyTo == 0 || r.Body == "" {
			return nil, fmt.Errorf("reply needs in_reply_to and body (got %d)", r.InReplyTo)
		}
		resp.Replies = append(resp.Replies, r)
	}
	resp.Changes = front.Changes
	return resp, nil
}

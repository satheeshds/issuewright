// Conversation state: one record per (workflow, issue-or-PR), which is what makes
// "one agent per issue" possible across ticks.
//
// A claim is about one attempt. A conversation is about the whole life of the
// target: which brain session holds the history, how many turns it has had, which
// comments it has already answered, and what the reviewer said last time. It lives
// beside the claims, in conversations/, so a human can read it, delete it, or
// correct a session id by hand.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Conversation states. "asking" is the one that is new: the agent has a question
// on GitHub and is waiting for a human, which is not a failure and must not be
// retried on the next tick.
const (
	ConversationWorking  = "working"
	ConversationAsking   = "asking"
	ConversationDone     = "done"
	ConversationBlocked  = "blocked"
	ConversationArchived = "archived"
)

// Conversation is the durable identity of one agent and its target.
type Conversation struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Number   int    `json:"number"`

	// SessionID is the brain's own conversation handle. It is deterministic and
	// recorded, so a lost state file can be rebuilt and a wrong one corrected.
	SessionID string `json:"session_id"`
	// Gen counts restarts ("@bot restart"): a restart keeps the record and opens a
	// fresh session, so the history starts clean without losing the audit trail.
	Gen int `json:"gen,omitempty"`

	Cwd    string `json:"cwd,omitempty"`
	Branch string `json:"branch,omitempty"`
	State  string `json:"state"`

	Turns      int       `json:"turns"`
	CostUSD    float64   `json:"cost_usd,omitempty"`
	OpenedAt   time.Time `json:"opened_at"`
	LastTurnAt time.Time `json:"last_turn_at,omitempty"`

	// LastCommentID and Answered are the turn cursor: a comment id is answered
	// once, which is where a turn's exactly-once semantics come from.
	LastCommentID int   `json:"last_comment_id"`
	Answered      []int `json:"answered,omitempty"`

	// LastQuestion is the question this agent asked on GitHub, kept so the turn that
	// answers it can quote it even on a brain with no session history.
	LastQuestion string `json:"last_question,omitempty"`

	// Review conversations only: the revision reviewed last, and the review it
	// produced, so the next revision's turn can open with "fixed / still open".
	ReviewedSHA string `json:"reviewed_sha,omitempty"`
	LastReview  string `json:"last_review,omitempty"`
}

// SessionIDFor is the deterministic session id for a target. Brain CLIs restrict
// session ids to letters, numbers, '.', '_' and '-', starting and ending with a
// letter or number, so the repo name is folded into that alphabet.
//
// gen 0 is the first session; gen n > 0 is the n-th restart.
func SessionIDFor(repo, workflow string, number, gen int) string {
	id := "iw-" + sanitizeID(repo) + "-" + sanitizeID(workflow) + "-" + fmt.Sprint(number)
	if gen > 0 {
		id += fmt.Sprintf("-r%d", gen)
	}
	return id
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	// Collapse runs of dashes so "a//b" and "a/b" do not collide differently.
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		return "repo"
	}
	return out
}

// Seen reports whether a comment id is already answered.
func (c Conversation) Seen(id int) bool {
	for _, a := range c.Answered {
		if a == id {
			return true
		}
	}
	return false
}

// MarkSeen records a comment id as answered, keeping the list bounded.
func (c *Conversation) MarkSeen(id int) {
	if c.Seen(id) {
		return
	}
	c.Answered = append(c.Answered, id)
	if id > c.LastCommentID {
		c.LastCommentID = id
	}
	if len(c.Answered) > maxAnswered {
		c.Answered = c.Answered[len(c.Answered)-maxAnswered:]
	}
}

// Turn records a completed turn: the counter, the time and the cost.
func (c *Conversation) Turn(now time.Time, costUSD float64) {
	c.Turns++
	c.LastTurnAt = now
	c.CostUSD += costUSD
}

func (s *Store) conversationDir(repo string) string {
	return filepath.Join(s.RepoDir(repo), "conversations")
}

func (s *Store) conversationPath(repo, workflow string, number int) string {
	return filepath.Join(s.conversationDir(repo), fmt.Sprintf("%s-%d.json", workflow, number))
}

// LoadConversation returns the conversation for a target, if any. A corrupt file
// is reported as absent: a broken state file must not wedge a repo, and the worst
// case is one cold turn.
func (s *Store) LoadConversation(repo, workflow string, number int) (Conversation, bool, error) {
	raw, err := os.ReadFile(s.conversationPath(repo, workflow, number))
	if errors.Is(err, os.ErrNotExist) {
		return Conversation{}, false, nil
	}
	if err != nil {
		return Conversation{}, false, err
	}
	var c Conversation
	if err := json.Unmarshal(raw, &c); err != nil {
		return Conversation{}, false, nil
	}
	return c, true, nil
}

// SaveConversation writes a conversation atomically.
func (s *Store) SaveConversation(c Conversation) error {
	if err := os.MkdirAll(s.conversationDir(c.Repo), 0o755); err != nil {
		return err
	}
	if c.SessionID == "" {
		c.SessionID = SessionIDFor(c.Repo, c.Workflow, c.Number, c.Gen)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := s.conversationPath(c.Repo, c.Workflow, c.Number)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Conversations lists every live (not archived) conversation in a repo, sorted by
// workflow then number.
func (s *Store) Conversations(repo string) ([]Conversation, error) {
	entries, err := os.ReadDir(s.conversationDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Conversation
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.conversationDir(repo), e.Name()))
		if err != nil {
			continue
		}
		var c Conversation
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		if c.State == ConversationArchived {
			continue
		}
		out = append(out, c)
	}
	sortConversations(out)
	return out, nil
}

func sortConversations(cs []Conversation) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0; j-- {
			a, b := cs[j-1], cs[j]
			if a.Workflow < b.Workflow || (a.Workflow == b.Workflow && a.Number <= b.Number) {
				break
			}
			cs[j-1], cs[j] = b, a
		}
	}
}

// FindConversation looks a conversation up by workflow name and number.
func (s *Store) FindConversation(repo, workflow string, number int) (Conversation, bool) {
	c, ok, err := s.LoadConversation(repo, workflow, number)
	if err != nil || !ok {
		return Conversation{}, false
	}
	return c, true
}

// ConversationByBranch returns the implement conversation whose branch matches,
// which is how a comment on the bot's own PR finds the agent that wrote it.
func (s *Store) ConversationByBranch(repo, branch string) (Conversation, bool) {
	if branch == "" {
		return Conversation{}, false
	}
	cs, err := s.Conversations(repo)
	if err != nil {
		return Conversation{}, false
	}
	for _, c := range cs {
		if c.Branch != "" && c.Branch == branch {
			return c, true
		}
	}
	return Conversation{}, false
}

// ArchiveConversation retires a conversation: the target was merged or closed.
// The record is moved, not deleted, because it is the audit trail of who worked
// on what, with which session and at what cost.
func (s *Store) ArchiveConversation(repo, workflow string, number int) error {
	path := s.conversationPath(repo, workflow, number)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var c Conversation
	if err := json.Unmarshal(raw, &c); err == nil {
		c.State = ConversationArchived
		if out, merr := json.MarshalIndent(c, "", "  "); merr == nil {
			raw = out
		}
	}
	dir := filepath.Join(s.conversationDir(repo), "archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), raw, 0o644); err != nil {
		return err
	}
	return os.Remove(path)
}

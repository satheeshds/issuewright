package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxAnswered caps the answered-comment memory so a long-lived PR does not grow a
// state file forever.
const maxAnswered = 200

// Cursor remembers which comments have been answered, so a killed run does not
// reply twice and a tick does not re-answer the same question.
type Cursor struct {
	LastCommentID int   `json:"last_comment_id"`
	Answered      []int `json:"answered"`
}

// Answered reports whether a comment id is already handled.
func (c Cursor) Has(id int) bool {
	for _, a := range c.Answered {
		if a == id {
			return true
		}
	}
	return false
}

// MarkAnswered records a comment id, keeping the list bounded.
func (c *Cursor) MarkAnswered(id int) {
	if c.Has(id) {
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

func (s *Store) cursorPath(repo string, number int) string {
	return filepath.Join(s.RepoDir(repo), fmt.Sprintf("respond-%d.json", number))
}

// LoadCursor reads the cursor for a PR, returning an empty one when absent.
func (s *Store) LoadCursor(repo string, number int) (Cursor, error) {
	raw, err := os.ReadFile(s.cursorPath(repo, number))
	if errors.Is(err, os.ErrNotExist) {
		return Cursor{}, nil
	}
	if err != nil {
		return Cursor{}, err
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		// A corrupt cursor must not wedge the PR: start over, and the answered set
		// rebuilding is cheaper than never replying again.
		return Cursor{}, nil
	}
	return c, nil
}

// SaveCursor writes the cursor atomically.
func (s *Store) SaveCursor(repo string, number int, c Cursor) error {
	if err := os.MkdirAll(s.RepoDir(repo), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := s.cursorPath(repo, number)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

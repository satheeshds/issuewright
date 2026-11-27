// Commit claims: a run that says "Fixed in b9ba799" is making a promise to a human,
// and the promise is checkable. This is where it gets checked.
package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// commitID matches something shaped like an abbreviated commit id.
var commitID = regexp.MustCompile(`(?i)[0-9a-f]{7,40}`)

// claimedCommits returns the commit ids a text names.
//
// The exclusions matter more than the match. A sha inside a URL, a digest
// (`sha256:…`), or another repository's reference (`owner/repo@abc1234`) is a
// citation, not a claim about this branch, so it is skipped. So is a run of hex
// with no digit at all, which is a word ("deadbeef") far more often than a commit.
func claimedCommits(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range commitID.FindAllStringIndex(text, -1) {
		id := text[loc[0]:loc[1]]
		var before, after byte
		if loc[0] > 0 {
			before = text[loc[0]-1]
		}
		if loc[1] < len(text) {
			after = text[loc[1]]
		}
		if isHexByte(before) || isHexByte(after) {
			continue // part of a longer hex run
		}
		switch before {
		case ':', '/', '@', '.', '=':
			continue
		}
		if after == '/' || after == ':' {
			continue
		}
		if !containsDigit(id) {
			continue
		}
		low := strings.ToLower(id)
		if !seen[low] {
			seen[low] = true
			out = append(out, low)
		}
	}
	return out
}

func isHexByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func containsDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// verifyClaims refuses a run whose text names a commit that the branch cannot
// back up.
//
// The rule is "exists, and is in this worktree's history". A model that invents an
// id fails the first half; a model that names a commit from somewhere else fails
// the second. Refusing is the point: a claim a human cannot verify is worse than no
// claim at all, because it tells them to stop looking.
func (e *Engine) verifyClaims(ctx context.Context, dir string, texts ...string) error {
	var bad []string
	for _, text := range texts {
		for _, id := range claimedCommits(text) {
			exists, inHistory := e.wts.IsCommit(ctx, dir, id)
			if exists && inHistory {
				continue
			}
			bad = append(bad, id)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("the run names commit(s) that are not in this branch: %s\n\n"+
		"Nothing was posted, because a human cannot check that claim and would stop looking for the change",
		strings.Join(bad, ", "))
}

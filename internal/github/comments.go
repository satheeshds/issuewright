package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Comment is one thing a human (or the bot) wrote on a pull request. Threads are
// GraphQL's idea — REST cannot tell you whether one is resolved.
type Comment struct {
	ID        int       `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	CreatedAt time.Time `json:"createdAt"`
	// IsReview marks an inline review comment (answered on its own thread) as
	// opposed to a conversation comment (answered by a new comment).
	IsReview bool `json:"isReview"`
}

// IsFrom reports whether a login is this comment's author, tolerating the two
// shapes GitHub uses: `name` in GraphQL, `name[bot]` in REST/PR JSON.
func (c Comment) IsFrom(login string) bool {
	if login == "" {
		return false
	}
	norm := func(s string) string {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "[bot]"))
	}
	return norm(c.Author) == norm(login)
}

// IsBot reports whether the author is a bot of any kind: our own login or any
// other `[bot]`-suffixed account. Answering another bot is how reply loops start.
func (c Comment) IsBot(selfLogin string) bool {
	return c.IsFrom(selfLogin) || strings.Contains(strings.ToLower(c.Author), "[bot]") ||
		strings.HasSuffix(strings.ToLower(c.Author), "-bot")
}

// CommentThread is an inline review thread.
type CommentThread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Comments   []Comment
}

// ListCommentThreads returns inline review threads and conversation comments for
// a pull request, in one GraphQL query.
func (c *CLI) ListCommentThreads(ctx context.Context, slug string, pr int) ([]CommentThread, []Comment, error) {
	owner, repo, err := splitSlug(slug)
	if err != nil {
		return nil, nil, err
	}
	query := `query($owner:String!,$repo:String!,$pr:Int!){
	  repository(owner:$owner,name:$repo){
	    pullRequest(number:$pr){
	      reviewThreads(first:50){
	        nodes{
	          id isResolved isOutdated
	          comments(first:50){ nodes{ databaseId body path line createdAt author{login} } }
	        }
	      }
	      comments(last:50){ nodes{ databaseId body createdAt author{login} } }
	    }
	  }
	}`
	out, err := c.gh(ctx, "api", "graphql",
		"-f", "query="+query,
		"-f", "owner="+owner, "-f", "repo="+repo, "-F", "pr="+strconv.Itoa(pr))
	if err != nil {
		return nil, nil, err
	}

	var parsed struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							IsOutdated bool   `json:"isOutdated"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int       `json:"databaseId"`
									Body       string    `json:"body"`
									Path       string    `json:"path"`
									Line       int       `json:"line"`
									CreatedAt  time.Time `json:"createdAt"`
									Author     struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
					Comments struct {
						Nodes []struct {
							DatabaseID int       `json:"databaseId"`
							Body       string    `json:"body"`
							CreatedAt  time.Time `json:"createdAt"`
							Author     struct {
								Login string `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, nil, fmt.Errorf("parse comment threads: %w", err)
	}

	prData := parsed.Data.Repository.PullRequest
	var threads []CommentThread
	for _, t := range prData.ReviewThreads.Nodes {
		thread := CommentThread{ID: t.ID, IsResolved: t.IsResolved, IsOutdated: t.IsOutdated}
		for _, cm := range t.Comments.Nodes {
			thread.Comments = append(thread.Comments, Comment{
				ID: cm.DatabaseID, Author: cm.Author.Login, Body: cm.Body,
				Path: cm.Path, Line: cm.Line, CreatedAt: cm.CreatedAt, IsReview: true,
			})
		}
		threads = append(threads, thread)
	}

	var conversation []Comment
	for _, cm := range prData.Comments.Nodes {
		conversation = append(conversation, Comment{
			ID: cm.DatabaseID, Author: cm.Author.Login, Body: cm.Body, CreatedAt: cm.CreatedAt,
		})
	}
	return threads, conversation, nil
}

// ListComments returns the conversation comments on an issue or pull request that
// are newer than afterID, oldest first.
//
// One REST call, newest page only: a conversation is answered from its recent
// comments, and paging back through a two-hundred-comment thread every tick would
// cost more than it tells us. Filtering by id rather than by timestamp keeps the
// cursor exact even when two comments share a second.
func (c *CLI) ListComments(ctx context.Context, slug string, number, afterID int) ([]Comment, error) {
	out, err := c.gh(ctx, "api", fmt.Sprintf("repos/%s/issues/%d/comments", slug, number),
		"-f", "per_page=100", "-f", "sort=created", "-f", "direction=desc")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID        int       `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}
	comments := make([]Comment, 0, len(raw))
	// The API answers newest first; a prompt reads oldest first, so the turn sees
	// the conversation in the order the humans had it.
	for i := len(raw) - 1; i >= 0; i-- {
		r := raw[i]
		if r.ID <= afterID {
			continue
		}
		comments = append(comments, Comment{
			ID: r.ID, Author: r.User.Login, Body: r.Body, CreatedAt: r.CreatedAt,
		})
	}
	return comments, nil
}

// ReplyToComment answers a comment where it was written: on the review thread for
// an inline comment, or as a new conversation comment otherwise.
func (c *CLI) ReplyToComment(ctx context.Context, slug string, pr int, to Comment, body string) error {
	if to.IsReview {
		_, err := c.gh(ctx, "api", "-X", "POST",
			fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", slug, pr, to.ID),
			"-f", "body="+body)
		return err
	}
	// A conversation comment has no thread to reply into: quote what we answer so
	// the exchange stays readable.
	quoted := "> " + strings.ReplaceAll(strings.TrimSpace(firstLine(to.Body)), "\n", "\n> ") + "\n\n" + body
	_, err := c.gh(ctx, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/issues/%d/comments", slug, pr), "-f", "body="+quoted)
	return err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// ReactToComment adds a reaction to a comment. Used to acknowledge a comment
// without spending a comment on "looking at this".
func (c *CLI) ReactToComment(ctx context.Context, slug string, pr int, to Comment, content string) error {
	path := fmt.Sprintf("repos/%s/issues/comments/%d/reactions", slug, to.ID)
	if to.IsReview {
		path = fmt.Sprintf("repos/%s/pulls/comments/%d/reactions", slug, to.ID)
	}
	_, err := c.gh(ctx, "api", "-X", "POST", path, "-f", "content="+content)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// ResolveThread marks a review thread resolved. GraphQL only: REST cannot.
func (c *CLI) ResolveThread(ctx context.Context, threadID string) error {
	mutation := `mutation($thread:ID!){ resolveReviewThread(input:{threadId:$thread}){ thread{ isResolved } } }`
	_, err := c.gh(ctx, "api", "graphql", "-f", "query="+mutation, "-f", "thread="+threadID)
	return err
}

// ListOpenPRs returns open pull requests, for finding the ones the runner has
// touched (its own branch prefix, or a review it posted).
func (c *CLI) ListOpenPRs(ctx context.Context, slug string) ([]PR, error) {
	out, err := c.gh(ctx, "pr", "list", "--repo", slug, "--state", "open",
		"--limit", "50", "--json", prFields)
	if err != nil {
		return nil, err
	}
	var raw []prJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse pr list: %w", err)
	}
	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, r.toPR())
	}
	return prs, nil
}

func splitSlug(slug string) (string, string, error) {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("slug %q is not owner/repo", slug)
	}
	return parts[0], parts[1], nil
}

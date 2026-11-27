package engine

import (
	"testing"

	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/state"
)

func TestTurnStart(t *testing.T) {
	const bot = "demo-bot[bot]"
	handles := []string{bot, "demo-bot"}
	asking := state.Conversation{State: state.ConversationAsking}
	done := state.Conversation{State: state.ConversationDone}
	answered := state.Conversation{State: state.ConversationAsking}
	answered.MarkSeen(42)

	cases := []struct {
		name    string
		conv    state.Conversation
		comment github.Comment
		ownPR   bool
		owned   bool
		want    bool
	}{
		{"the answer to a question", asking, github.Comment{ID: 1, Author: "human", Body: "use the second option"}, false, false, true},
		{"a mention in a settled thread", done, github.Comment{ID: 2, Author: "human", Body: "@demo-bot ptal"}, false, false, true},
		{"a reply in a thread the bot opened", done, github.Comment{ID: 3, Author: "human", Body: "why?"}, false, true, true},
		{"feedback on the agent's own pull request", done, github.Comment{ID: 9, Author: "human", Body: "this loop is off by one"}, true, false, true},
		{"chatter on a done issue", done, github.Comment{ID: 4, Author: "human", Body: "nice, thanks!"}, false, false, false},
		{"chatter on someone else's pull request", done, github.Comment{ID: 10, Author: "human", Body: "lgtm"}, false, false, false},
		{"ours", asking, github.Comment{ID: 5, Author: bot, Body: "which option?"}, true, true, false},
		{"ours without the suffix", asking, github.Comment{ID: 6, Author: "demo-bot", Body: "which option?"}, false, false, false},
		{"another bot", asking, github.Comment{ID: 7, Author: "other[bot]", Body: "hello"}, true, true, false},
		{"already answered", answered, github.Comment{ID: 42, Author: "human", Body: "yes"}, true, true, false},
		{"empty body", asking, github.Comment{ID: 8, Author: "human", Body: "   "}, false, false, false},
		{"no id", asking, github.Comment{ID: 0, Author: "human", Body: "hi"}, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := turnStart(c.conv, c.comment, bot, handles, c.ownPR, c.owned); got != c.want {
				t.Errorf("turnStart = %v, want %v", got, c.want)
			}
		})
	}
}

package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	c := Conversation{
		Repo: "demo", Workflow: "implement", Number: 12,
		Cwd: "/work/issue-12", Branch: "agent/issue-12",
		State: ConversationWorking, OpenedAt: now, CostUSD: 0.5,
	}
	c.MarkSeen(41)
	c.Turn(now, 0.25)
	if err := s.SaveConversation(c); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.LoadConversation("demo", "implement", 12)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "iw-demo-implement-12" {
		t.Errorf("session id = %q, want iw-demo-implement-12", got.SessionID)
	}
	if got.Branch != "agent/issue-12" || got.Turns != 1 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if !got.Seen(41) {
		t.Error("answered comment id lost")
	}
	if got.CostUSD != 0.75 {
		t.Errorf("cost = %v, want 0.75 (0.5 spent + 0.25 this turn)", got.CostUSD)
	}
}

func TestConversationSessionIDIsSafeForACLIFlag(t *testing.T) {
	cases := []struct {
		repo, workflow string
		number, gen    int
		want           string
	}{
		{"issuewright", "implement", 12, 0, "iw-issuewright-implement-12"},
		{"you/repo", "review", 7, 0, "iw-you-repo-review-7"},
		{"My_Repo.", "implement", 3, 2, "iw-my-repo-implement-3-r2"},
	}
	for _, c := range cases {
		if got := SessionIDFor(c.repo, c.workflow, c.number, c.gen); got != c.want {
			t.Errorf("SessionIDFor(%q,%q,%d,%d) = %q, want %q", c.repo, c.workflow, c.number, c.gen, got, c.want)
		}
	}
	id := SessionIDFor("a//b", "implement", 1, 0)
	if strings.Contains(id, "--") {
		t.Errorf("session id %q has a doubled dash", id)
	}
	if first := id[0]; first == '-' || first == '.' {
		t.Errorf("session id %q must start with a letter or number", id)
	}
	last := id[len(id)-1]
	if last == '-' || last == '.' {
		t.Errorf("session id %q must end with a letter or number", id)
	}
}

func TestConversationCursorIsBounded(t *testing.T) {
	var c Conversation
	for i := 1; i <= maxAnswered+25; i++ {
		c.MarkSeen(i)
	}
	if len(c.Answered) != maxAnswered {
		t.Fatalf("answered kept %d ids, want %d", len(c.Answered), maxAnswered)
	}
	if !c.Seen(maxAnswered + 25) {
		t.Error("the newest id must survive the cap")
	}
	if c.LastCommentID != maxAnswered+25 {
		t.Errorf("last comment id = %d, want %d", c.LastCommentID, maxAnswered+25)
	}
	c.MarkSeen(5) // duplicate, and old: no new entry
	if len(c.Answered) != maxAnswered {
		t.Errorf("duplicate grew the cursor to %d", len(c.Answered))
	}
}

func TestConversationByBranchFindsTheAuthor(t *testing.T) {
	s := New(t.TempDir())
	impl := Conversation{Repo: "demo", Workflow: "implement", Number: 12,
		Branch: "agent/issue-12", State: ConversationDone}
	review := Conversation{Repo: "demo", Workflow: "review", Number: 44,
		Branch: "feature/x", State: ConversationDone}
	for _, c := range []Conversation{impl, review} {
		if err := s.SaveConversation(c); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := s.ConversationByBranch("demo", "agent/issue-12")
	if !ok || got.Workflow != "implement" || got.Number != 12 {
		t.Fatalf("by branch = %+v, ok=%v", got, ok)
	}
	if _, ok := s.ConversationByBranch("demo", "someone/else"); ok {
		t.Error("an unknown branch must not match")
	}
	if _, ok := s.ConversationByBranch("demo", ""); ok {
		t.Error("an empty branch must not match")
	}
}

func TestArchiveConversationMovesTheRecord(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	c := Conversation{Repo: "demo", Workflow: "implement", Number: 7, State: ConversationDone}
	if err := s.SaveConversation(c); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveConversation("demo", "implement", 7); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LoadConversation("demo", "implement", 7); ok {
		t.Error("an archived conversation must leave the live set")
	}
	live, err := s.Conversations("demo")
	if err != nil || len(live) != 0 {
		t.Fatalf("live = %v, err = %v", live, err)
	}
	archived := filepath.Join(s.RepoDir("demo"), "conversations", "archive", "implement-7.json")
	raw, err := os.ReadFile(archived)
	if err != nil {
		t.Fatalf("archive should keep the record: %v", err)
	}
	if !strings.Contains(string(raw), ConversationArchived) {
		t.Errorf("archived record should say so: %s", raw)
	}
	// Archiving twice is a no-op, not an error: the tick may see a close twice.
	if err := s.ArchiveConversation("demo", "implement", 7); err != nil {
		t.Errorf("second archive: %v", err)
	}
}

func TestCorruptConversationIsTreatedAsAbsent(t *testing.T) {
	s := New(t.TempDir())
	path := s.conversationPath("demo", "implement", 3)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LoadConversation("demo", "implement", 3); ok || err != nil {
		t.Fatalf("corrupt conversation: ok=%v err=%v, want absent", ok, err)
	}
	if cs, err := s.Conversations("demo"); err != nil || len(cs) != 0 {
		t.Errorf("corrupt conversation listed: %v, %v", cs, err)
	}
}

func TestClaimsIgnoreConversationFiles(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SaveClaim(Claim{Repo: "demo", Workflow: "implement", Number: 1, State: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConversation(Conversation{Repo: "demo", Workflow: "implement", Number: 1}); err != nil {
		t.Fatal(err)
	}
	claims, err := s.Claims("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1 (conversations live in their own directory)", len(claims))
	}
}

package workflow

import (
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/brain"
)

type fakeWF struct {
	name     string
	triggers []Trigger
	perms    Perms
}

func (f fakeWF) Name() string        { return f.name }
func (f fakeWF) Triggers() []Trigger { return f.triggers }
func (f fakeWF) Perms() Perms        { return f.perms }
func (f fakeWF) Prompt(Job) (brain.Spec, error) {
	return brain.Spec{Prompt: f.name}, nil
}

func TestRegisterGetNames(t *testing.T) {
	Register(fakeWF{name: "test-wf"})
	t.Cleanup(func() { Unregister("test-wf") })
	w, err := Get("test-wf")
	if err != nil || w.Name() != "test-wf" {
		t.Fatalf("Get = %v %v", w, err)
	}
	found := false
	for _, n := range Names() {
		if n == "test-wf" {
			found = true
		}
	}
	if !found {
		t.Errorf("Names = %v", Names())
	}
}

func TestUnknownWorkflowNamesChoices(t *testing.T) {
	Register(fakeWF{name: "test-known-wf"})
	t.Cleanup(func() { Unregister("test-known-wf") })
	_, err := Get("nope")
	if err == nil || !strings.Contains(err.Error(), "test-known-wf") {
		t.Fatalf("error = %v", err)
	}
}

func TestAccepts(t *testing.T) {
	w := fakeWF{
		name: "test-accepts",
		triggers: []Trigger{
			{Kind: IssueAssigned},
			{Kind: IssueLabel},                // empty label = profile force label
			{Kind: IssueLabel, Label: "docs"}, // explicit label
		},
	}
	cases := []struct {
		name string
		ev   Event
		want bool
	}{
		{"assigned", Event{Kind: IssueAssigned, Number: 1}, true},
		{"force label", Event{Kind: IssueLabel, Labels: []string{"agent:go"}}, true},
		{"explicit label", Event{Kind: IssueLabel, Labels: []string{"docs"}}, true},
		{"other label", Event{Kind: IssueLabel, Labels: []string{"bug"}}, false},
		{"pr event", Event{Kind: PROpened, Number: 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Accepts(w, tc.ev, LabelDefaults{Issue: "agent:go", PR: "review:go"}); got != tc.want {
				t.Errorf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanOrdersReadOnlyFirstAndSkipsDisabled(t *testing.T) {
	impl := fakeWF{name: "test-impl", triggers: []Trigger{{Kind: IssueAssigned}}, perms: Perms{Write: true, Checks: true}}
	rev := fakeWF{name: "test-rev", triggers: []Trigger{{Kind: PROpened}}, perms: Perms{Comment: true}}
	Register(impl)
	Register(rev)
	t.Cleanup(func() {
		Unregister("test-impl")
		Unregister("test-rev")
	})

	events := []Event{
		{Kind: IssueAssigned, Number: 9},
		{Kind: PROpened, Number: 4},
	}
	got := Plan(events, func(w Workflow) bool { return true }, LabelDefaults{Issue: "agent:go", PR: "review:go"})
	var names []string
	for _, s := range got {
		names = append(names, s.Workflow.Name())
	}
	if len(names) < 2 || names[0] != "test-rev" {
		t.Fatalf("review must be scheduled first: %v", names)
	}

	disabled := Plan(events, func(w Workflow) bool { return w.Name() != "test-impl" }, LabelDefaults{Issue: "agent:go"})
	for _, s := range disabled {
		if s.Workflow.Name() == "test-impl" {
			t.Fatal("a disabled workflow was scheduled")
		}
	}
}

func TestPlanDeduplicates(t *testing.T) {
	impl := fakeWF{name: "test-dedup", triggers: []Trigger{{Kind: IssueAssigned}, {Kind: IssueLabel}}, perms: Perms{Write: true}}
	Register(impl)
	t.Cleanup(func() { Unregister("test-dedup") })

	events := []Event{
		{Kind: IssueAssigned, Number: 5},
		{Kind: IssueLabel, Number: 5, Labels: []string{"agent:go"}},
	}
	got := Plan(events, nil, LabelDefaults{Issue: "agent:go"})
	count := 0
	for _, s := range got {
		if s.Workflow.Name() == "test-dedup" {
			count++
		}
	}
	// Two different trigger kinds for the same issue are two events; the engine
	// dedupes per (workflow, number) before dispatching, so Plan keeps both and
	// the caller takes the first.
	if count != 2 {
		t.Errorf("distinct trigger kinds should both be planned, got %d", count)
	}
}

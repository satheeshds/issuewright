package brain

import (
	"context"
	"io"
	"strings"
	"testing"
)

type fakeDriver struct {
	name string
	caps Caps
}

func (f fakeDriver) Name() string { return f.name }
func (f fakeDriver) Caps() Caps   { return f.caps }
func (f fakeDriver) Run(context.Context, Spec, io.Writer) (Result, error) {
	return Result{Driver: f.name}, nil
}

func TestRegisterAndGet(t *testing.T) {
	Register(fakeDriver{name: "test-a", caps: Caps{Resume: true}})
	t.Cleanup(func() { Unregister("test-a") })

	d, err := Get("test-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.Name() != "test-a" || !d.Caps().Resume {
		t.Errorf("driver = %+v", d)
	}
	if got := Names(); len(got) == 0 || got[0] != "test-a" {
		t.Errorf("Names = %v", got)
	}
}

func TestDuplicateRegisterPanics(t *testing.T) {
	Register(fakeDriver{name: "test-dup"})
	t.Cleanup(func() { Unregister("test-dup") })
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	Register(fakeDriver{name: "test-dup"})
}

func TestUnknownDriverListsKnown(t *testing.T) {
	Register(fakeDriver{name: "test-known"})
	t.Cleanup(func() { Unregister("test-known") })

	_, err := Get("test-nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "test-nope") || !strings.Contains(msg, "test-known") {
		t.Errorf("error must name the mistake and the choices: %s", msg)
	}
	if !strings.Contains(msg, "known:") {
		t.Errorf("error should mention the known list: %s", msg)
	}
}

func TestValidThinking(t *testing.T) {
	for _, ok := range []string{"", "off", "high", "max"} {
		if !ValidThinking(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"HIGH", "extreme", "medium "} {
		if ValidThinking(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	// The plan's pi driver maps thinking levels; keep the list in sync with pi's
	// --help output.
	if len(ThinkingLevels) != 7 {
		t.Errorf("thinking levels = %v", ThinkingLevels)
	}
}

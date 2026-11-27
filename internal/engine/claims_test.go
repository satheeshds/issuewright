package engine

import (
	"reflect"
	"testing"
)

func TestClaimedCommits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"a plain claim", "The handler is wrapped now. Fixed in b9ba799.", []string{"b9ba799"}},
		{"parenthesised", "Fixed (see 0269415) by wrapping it", []string{"0269415"}},
		{"a full sha", "commit 02694153d0a41d01a3ffb14e2e8e4561a90f7c2c", []string{"02694153d0a41d01a3ffb14e2e8e4561a90f7c2c"}},
		{"deduplicated", "b9ba799 fixed it, and b9ba799 is the only change", []string{"b9ba799"}},
		{"a url is a citation", "https://github.com/acme/demo/commit/0269415", nil},
		{"another repo's commit", "bumped to acme/lib@0269415", nil},
		{"a digest", "image sha256:0269415", nil},
		{"a version", "v1.2.3", nil},
		{"a word", "the deadbeef handler", nil},
		{"too short", "in the abc123 file", nil},
		{"nothing", "no commit mentioned, the diff speaks", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := claimedCommits(c.text)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("claimedCommits(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

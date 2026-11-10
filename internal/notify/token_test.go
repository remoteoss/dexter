package notify

import "testing"

// TestProgressTokensDifferBetweenDaemons: each daemon numbers its tasks from
// one. An editor that a proxy moved to a new daemon may still show a token of
// the old one, so the new daemon must not use the same token.
func TestProgressTokensDifferBetweenDaemons(t *testing.T) {
	first, second := New(), New()
	first.SetLogf(func(string, ...any) {})
	second.SetLogf(func(string, ...any) {})
	a := first.Begin("index.build", "Indexing", "", false)
	b := second.Begin("index.build", "Indexing", "", false)
	if a.token == b.token {
		t.Fatalf("two daemons use the token %q", a.token)
	}
}

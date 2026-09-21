package beam

import "testing"

// EXPORT_EXT encodes its arity as SMALL_INTEGER_EXT, not as a raw byte, so a
// skip that consumes one byte leaves the reader misaligned on every term that
// follows. The sibling atom is only reachable when the arity was read as a term.
func TestSkipExportExtKeepsAlignment(t *testing.T) {
	tests := []struct {
		name  string
		arity int
	}{
		{"zero arity", 0},
		{"three arity", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w etfTestWriter
			w.smallTuple(2)
			w.atom("default")
			w.export("Elixir.DateTime", "utc_now", tt.arity)
			w.atom("after")

			r := &etfReader{buf: w.buf}
			arity, err := r.enterTuple()
			if err != nil || arity != 2 {
				t.Fatalf("enterTuple = %d, %v", arity, err)
			}
			if err := r.skip(); err != nil {
				t.Fatalf("skip key: %v", err)
			}
			if err := r.skip(); err != nil {
				t.Fatalf("skip export: %v", err)
			}
			got, err := r.readAtom()
			if err != nil {
				t.Fatalf("read after EXPORT_EXT: %v", err)
			}
			if got != "after" {
				t.Errorf("next term = %q, want %q", got, "after")
			}
			if r.remaining() != 0 {
				t.Errorf("%d bytes left over", r.remaining())
			}
		})
	}
}

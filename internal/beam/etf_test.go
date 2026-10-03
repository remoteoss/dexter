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

// A term that ends exactly at the end of the input is stepped over whole, and
// every strict prefix of it is rejected: the count of terms still to step over
// is compared with the bytes after each header, not with the header as well.
func TestSkipTermsThatEndTheInput(t *testing.T) {
	cases := map[string]func(w *etfTestWriter){
		"tuple of an empty list": func(w *etfTestWriter) {
			w.smallTuple(1)
			w.nil()
		},
		"empty map": func(w *etfTestWriter) {
			w.mapHeader(0)
		},
		"list of empty lists": func(w *etfTestWriter) {
			w.listHeader(2)
			w.nil()
			w.nil()
			w.nil()
		},
		"map of empty lists": func(w *etfTestWriter) {
			w.mapHeader(1)
			w.nil()
			w.nil()
		},
		"nested tuples": func(w *etfTestWriter) {
			w.smallTuple(2)
			w.smallTuple(1)
			w.nil()
			w.listHeader(1)
			w.smallTuple(0)
			w.nil()
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			var w etfTestWriter
			build(&w)
			r := &etfReader{buf: w.buf}
			if err := r.skip(); err != nil || r.remaining() != 0 {
				t.Fatalf("skip = %v with %d bytes left, want the whole term", err, r.remaining())
			}
			for i := range len(w.buf) {
				r := &etfReader{buf: w.buf[:i]}
				if err := r.skip(); err == nil {
					t.Errorf("skip accepted a truncation at %d of %d bytes", i, len(w.buf))
				}
			}
		})
	}
}

package treesitter

import (
	"reflect"
	"testing"
)

const dslSource = `defmodule M do
  use Ash.Resource

  code_interface do
    define :create, args: [:email]
  end

  attributes do
    attribute :email, :string
  end
end
`

// Lines of dslSource, for keeping the table below honest:
//
//	0 defmodule M do        4     define :create, ...   8     attribute :email, ...
//	1   use Ash.Resource    5   end                     9   end
//	2                       6                          10 end
//	3   code_interface do   7   attributes do
func TestEnclosingBlockPath(t *testing.T) {
	tests := []struct {
		name     string
		line     uint
		col      uint
		want     []string
		comments string
	}{
		{"module level statement", 1, 6, []string{"defmodule"}, "inside use Ash.Resource"},
		{"inside a section body", 4, 4, []string{"defmodule", "code_interface"}, "on the define call"},
		{
			"cursor just past the typed name", 4, 10,
			[]string{"defmodule", "code_interface"},
			"where a completion cursor actually sits",
		},
		{
			"on the section call itself", 3, 4,
			[]string{"defmodule"},
			"completing code_interface must not report itself as enclosing",
		},
		{
			"after the section end", 5, 4,
			[]string{"defmodule"},
			"the closing end is outside the block for scoping purposes",
		},
		{"inside a sibling section", 8, 4, []string{"defmodule", "attributes"}, ""},
		{"on the module end", 10, 0, nil, "outside everything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EnclosingBlockPath([]byte(dslSource), tt.line, tt.col)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EnclosingBlockPath(%d, %d) = %v, want %v (%s)",
					tt.line, tt.col, got, tt.want, tt.comments)
			}
		})
	}
}

func TestEnclosingBlockPathNested(t *testing.T) {
	src := `defmodule M do
  code_interface do
    define :create do
      custom_input :foo
    end
  end
end
`
	// 0 defmodule, 1 code_interface, 2 define, 3 custom_input, 4/5/6 ends
	want := []string{"defmodule", "code_interface", "define"}
	if got := EnclosingBlockPath([]byte(src), 3, 6); !reflect.DeepEqual(got, want) {
		t.Errorf("nested path = %v, want %v", got, want)
	}
	// Inside define's own body but not yet in custom_input's.
	if got := EnclosingBlockPath([]byte(src), 3, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("nested path at line start = %v, want %v", got, want)
	}
}

// Language forms are reported too; deciding which suffix means something is the
// caller's job, so a skip list here would have to track the grammar.
func TestEnclosingBlockPathIncludesLanguageForms(t *testing.T) {
	src := `defmodule M do
  def create(attrs) do
    if valid?(attrs) do
      :ok
    end
  end
end
`
	want := []string{"defmodule", "def", "if"}
	if got := EnclosingBlockPath([]byte(src), 3, 6); !reflect.DeepEqual(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

// A qualified call names its own target, so there is nothing to infer from it.
func TestEnclosingBlockPathSkipsQualifiedCalls(t *testing.T) {
	src := `defmodule M do
  Foo.bar do
    thing
  end
end
`
	want := []string{"defmodule"}
	if got := EnclosingBlockPath([]byte(src), 2, 4); !reflect.DeepEqual(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

// A completion cursor lives in code that does not parse yet. Treesitter is
// error-tolerant as long as the enclosing blocks are closed, which they are in
// every realistic edit: the developer is typing inside an existing body.
func TestEnclosingBlockPathPartialCode(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"token half typed", "defmodule M do\n  code_interface do\n    defin\n  end\nend\n"},
		{"blank line in body", "defmodule M do\n  code_interface do\n    \n  end\nend\n"},
		{"outer end missing", "defmodule M do\n  code_interface do\n    defin\n  end\n"},
	}
	want := []string{"defmodule", "code_interface"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, col := range []uint{4, 9} {
				if got := EnclosingBlockPath([]byte(tt.src), 2, col); !reflect.DeepEqual(got, want) {
					t.Errorf("col %d: path = %v, want %v", col, got, want)
				}
			}
		})
	}
}

// With every enclosing block left open, treesitter collapses the tail into a
// single ERROR node of bare tokens — there are no call or do_block nodes left to
// walk. That is accepted: such a file does not compile, so it has no BEAM and
// there is nothing generated to offer inside it anyway.
func TestEnclosingBlockPathFullyUnterminated(t *testing.T) {
	src := "defmodule M do\n  code_interface do\n    defin"
	if got := EnclosingBlockPath([]byte(src), 2, 9); got != nil {
		t.Errorf("expected no path from an all-ERROR tree, got %v", got)
	}
}

func TestEnclosingBlockPathEmpty(t *testing.T) {
	if got := EnclosingBlockPath(nil, 0, 0); got != nil {
		t.Errorf("empty source = %v, want nil", got)
	}
	if got := EnclosingBlockPath([]byte(dslSource), 999, 0); got != nil {
		t.Errorf("out of range = %v, want nil", got)
	}
}

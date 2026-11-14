package lsp

import (
	"slices"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

// A rename does not change an atom, so it lists the apply/3 calls that name
// the function with one: through the module, an alias, __MODULE__ in the
// module, or a variable. Other atoms and other modules are not listed.
func TestApplyCalls(t *testing.T) {
	src := []byte(`defmodule MyApp.Accounts do
  def get_user(id), do: id
  def a, do: apply(__MODULE__, :get_user, [1])
end

defmodule MyApp.Admin do
  alias MyApp.Accounts
  def b(mod), do: {apply(MyApp.Accounts, :get_user, [2]), Kernel.apply(Accounts, :get_user, [3])}
  def c(mod), do: apply(mod, :get_user, [4])
  def d, do: apply(__MODULE__, :get_user, [5])
  def e, do: {apply(MyApp.Other, :get_user, [6]), apply(Accounts, :other, [7]), :erlang.apply(Accounts, :get_user, [8])}
end
`)
	res := parser.TokenizeFull(src)
	p := &parsedSource{text: string(src), src: src, tokens: res.Tokens, interp: res.Interp, lineStarts: res.LineStarts}
	got := applyCalls(p, "get_user", []string{"MyApp.Accounts"})
	if want := []int{3, 8, 8, 9, 11}; !slices.Equal(got, want) {
		t.Errorf("applyCalls = %v, want %v", got, want)
	}
}

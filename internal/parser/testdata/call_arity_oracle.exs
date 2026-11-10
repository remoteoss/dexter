# Prints the arity of every named call in the Elixir files named on stdin, as
# Elixir's own parser reads it, for TestCallArity_MatchesElixir:
#
#   path<TAB>line<TAB>column<TAB>name<TAB>arity
#
# A pipe gives its target one more argument, a do block is one keyword
# argument, and a capture `&name/2` has the arity after the slash. Calls in
# #{} interpolations are included; the Kernel.to_string/1 that interpolation
# adds is not. A file that does not parse is skipped.

defmodule CallArityOracle do
  @name ~r/\A[a-z_][a-zA-Z0-9_]*[?!]?\z/

  def run do
    IO.stream(:stdio, :line)
    |> Stream.map(&String.trim/1)
    |> Stream.reject(&(&1 == ""))
    |> Enum.each(&file/1)
  end

  defp file(path) do
    with {:ok, src} <- File.read(path),
         {:ok, ast} <- quoted(src, path) do
      ast
      |> walk([])
      |> Enum.reverse()
      |> Enum.each(fn {line, col, name, arity} ->
        IO.puts("#{path}\t#{line}\t#{col}\t#{name}\t#{arity}")
      end)
    end
  end

  defp quoted(src, path) do
    Code.string_to_quoted(src, columns: true, file: path, emit_warnings: false)
  rescue
    _ -> :error
  end

  # A pipe: the target gets the left side as its first argument.
  defp walk({:|>, _, [left, right]}, acc), do: acc |> then(&walk(left, &1)) |> then(&call(right, 1, &1))

  # A capture of a named function with its arity.
  defp walk({:&, _, [{:/, _, [ref, n]}]}, acc) when is_integer(n) do
    case ref do
      {name, meta, ctx} when is_atom(name) and is_atom(ctx) -> record(acc, meta, name, n)
      {{:., _, [recv, name]}, meta, []} when is_atom(name) -> recv |> walk(record(acc, meta, name, n))
      other -> walk(other, acc)
    end
  end

  defp walk(node, acc), do: call(node, 0, acc)

  # A pipe into a bracketed pipe, `x |> (f() |> g())`: x goes to f.
  defp call({:|>, _, [left, right]}, extra, acc) when extra > 0 do
    acc |> then(&call(left, extra, &1)) |> then(&call(right, 1, &1))
  end

  # A remote call: Mod.name(args) or :mod.name(args).
  defp call({{:., _, [recv, name]}, meta, args}, extra, acc) when is_atom(name) and is_list(args) do
    acc = if meta[:from_interpolation], do: acc, else: record(acc, meta, name, length(args) + extra)
    acc = walk(recv, acc)
    Enum.reduce(args, acc, &walk/2)
  end

  # A local call, or in a pipe a name without parentheses (`x |> name`).
  defp call({name, meta, args}, extra, acc) when is_atom(name) and is_list(meta) and (is_list(args) or (is_atom(args) and extra > 0)) do
    acc = record(acc, meta, name, if(is_list(args), do: length(args), else: 0) + extra)
    if is_list(args), do: Enum.reduce(args, acc, &walk/2), else: acc
  end

  defp call({left, meta, args}, _extra, acc) when is_list(meta) and is_list(args) do
    Enum.reduce(args, walk(left, acc), &walk/2)
  end

  defp call({left, right}, _extra, acc), do: walk(right, walk(left, acc))
  defp call(list, _extra, acc) when is_list(list), do: Enum.reduce(list, acc, &walk/2)
  defp call(_other, _extra, acc), do: acc

  defp record(acc, meta, name, arity) do
    text = Atom.to_string(name)

    if meta[:line] && meta[:column] && Regex.match?(@name, text) && name not in [:__aliases__, :__block__] do
      [{meta[:line], meta[:column], text, arity} | acc]
    else
      acc
    end
  end
end

CallArityOracle.run()

# Call shapes whose arity Dexter must read as Elixir does. TestCallArity_
# MatchesElixir parses this file with Elixir and compares every named call.
# It is never compiled, so the calls need not exist.
defmodule ArityCases do
  import Kernel, except: [inspect: 1]

  def f(a), do: f(a, [])
  def f(a, b), do: {a, b}
  def g(a, b \\ 1, c \\ 2), do: {a, b, c}

  def pipes(x, conn) do
    x |> f()
    x |> f([])
    x |> f
    x |> Mod.Sub.h(1)
    x |> :binary.replace("a", "b", [:global])
    x |> conn.execute()
    x |> Mix.shell().info()
    x |> (List.flatten() |> Enum.map(&(&1 * 2)))
    x
    |> f()
    |> Mod.h(1)
  end

  def captures(list, conn) do
    Enum.map(list, &f/1)
    Enum.map(list, &Mod.h/2)
    Enum.map(list, &conn.handle/1)
    Enum.map(list, &f(&1, []))
    Enum.map(list, &Mod.h(&1, 1, 2))
  end

  def no_parens(a, b) do
    f a
    f a, b
    f a, b: 1, c: 2
    Mod.h a, b
    assert f(a) == b
    assert not f(a)
    assert !Mod.ok?(a)
    assert_received ^a
    assert @moduledoc == "x"
    IO.puts inspect a, label: "x"
    f (a)
    if (a or b) and f(b) do
      :ok
    end
  end

  def zero(map, m) do
    {map.key, Mod.value, m.a.b, Enum.fetch!(map, 1).name, map.key != nil}
  end

  def do_blocks(a, b) do
    setup do
      :ok
    end

    Mod.go do
      :ok
    end

    describe "x" do
      :ok
    end

    x = f(a) do
      :ok
    end

    if y = f(a) do
      y
    end

    case f(a) || g(b) do
      _ -> :ok
    end

    if a.valid? do
      :ok
    end

    if not Mod.ok?() do
      :ok
    end

    {x}
  end

  def heads_and_guards(a)
      when is_atom(a) and is_function(a, 1) do
    with {:ok, b} <-
           f(a),
         {:ok, c} <- g(b, 1) do
      c
    end
  end

  def inner_no_parens(a, b) do
    Repo.all(from p in Post, where: p.x == ^a, select: p)
    Repo.all from p in Post, select: p
    f(g a, b)
    Enum.reduce(
      for x <- a do
        x
      end,
      b,
      &f/2
    )
    write(
      defimpl Proto, for: List do
        def ok(_), do: true
      end
    )
  end

  def keywords(a) do
    f("a b": 1, +: 2, CONFIG: a)
    f(a, do: 1, else: 2)
    f(a, if: 1)
  end

  def strings(a) do
    "#{f(a)} and #{Mod.h(a, 1)}"
    "#{"#{f(a, a)}"}"
    "#{a |> Mod.h(1)}"
    ~s(text #{f(a)} more)
    ~S(raw \) text)
    f(~s(a (b), a)
    f(~r/a,b/, a)
  end

  def operators(a, b) do
    f(a && b, a)
    f(a != b, !a)
    f(a <<< 1, b)
    f(<<a, b>>, <<^a, b::binary>>)
    f(&<<&1, 2>>, a)
    f(a in [1, 2], a not in [3])
    f(-a, a - 1)
  end

  def multiline(a, b) do
    assert_raise Mod.Error,
                 "first " <>
                   "second",
                 fn -> f(a) end

    f(
      a,
      b
    )

    Mod.h(a,
      b, c: 1)
  end
end

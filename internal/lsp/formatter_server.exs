# Persistent formatter server for Dexter LSP.
#
# Loads .formatter.exs once and caches the formatter options, then loops over
# stdin formatting requests — no VM startup cost per format.
#
# Plugins (e.g. Styler) are loaded from the project's _build directory —
# no Mix.install or Hex downloads needed.
#
# Protocol (request):  2-byte filename length (big-endian) + filename +
#                      4-byte content length (big-endian) + content
# Protocol (response): 1-byte status (0=ok, 1=error) +
#                      4-byte result length (big-endian) + result
#
# Sends a ready response (status=0, length=0) once initialization is complete.
#
# Force raw byte mode on stdin/stdout — without this, the Erlang IO server
# applies Unicode encoding, expanding bytes > 127 to multi-byte UTF-8 and
# corrupting our binary protocol framing.
:io.setopts(:standard_io, encoding: :latin1)

[mix_root, formatter_exs_path, project_root_arg] = System.argv()

# In umbrella apps, _build and deps live at the umbrella root, not in
# individual app directories. Walk up from mix_root (bounded by the project
# root) to find the nearest ancestor that contains a _build directory.
expanded_mix_root = Path.expand(mix_root)
expanded_boundary = Path.expand(project_root_arg)

# If there are really umbrella apps with a distance greater than 20 to the root
# we can update this (or maybe make it configurable), but 20 seems like a sane
# limit.
project_root =
  Enum.reduce_while(1..20, expanded_mix_root, fn _, dir ->
    cond do
      File.dir?(Path.join(dir, "_build")) ->
        {:halt, dir}
      dir == expanded_boundary ->
        {:halt, expanded_mix_root}
      true ->
        parent = Path.dirname(dir)

        if parent == dir do
          {:halt, expanded_mix_root}
        else
          {:cont, parent}
        end
    end
  end)

# Add the project's compiled deps to the code path so plugins are available
# without needing Mix.install
project_root
|> Path.join("_build/dev/lib/*/ebin")
|> Path.wildcard()
|> Enum.each(&Code.prepend_path/1)

# Read .formatter.exs
raw_opts =
  if File.regular?(formatter_exs_path) do
    {result, _} = Code.eval_file(formatter_exs_path)
    if is_list(result), do: result, else: []
  else
    []
  end

plugins = Keyword.get(raw_opts, :plugins, [])

# Resolve locals_without_parens from import_deps by reading each dep's exported
# formatter config. Mix does this automatically in mix format, but we must
# replicate it here since we eval .formatter.exs directly.
import_deps_locals =
  raw_opts
  |> Keyword.get(:import_deps, [])
  |> Enum.flat_map(fn dep ->
    dep_formatter = Path.join([project_root, "deps", to_string(dep), ".formatter.exs"])

    if File.regular?(dep_formatter) do
      {dep_opts, _} = Code.eval_file(dep_formatter)

      if is_list(dep_opts) do
        dep_opts
        |> Keyword.get(:export, [])
        |> Keyword.get(:locals_without_parens, [])
      else
        []
      end
    else
      []
    end
  end)

explicit_locals = Keyword.get(raw_opts, :locals_without_parens, [])
all_locals_without_parens = Enum.uniq(import_deps_locals ++ explicit_locals)

# Extract formatting options
format_opts =
  raw_opts
  |> Keyword.take([
    :line_length,
    :normalize_bitstring_modifiers,
    :normalize_charlists_as_sigils,
    :force_do_end_blocks
  ])
  |> Keyword.put(:locals_without_parens, all_locals_without_parens)

# Resolve which plugins are actually loaded
active_plugins = Enum.filter(plugins, &Code.ensure_loaded?/1)

missing_plugins = plugins -- active_plugins

if missing_plugins != [] do
  IO.puts(:stderr, "Formatter: WARNING: could not load plugins: #{Enum.map_join(missing_plugins, ", ", &inspect/1)} (not compiled in _build?). Falling back to standard formatter.")
end

if active_plugins != [] do
  IO.puts(:stderr, "Formatter: plugins loaded: #{Enum.map_join(active_plugins, ", ", &inspect/1)}")
else
  IO.puts(:stderr, "Formatter: no plugins")
end

defmodule Formatter.Loop do
  def run(format_opts, plugins, first_call?) do
    # Signal ready if first call: status=0, length=0
    if first_call?, do: IO.binwrite(:stdio, <<0, 0, 0, 0, 0>>)

    case IO.binread(:stdio, 2) do
      <<filename_len::unsigned-big-16>> ->
        filename = if filename_len > 0, do: IO.binread(:stdio, filename_len), else: ""
        <<content_len::unsigned-big-32>> = IO.binread(:stdio, 4)
        content = IO.binread(:stdio, content_len)

        {status, result} = format(content, filename, format_opts, plugins)
        size = byte_size(result)
        IO.binwrite(:stdio, <<status::8, size::unsigned-big-32, result::binary>>)
        run(format_opts, plugins, false)

      _ ->
        :ok
    end
  end

  defp format(content, filename, format_opts, plugins) when is_binary(content) do
    try do
      opts = if filename != "", do: [file: filename] ++ format_opts, else: format_opts
      ext = Path.extname(filename)

      # Filter plugins to those that handle this file extension
      applicable_plugins =
        Enum.filter(plugins, fn plugin ->
          features = plugin.features(format_opts)
          extensions = Keyword.get(features, :extensions, [])
          # If a plugin declares no extensions, it handles .ex/.exs by default
          extensions == [] or ext in extensions
        end)

      formatted =
        if applicable_plugins != [] do
          # Redirect group leader to stderr during plugin calls so any
          # IO.puts from plugins doesn't corrupt the binary protocol on stdout.
          old_gl = Process.group_leader()
          Process.group_leader(self(), Process.whereis(:standard_error))

          try do
            Enum.reduce(applicable_plugins, content, fn plugin, acc ->
              plugin.format(acc, opts)
            end)
          after
            Process.group_leader(self(), old_gl)
          end
        else
          content |> Code.format_string!(opts) |> IO.iodata_to_binary()
        end

      # Ensure trailing newline to match mix format output
      formatted =
        if String.ends_with?(formatted, "\n"),
          do: formatted,
          else: formatted <> "\n"

      {0, formatted}
    rescue
      e -> {1, Exception.message(e)}
    catch
      kind, reason -> {1, "#{kind}: #{inspect(reason)}"}
    end
  end

  defp format(_, _, _, _), do: {1, "invalid input"}
end

try do
  Formatter.Loop.run(format_opts, active_plugins, true)
rescue
  e ->
    IO.puts(:stderr, "Formatter: crash in loop: #{Exception.message(e)}")
    IO.puts(:stderr, Exception.format_banner(:error, e, __STACKTRACE__))
catch
  kind, reason ->
    IO.puts(:stderr, "Formatter: crash in loop: #{inspect(kind)} #{inspect(reason)}")
end

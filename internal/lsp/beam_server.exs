# Persistent BEAM server for Dexter LSP.
#
# Single process that hosts:
#   - Multiple formatter instances (one per .formatter.exs, started on demand)
#   - A singleton CodeIntel service for Erlang source/docs lookups
#
# Communication is via stdin/stdout with binary framing:
#
# Request envelope:  1-byte service tag + service-specific payload
# Response envelope: service-specific (see below)
#
# Service tags:
#   0x00 = Formatter
#   0x01 = CodeIntel
#
# Formatter protocol (after service tag):
#   Request:  2-byte formatter_exs path length (big-endian) + formatter_exs path +
#             2-byte filename length (big-endian) + filename +
#             4-byte content length (big-endian) + content
#   Response: 1-byte status (0=ok, 1=error) +
#             4-byte result length (big-endian) + result
#
# CodeIntel protocol (after service tag):
#   Request:  1-byte op +
#             2-byte module length (big-endian) + module +
#             2-byte function length (big-endian) + function +
#             1-byte arity (255 = unspecified)
#
#   Op 0 (erlang_source) response:
#             1-byte status (0=ok, 1=not_found) +
#             2-byte file length (big-endian) + file +
#             4-byte line (big-endian, 0 if not found)
#
#   Op 1 (erlang_docs) response:
#             1-byte status (0=ok, 1=not_found) +
#             4-byte doc length (big-endian) + doc (markdown string)
#
# Sends a ready signal once initialization is complete:
#   1-byte status (0=ok) + 4-byte length (0)
#
# Force raw byte mode on stdin/stdout — without this, the Erlang IO server
# applies Unicode encoding, expanding bytes > 127 to multi-byte UTF-8 and
# corrupting our binary protocol framing.
:io.setopts(:standard_io, encoding: :latin1)

[project_root_arg] = System.argv()

# In umbrella apps, _build and deps live at the umbrella root, not in
# individual app directories. Walk up from project_root (bounded to 20 levels)
# to find the nearest ancestor that contains a _build directory.
expanded_project_root = Path.expand(project_root_arg)

build_root =
  Enum.reduce_while(1..20, expanded_project_root, fn _, dir ->
    cond do
      File.dir?(Path.join(dir, "_build")) ->
        {:halt, dir}

      true ->
        parent = Path.dirname(dir)

        if parent == dir do
          {:halt, expanded_project_root}
        else
          {:cont, parent}
        end
    end
  end)

# Add the project's compiled deps to the code path so plugins are available
# without needing Mix.install
build_root
|> Path.join("_build/dev/lib/*/ebin")
|> Path.wildcard()
|> Enum.each(&Code.prepend_path/1)

# Formatter Service

defmodule Dexter.Formatter do
  use GenServer

  def start_link(formatter_exs_path) do
    GenServer.start_link(__MODULE__, formatter_exs_path, name: via(formatter_exs_path))
  end

  def format(formatter_exs_path, content, filename) do
    GenServer.call(via(formatter_exs_path), {:format, content, filename}, :infinity)
  end

  defp via(formatter_exs_path) do
    {:via, Registry, {Dexter.FormatterRegistry, formatter_exs_path}}
  end

  @impl true
  def init(formatter_exs_path) do
    {format_opts, active_plugins} = load_formatter_config(formatter_exs_path)
    {:ok, %{format_opts: format_opts, plugins: active_plugins, path: formatter_exs_path}}
  end

  @impl true
  def handle_call({:format, content, filename}, _from, state) do
    result = do_format(content, filename, state.format_opts, state.plugins)
    {:reply, result, state}
  end

  defp load_formatter_config(formatter_exs_path) do
    # Find the build root by looking for _build from the formatter's directory
    formatter_dir = Path.dirname(formatter_exs_path)

    project_root =
      Enum.reduce_while(1..20, formatter_dir, fn _, dir ->
        cond do
          File.dir?(Path.join(dir, "_build")) -> {:halt, dir}
          true ->
            parent = Path.dirname(dir)
            if parent == dir, do: {:halt, formatter_dir}, else: {:cont, parent}
        end
      end)

    # Ensure compiled deps are on the code path so plugins can be loaded
    project_root
    |> Path.join("_build/dev/lib/*/ebin")
    |> Path.wildcard()
    |> Enum.each(&Code.prepend_path/1)

    raw_opts =
      if File.regular?(formatter_exs_path) do
        {result, _} = Code.eval_file(formatter_exs_path)
        if is_list(result), do: result, else: []
      else
        []
      end

    plugins = Keyword.get(raw_opts, :plugins, [])

    # Resolve locals_without_parens from import_deps
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

    format_opts =
      raw_opts
      |> Keyword.take([
        :line_length,
        :normalize_bitstring_modifiers,
        :normalize_charlists_as_sigils,
        :force_do_end_blocks
      ])
      |> Keyword.put(:locals_without_parens, all_locals_without_parens)

    active_plugins = Enum.filter(plugins, &Code.ensure_loaded?/1)

    missing_plugins = plugins -- active_plugins

    if missing_plugins != [] do
      IO.puts(:stderr, "Formatter: WARNING: could not load plugins: #{Enum.map_join(missing_plugins, ", ", &inspect/1)} (not compiled in _build?). Falling back to standard formatter.")
    end

    if active_plugins != [] do
      IO.puts(:stderr, "Formatter: plugins loaded for #{formatter_exs_path}: #{Enum.map_join(active_plugins, ", ", &inspect/1)}")
    end

    {format_opts, active_plugins}
  end

  defp do_format(content, filename, format_opts, plugins) when is_binary(content) do
    try do
      opts = if filename != "", do: [file: filename] ++ format_opts, else: format_opts
      ext = Path.extname(filename)

      # Filter plugins to those that handle this file extension
      applicable_plugins =
        Enum.filter(plugins, fn plugin ->
          features = plugin.features(format_opts)
          extensions = Keyword.get(features, :extensions, [])
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

  defp do_format(_, _, _, _), do: {1, "invalid input"}
end

# CodeIntel Service

defmodule Dexter.CodeIntel do
  @op_erlang_source 0
  @op_erlang_docs 1

  def handle_request() do
    case IO.binread(:stdio, 1) do
      <<@op_erlang_source>> ->
        handle_erlang_source()

      <<@op_erlang_docs>> ->
        handle_erlang_docs()

      _ ->
        :eof
    end
  end

  defp handle_erlang_source do
    <<module_len::unsigned-big-16>> = IO.binread(:stdio, 2)
    module_name = if module_len > 0, do: IO.binread(:stdio, module_len), else: ""
    <<function_len::unsigned-big-16>> = IO.binread(:stdio, 2)
    function_name = if function_len > 0, do: IO.binread(:stdio, function_len), else: ""
    <<arity::unsigned-8>> = IO.binread(:stdio, 1)

    {status, file, line} = resolve_erlang_source(module_name, function_name, arity)

    file_bytes = if file, do: file, else: ""
    file_len = byte_size(file_bytes)
    IO.binwrite(:stdio, <<status::8, file_len::unsigned-big-16, file_bytes::binary, line::unsigned-big-32>>)
    :ok
  end

  defp resolve_erlang_source(module_name, function_name, arity) do
    module_atom = String.to_atom(module_name)

    case find_source_file(module_atom) do
      nil ->
        {1, nil, 0}

      source_file ->
        line = find_function_line(module_atom, function_name, arity)
        {0, source_file, line}
    end
  end

  defp find_source_file(module) do
    case :code.get_object_code(module) do
      {_module, _binary, beam_path} ->
        erl_file =
          beam_path
          |> to_string()
          |> String.replace(~r|(.+)/ebin/([^\s]+)\.beam$|, "\\1/src/\\2.erl")

        if File.exists?(erl_file, [:raw]), do: erl_file, else: nil

      :error ->
        nil
    end
  end

  defp find_function_line(_module_atom, "", _arity), do: 0

  defp find_function_line(module_atom, function_name, arity) do
    function_atom = String.to_atom(function_name)

    # Try abstract code first for arity-accurate line numbers
    line = find_line_from_abstract_code(module_atom, function_atom, arity)

    if line > 0 do
      line
    else
      # Fallback: regex search in source file
      find_line_from_source(module_atom, function_atom)
    end
  end

  defp find_line_from_abstract_code(module_atom, function_atom, arity) do
    beam_path = :code.which(module_atom)

    if is_list(beam_path) do
      case :beam_lib.chunks(beam_path, [:abstract_code]) do
        {:ok, {_, [{:abstract_code, {:raw_abstract_v1, forms}}]}} ->
          find_function_in_forms(forms, function_atom, arity)

        _ ->
          0
      end
    else
      0
    end
  end

  defp find_function_in_forms(forms, function_atom, 255 = _unspecified) do
    Enum.find_value(forms, 0, fn
      {:function, anno, ^function_atom, _arity, _clauses} ->
        anno_line(anno)

      _ ->
        nil
    end)
  end

  defp find_function_in_forms(forms, function_atom, arity) do
    exact =
      Enum.find_value(forms, nil, fn
        {:function, anno, ^function_atom, ^arity, _clauses} ->
          anno_line(anno)

        _ ->
          nil
      end)

    exact || find_function_in_forms(forms, function_atom, 255)
  end

  defp anno_line(anno) when is_integer(anno), do: anno
  defp anno_line(anno) when is_list(anno), do: Keyword.get(anno, :line, 0)
  defp anno_line(anno) when is_map(anno), do: Map.get(anno, :line, 0)
  defp anno_line(_), do: 0

  defp find_line_from_source(module_atom, function_atom) do
    case find_source_file(module_atom) do
      nil ->
        0

      source_file ->
        pattern = ~r/^#{Regex.escape(to_string(function_atom))}\b\(/u

        source_file
        |> File.stream!()
        |> Stream.with_index(1)
        |> Enum.find_value(0, fn {line, line_number} ->
          if Regex.match?(pattern, line), do: line_number, else: nil
        end)
    end
  end

  # Erlang Docs

  defp handle_erlang_docs do
    <<module_len::unsigned-big-16>> = IO.binread(:stdio, 2)
    module_name = if module_len > 0, do: IO.binread(:stdio, module_len), else: ""
    <<function_len::unsigned-big-16>> = IO.binread(:stdio, 2)
    function_name = if function_len > 0, do: IO.binread(:stdio, function_len), else: ""
    <<arity::unsigned-8>> = IO.binread(:stdio, 1)

    {status, doc} = fetch_erlang_docs(module_name, function_name, arity)

    doc_bytes = doc || ""
    doc_len = byte_size(doc_bytes)
    IO.binwrite(:stdio, <<status::8, doc_len::unsigned-big-32, doc_bytes::binary>>)
    :ok
  end

  defp fetch_erlang_docs(module_name, function_name, arity) do
    module_atom = String.to_atom(module_name)

    case Code.fetch_docs(module_atom) do
      {:docs_v1, _, :erlang, _format, module_doc, _metadata, docs} ->
        if function_name == "" do
          case extract_doc_text(module_doc) do
            nil -> {1, nil}
            text -> {0, text}
          end
        else
          function_atom = String.to_atom(function_name)
          find_function_doc(docs, function_atom, arity)
        end

      _ ->
        {1, nil}
    end
  end

  defp find_function_doc(docs, function_atom, arity) do
    candidates =
      Enum.filter(docs, fn
        {{:function, ^function_atom, _arity}, _anno, _sig, _doc, _meta} -> true
        _ -> false
      end)

    match =
      if arity != 255 do
        Enum.find(candidates, fn
          {{:function, _, ^arity}, _, _, _, _} -> true
          _ -> false
        end) || List.first(candidates)
      else
        List.first(candidates)
      end

    case match do
      {{:function, _, match_arity}, _anno, signatures, doc, _meta} ->
        signature = format_signatures(signatures, function_atom, match_arity)
        doc_text = extract_doc_text(doc)

        parts = []
        parts = if signature != "", do: parts ++ ["```erlang\n#{signature}\n```"], else: parts
        parts = if doc_text, do: parts ++ [doc_text], else: parts

        case parts do
          [] -> {1, nil}
          _ -> {0, Enum.join(parts, "\n\n")}
        end

      nil ->
        {1, nil}
    end
  end

  defp format_signatures(signatures, function_atom, arity) when is_list(signatures) do
    case signatures do
      [sig | _] when is_binary(sig) -> sig
      _ -> "#{function_atom}/#{arity}"
    end
  end

  defp format_signatures(_, function_atom, arity), do: "#{function_atom}/#{arity}"

  defp extract_doc_text(%{"en" => text}), do: text
  defp extract_doc_text(:hidden), do: nil
  defp extract_doc_text(:none), do: nil
  defp extract_doc_text(_), do: nil
end

# Main IO Loop

defmodule Dexter.Loop do
  @service_formatter 0
  @service_code_intel 1

  def run(first_call?) do
    if first_call?, do: IO.binwrite(:stdio, <<0, 0, 0, 0, 0>>)

    case IO.binread(:stdio, 1) do
      <<@service_formatter>> ->
        case handle_format_request() do
          :ok -> run(false)
          :eof -> :ok
        end

      <<@service_code_intel>> ->
        case Dexter.CodeIntel.handle_request() do
          :ok -> run(false)
          :eof -> :ok
        end

      _ ->
        :ok
    end
  end

  defp handle_format_request do
    case IO.binread(:stdio, 2) do
      <<config_path_len::unsigned-big-16>> ->
        config_path = if config_path_len > 0, do: IO.binread(:stdio, config_path_len), else: ""
        <<filename_len::unsigned-big-16>> = IO.binread(:stdio, 2)
        filename = if filename_len > 0, do: IO.binread(:stdio, filename_len), else: ""
        <<content_len::unsigned-big-32>> = IO.binread(:stdio, 4)
        content = IO.binread(:stdio, content_len)

        # Start a formatter for this config if we haven't seen it before
        ensure_formatter(config_path)

        {status, result} = Dexter.Formatter.format(config_path, content, filename)
        size = byte_size(result)
        IO.binwrite(:stdio, <<status::8, size::unsigned-big-32, result::binary>>)
        :ok

      _ ->
        :eof
    end
  end

  defp ensure_formatter(config_path) do
    case Registry.lookup(Dexter.FormatterRegistry, config_path) do
      [{_pid, _}] ->
        :ok

      [] ->
        DynamicSupervisor.start_child(
          Dexter.FormatterSup,
          {Dexter.Formatter, config_path}
        )

        :ok
    end
  end
end

# Boot

{:ok, _} = Registry.start_link(keys: :unique, name: Dexter.FormatterRegistry)
{:ok, _} = DynamicSupervisor.start_link(strategy: :one_for_one, name: Dexter.FormatterSup)

IO.puts(:stderr, "Dexter BEAM: started (pid #{System.pid()})")

try do
  Dexter.Loop.run(true)
rescue
  e ->
    IO.puts(:stderr, "Dexter BEAM: crash in loop: #{Exception.message(e)}")
    IO.puts(:stderr, Exception.format_banner(:error, e, __STACKTRACE__))
catch
  kind, reason ->
    IO.puts(:stderr, "Dexter BEAM: crash in loop: #{inspect(kind)} #{inspect(reason)}")
end

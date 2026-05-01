# Persistent BEAM server for Dexter LSP.
#
# Single process that hosts:
#   - Multiple formatter instances (one per .formatter.exs, started on demand)
#   - A singleton CodeIntel service for Erlang source/docs lookups
#
# Communication is via stdin/stdout with framed binary messages:
#
# Frame 0x00 = request:
#   request_id(u32) + service(u8) + op(u8) + payload_len(u32) + payload
#
# Frame 0x01 = response:
#   request_id(u32) + status(u8) + payload_len(u32) + payload
#
# Frame 0x02 = notification:
#   op(u8) + payload_len(u32) + payload
#
# Frame 0x03 = ready:
#   status(u8) + payload_len(u32) + payload
#
# Service tags:
#   0x00 = Formatter
#   0x01 = CodeIntel
#
# Formatter op 0 (format) payload:
#   2-byte formatter_exs path length (big-endian) + formatter_exs path +
#   2-byte filename length (big-endian) + filename +
#   4-byte content length (big-endian) + content
#
# CodeIntel op 0 (erlang_source) payload:
#   2-byte module length (big-endian) + module +
#   2-byte function length (big-endian) + function +
#   1-byte arity (255 = unspecified)
#
# CodeIntel op 1 (erlang_docs) payload:
#   Same as erlang_source
#
# CodeIntel op 2 (warm_otp_modules) payload:
#   empty; results arrive asynchronously via notification 0
#
# CodeIntel op 3 (erlang_exports) payload:
#   2-byte module length (big-endian) + module
#
# CodeIntel op 4 (runtime_info) payload:
#   empty
#
# Notification 0 (otp_modules_ready) payload:
#   2-byte module_count (big-endian) + [name_len(u16) name]
#
# Notification 1 (otp_modules_failed) payload:
#   error string
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

# Protocol Writer

defmodule Dexter.Writer do
  use GenServer

  @frame_response 1
  @frame_notification 2
  @frame_ready 3

  @notif_otp_modules_ready 0
  @notif_otp_modules_failed 1

  def start_link() do
    GenServer.start_link(__MODULE__, :ok, name: __MODULE__)
  end

  def send_ready(status, payload \\ <<>>) when is_binary(payload) do
    GenServer.call(__MODULE__, {:write, ready_frame(status, payload)}, :infinity)
  end

  def send_response(req_id, status, payload) when is_binary(payload) do
    GenServer.cast(__MODULE__, {:write, response_frame(req_id, status, payload)})
  end

  def send_otp_modules_ready(names) do
    GenServer.cast(__MODULE__, {:write, notification_frame(@notif_otp_modules_ready, encode_module_names(names))})
  end

  def send_otp_modules_failed(message) do
    payload = if message, do: to_string(message), else: ""
    GenServer.cast(__MODULE__, {:write, notification_frame(@notif_otp_modules_failed, payload)})
  end

  @impl true
  def init(:ok), do: {:ok, nil}

  @impl true
  def handle_call({:write, frame}, _from, state) do
    write_frame(frame)
    {:reply, :ok, state}
  end

  @impl true
  def handle_cast({:write, frame}, state) do
    write_frame(frame)
    {:noreply, state}
  end

  defp write_frame(frame) do
    case IO.binwrite(:stdio, frame) do
      :ok -> :ok
      {:error, reason} -> exit({:write_failed, reason})
    end
  end

  defp response_frame(req_id, status, payload) do
    <<@frame_response::8, req_id::unsigned-big-32, status::8, byte_size(payload)::unsigned-big-32,
      payload::binary>>
  end

  defp notification_frame(op, payload) do
    <<@frame_notification::8, op::8, byte_size(payload)::unsigned-big-32, payload::binary>>
  end

  defp ready_frame(status, payload) do
    <<@frame_ready::8, status::8, byte_size(payload)::unsigned-big-32, payload::binary>>
  end

  defp encode_module_names(names) do
    payload =
      for name <- names, into: <<>> do
        <<byte_size(name)::unsigned-big-16, name::binary>>
      end

    <<length(names)::unsigned-big-16, payload::binary>>
  end
end

# CodeIntel Service

defmodule Dexter.CodeIntelCache do
  use GenServer

  def start_link() do
    GenServer.start_link(__MODULE__, %{}, name: __MODULE__)
  end

  def warm_otp_modules() do
    GenServer.call(__MODULE__, :warm_otp_modules)
  end

  @impl true
  def init(_state) do
    {:ok, %{otp_modules: nil, loading: false}}
  end

  @impl true
  def handle_call(:warm_otp_modules, _from, %{otp_modules: names} = state) when is_list(names) do
    Dexter.Writer.send_otp_modules_ready(names)
    {:reply, :ok, state}
  end

  def handle_call(:warm_otp_modules, _from, %{loading: true} = state) do
    {:reply, :ok, state}
  end

  def handle_call(:warm_otp_modules, _from, state) do
    {:ok, _pid} =
      Task.Supervisor.start_child(Dexter.TaskSup, fn ->
        result =
          try do
            {:ok, compute_otp_module_names()}
          rescue
            error -> {:error, {:error, error, __STACKTRACE__}}
          catch
            kind, reason -> {:error, {kind, reason}}
          end

        GenServer.cast(__MODULE__, {:otp_module_result, result})
      end)

    {:reply, :ok, %{state | loading: true}}
  end

  @impl true
  def handle_cast({:otp_module_result, {:ok, names}}, state) do
    Dexter.Writer.send_otp_modules_ready(names)
    {:noreply, %{state | otp_modules: names, loading: false}}
  end

  def handle_cast({:otp_module_result, {:error, reason}}, state) do
    IO.puts(:stderr, "CodeIntelCache: failed to load OTP modules: #{inspect(reason)}")
    Dexter.Writer.send_otp_modules_failed(inspect(reason))
    {:noreply, %{state | loading: false}}
  end

  defp compute_otp_module_names do
    otp_root = :code.lib_dir() |> to_string()

    :code.all_available()
    |> Enum.reduce([], fn {name, path, _loaded}, acc ->
      mod_name = to_string(name)

      if is_list(path) and String.starts_with?(to_string(path), otp_root) and
           not String.starts_with?(mod_name, "Elixir.") do
        [mod_name | acc]
      else
        acc
      end
    end)
    |> Enum.sort()
  end
end

defmodule Dexter.CodeIntel do
  @op_erlang_source 0
  @op_erlang_docs 1
  @op_warm_otp_modules 2
  @op_erlang_exports 3
  @op_runtime_info 4

  def handle_request(op, payload) do
    case op do
      @op_erlang_source -> handle_erlang_source(payload)
      @op_erlang_docs -> handle_erlang_docs(payload)
      @op_warm_otp_modules -> handle_warm_otp_modules(payload)
      @op_erlang_exports -> handle_erlang_exports(payload)
      @op_runtime_info -> handle_runtime_info(payload)
      _ -> {1, "unknown code intel op: #{inspect(op)}"}
    end
  end

  defp handle_erlang_source(payload) do
    case parse_module_function_arity(payload) do
      {:ok, module_name, function_name, arity} ->
        {status, file, line} = resolve_erlang_source(module_name, function_name, arity)
        file_bytes = if file, do: file, else: ""
        {status, <<byte_size(file_bytes)::unsigned-big-16, file_bytes::binary, line::unsigned-big-32>>}

      :error ->
        {1, "invalid erlang_source payload"}
    end
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

    line = find_line_from_abstract_code(module_atom, function_atom, arity)

    if line > 0 do
      line
    else
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

  defp handle_erlang_docs(payload) do
    case parse_module_function_arity(payload) do
      {:ok, module_name, function_name, arity} ->
        {status, doc} = fetch_erlang_docs(module_name, function_name, arity)
        doc_bytes = doc || ""
        {status, <<byte_size(doc_bytes)::unsigned-big-32, doc_bytes::binary>>}

      :error ->
        {1, "invalid erlang_docs payload"}
    end
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

  defp find_function_doc(docs, name_atom, arity) do
    case find_doc_entry(docs, :function, name_atom, arity) do
      nil -> find_doc_entry(docs, :type, name_atom, arity) || {1, nil}
      result -> result
    end
  end

  defp find_doc_entry(docs, kind, name_atom, arity) do
    candidates =
      Enum.filter(docs, fn
        {{^kind, ^name_atom, _arity}, _anno, _sig, _doc, _meta} -> true
        _ -> false
      end)

    match =
      if arity != 255 do
        Enum.find(candidates, fn
          {{_, _, ^arity}, _, _, _, _} -> true
          _ -> false
        end) || List.first(candidates)
      else
        List.first(candidates)
      end

    case match do
      {{_, _, match_arity}, _anno, signatures, doc, _meta} ->
        signature = format_signatures(signatures, name_atom, match_arity)
        doc_text = extract_doc_text(doc)

        parts = []
        parts = if signature != "", do: parts ++ ["```erlang\n#{signature}\n```"], else: parts
        parts = if doc_text, do: parts ++ [doc_text], else: parts

        case parts do
          [] -> nil
          _ -> {0, Enum.join(parts, "\n\n")}
        end

      nil ->
        nil
    end
  end

  defp handle_warm_otp_modules(_payload) do
    :ok = Dexter.CodeIntelCache.warm_otp_modules()
    {0, <<>>}
  end

  defp handle_erlang_exports(payload) do
    case parse_module(payload) do
      {:ok, module_name} ->
        mod_atom = String.to_atom(module_name)
        export_params = export_param_names(mod_atom)

        exports =
          case :code.ensure_loaded(mod_atom) do
            {:module, _} ->
              mod_atom.module_info(:exports)
              |> Enum.reject(fn {f, _} -> f in [:module_info, :behaviour_info] end)

            _ ->
              []
          end

        exports_payload =
          for {func, arity} <- exports, into: <<>> do
            func_str = to_string(func)
            params = Map.get(export_params, {func, arity}, "")

            <<byte_size(func_str)::unsigned-big-16, func_str::binary, arity::unsigned-8,
              byte_size(params)::unsigned-big-16, params::binary>>
          end

        {0, <<length(exports)::unsigned-big-16, exports_payload::binary>>}

      :error ->
        {1, "invalid erlang_exports payload"}
    end
  end

  defp export_param_names(module_atom) do
    case Code.fetch_docs(module_atom) do
      {:docs_v1, _, :erlang, _format, _module_doc, _metadata, docs} ->
        Enum.reduce(docs, %{}, fn
          {{:function, name, arity}, _anno, signatures, _doc, _meta}, acc ->
            case signature_params(signatures, arity) do
              "" -> acc
              params -> Map.put(acc, {name, arity}, params)
            end

          _other, acc ->
            acc
        end)

      _ ->
        %{}
    end
  end

  defp signature_params(signatures, arity) when is_list(signatures) do
    signatures
    |> Enum.find_value("", fn
      sig when is_binary(sig) ->
        case extract_signature_args(sig) do
          {:ok, args} ->
            params =
              args
              |> split_signature_args()
              |> Enum.with_index(1)
              |> Enum.map(fn {param, index} -> normalize_signature_param(param, index) end)

            if length(params) == arity, do: Enum.join(params, ","), else: nil

          :error ->
            nil
        end

      _ ->
        nil
    end)
  end

  defp signature_params(_, _arity), do: ""

  defp extract_signature_args(signature) do
    case :binary.match(signature, "(") do
      {start, 1} ->
        rest = binary_part(signature, start + 1, byte_size(signature) - start - 1)
        collect_signature_args(rest, 0, [])

      :nomatch ->
        :error
    end
  end

  defp collect_signature_args(<<>>, _depth, _acc), do: :error

  defp collect_signature_args(<<")", _rest::binary>>, 0, acc) do
    {:ok, acc |> Enum.reverse() |> IO.iodata_to_binary()}
  end

  defp collect_signature_args(<<"(", rest::binary>>, depth, acc),
    do: collect_signature_args(rest, depth + 1, ["(" | acc])

  defp collect_signature_args(<<")", rest::binary>>, depth, acc),
    do: collect_signature_args(rest, depth - 1, [")" | acc])

  defp collect_signature_args(<<char::utf8, rest::binary>>, depth, acc),
    do: collect_signature_args(rest, depth, [<<char::utf8>> | acc])

  defp split_signature_args(""), do: []

  defp split_signature_args(args) do
    {parts, current, _depths} =
      args
      |> String.to_charlist()
      |> Enum.reduce({[], [], {0, 0, 0}}, fn char, {parts, current, {paren, bracket, brace}} ->
        case char do
          ?, when paren == 0 and bracket == 0 and brace == 0 ->
            part = current |> Enum.reverse() |> to_string() |> String.trim()
            {[part | parts], [], {paren, bracket, brace}}

          ?( ->
            {parts, [char | current], {paren + 1, bracket, brace}}

          ?) ->
            {parts, [char | current], {paren - 1, bracket, brace}}

          ?[ ->
            {parts, [char | current], {paren, bracket + 1, brace}}

          ?] ->
            {parts, [char | current], {paren, bracket - 1, brace}}

          ?{ ->
            {parts, [char | current], {paren, bracket, brace + 1}}

          ?} ->
            {parts, [char | current], {paren, bracket, brace - 1}}

          _ ->
            {parts, [char | current], {paren, bracket, brace}}
        end
      end)

    last = current |> Enum.reverse() |> to_string() |> String.trim()

    (if last == "", do: parts, else: [last | parts])
    |> Enum.reverse()
  end

  defp normalize_signature_param(param, index) do
    name =
      Regex.scan(~r/[A-Za-z][A-Za-z0-9_]*/, param)
      |> List.flatten()
      |> Enum.map(&Macro.underscore/1)
      |> Enum.join("_")
      |> String.replace(~r/_+/, "_")
      |> String.trim("_")

    if name == "", do: "arg#{index}", else: name
  end

  defp handle_runtime_info(_payload) do
    otp_release = :erlang.system_info(:otp_release) |> to_string()
    code_root_dir = :code.root_dir() |> to_string()

    {0,
     <<byte_size(otp_release)::unsigned-big-16, otp_release::binary,
       byte_size(code_root_dir)::unsigned-big-16, code_root_dir::binary>>}
  end

  defp parse_module_function_arity(payload) do
    with <<module_len::unsigned-big-16, rest::binary>> <- payload,
         {:ok, module_name, rest} <- take_string(rest, module_len),
         <<function_len::unsigned-big-16, rest::binary>> <- rest,
         {:ok, function_name, rest} <- take_string(rest, function_len),
         <<arity::unsigned-8>> <- rest do
      {:ok, module_name, function_name, arity}
    else
      _ -> :error
    end
  end

  defp parse_module(payload) do
    with <<module_len::unsigned-big-16, rest::binary>> <- payload,
         {:ok, module_name, <<>>} <- take_string(rest, module_len) do
      {:ok, module_name}
    else
      _ -> :error
    end
  end

  defp take_string(binary, size) when byte_size(binary) >= size do
    <<value::binary-size(size), rest::binary>> = binary
    {:ok, value, rest}
  end

  defp take_string(_binary, _size), do: :error

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
  @frame_request 0
  @service_formatter 0
  @service_code_intel 1
  @formatter_op_format 0

  def run do
    case read_request_frame() do
      {:ok, req_id, service, op, payload} ->
        dispatch_request(req_id, service, op, payload)
        run()

      :eof ->
        :ok

      {:error, reason} ->
        :erlang.display({:beam_loop_read_error, reason})
        :ok
    end
  end

  defp dispatch_request(req_id, service, op, payload) do
    {:ok, _pid} =
      Task.Supervisor.start_child(Dexter.TaskSup, fn ->
        {status, response_payload} =
          try do
            case {service, op} do
              {@service_formatter, @formatter_op_format} ->
                handle_format_request(payload)

              {@service_code_intel, _} ->
                Dexter.CodeIntel.handle_request(op, payload)

              _ ->
                {1, "unknown request #{service}/#{op}"}
            end
          rescue
            error ->
              :erlang.display({:beam_request_crash, service, op, error, __STACKTRACE__})
              {1, Exception.message(error)}
          catch
            kind, reason ->
              :erlang.display({:beam_request_crash, service, op, kind, reason})
              {1, inspect({kind, reason})}
          end

        Dexter.Writer.send_response(req_id, status, response_payload)
      end)

    :ok
  end

  defp read_request_frame do
    with {:ok, @frame_request} <- read_byte(),
         {:ok, <<req_id::unsigned-big-32>>} <- read_exact(4),
         {:ok, <<service::8, op::8, payload_len::unsigned-big-32>>} <- read_exact(6),
         {:ok, payload} <- read_exact(payload_len) do
      {:ok, req_id, service, op, payload}
    else
      :eof -> :eof
      {:error, :eof} -> :eof
      {:ok, other} -> {:error, {:unexpected_frame, other}}
      {:error, reason} -> {:error, reason}
    end
  end

  defp read_byte do
    case IO.binread(:stdio, 1) do
      :eof -> :eof
      <<byte>> -> {:ok, byte}
      other -> {:error, {:bad_read, other}}
    end
  end

  defp read_exact(0), do: {:ok, <<>>}

  defp read_exact(size) when size > 0 do
    case IO.binread(:stdio, size) do
      :eof ->
        {:error, :eof}

      data when is_binary(data) and byte_size(data) == size ->
        {:ok, data}

      data when is_binary(data) ->
        {:error, {:short_read, size, byte_size(data)}}

      other ->
        {:error, {:bad_read, other}}
    end
  end

  defp handle_format_request(payload) do
    case parse_format_payload(payload) do
      {:ok, config_path, filename, content} ->
        with :ok <- ensure_formatter(config_path) do
          Dexter.Formatter.format(config_path, content, filename)
        else
          {:error, reason} -> {1, format_formatter_start_error(reason)}
        end

      :error ->
        {1, "invalid format payload"}
    end
  end

  defp parse_format_payload(payload) do
    with <<config_path_len::unsigned-big-16, rest::binary>> <- payload,
         {:ok, config_path, rest} <- take_string(rest, config_path_len),
         <<filename_len::unsigned-big-16, rest::binary>> <- rest,
         {:ok, filename, rest} <- take_string(rest, filename_len),
         <<content_len::unsigned-big-32, rest::binary>> <- rest,
         {:ok, content, <<>>} <- take_string(rest, content_len) do
      {:ok, config_path, filename, content}
    else
      _ -> :error
    end
  end

  defp take_string(binary, size) when byte_size(binary) >= size do
    <<value::binary-size(size), rest::binary>> = binary
    {:ok, value, rest}
  end

  defp take_string(_binary, _size), do: :error

  defp ensure_formatter(config_path) do
    case Registry.lookup(Dexter.FormatterRegistry, config_path) do
      [{_pid, _}] ->
        :ok

      [] ->
        case DynamicSupervisor.start_child(
               Dexter.FormatterSup,
               {Dexter.Formatter, config_path}
             ) do
          {:ok, _pid} -> :ok
          {:error, {:already_started, _pid}} -> :ok
          {:error, reason} -> {:error, reason}
        end
    end
  end

  defp format_formatter_start_error({%_{} = error, _stacktrace}) do
    Exception.message(error)
  end

  defp format_formatter_start_error(reason), do: inspect(reason)
end

# Boot

{:ok, _} = Registry.start_link(keys: :unique, name: Dexter.FormatterRegistry)
{:ok, _} = DynamicSupervisor.start_link(strategy: :one_for_one, name: Dexter.FormatterSup)
{:ok, _} = Task.Supervisor.start_link(name: Dexter.TaskSup)
{:ok, _} = Dexter.Writer.start_link()
{:ok, _} = Dexter.CodeIntelCache.start_link()

IO.puts(:stderr, "Dexter BEAM: started (pid #{System.pid()})")

try do
  :ok = Dexter.Writer.send_ready(0)
  Dexter.Loop.run()
rescue
  e ->
    IO.puts(:stderr, "Dexter BEAM: crash in loop: #{Exception.message(e)}")
    IO.puts(:stderr, Exception.format_banner(:error, e, __STACKTRACE__))
catch
  kind, reason ->
    IO.puts(:stderr, "Dexter BEAM: crash in loop: #{inspect(kind)} #{inspect(reason)}")
end

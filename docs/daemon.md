# Workspace daemon architecture

Dexter runs one daemon per workspace. The daemon is the only process that opens
the workspace index for normal operation and the only owner of filesystem and Git
watchers. LSP and ordinary CLI processes are frontends that connect to it over a
local socket; the MCP frontend will attach the same way.

Implementation status: the daemon serves the LSP frontend and the `lookup`,
`references`, `reindex`, and `stop` commands. MCP is designed for but not
migrated yet — see [The MCP frontend](#the-mcp-frontend).

`dexter init` remains an offline maintenance command. It acquires the same
workspace ownership lock as the daemon, asks an idle daemon to exit, and refuses
to run while a daemon with attached clients owns the workspace. There is no
in-process editor mode: the daemon belongs to the workspace, and an editor that
served the index itself would be one more owner competing with the CLI — and,
once it lands, MCP — for the same caches and the same writer.

## Goals

- Keep one authoritative, fresh index for every frontend.
- Reuse expensive workspace caches across short-lived CLI and MCP requests.
- Never trade lookup correctness for startup or indexing speed.
- Keep the hot LSP path close to its in-process latency.
- Recover automatically after crashes without leases, PID files, or permanent
  stale locks.
- Keep protocol adapters independent: the workspace runtime must not import LSP
  transport, MCP, Cobra, or editor-specific concepts, and an adapter must not
  have to edit the daemon.

## Process and transport model

```text
editor <-> dexter lsp --stdio proxy --\
                                      local socket <-> workspace daemon
MCP frontend (planned) -------------/                   |
CLI lookup/references/reindex -------/                   +-- SQLite store
                                                          +-- mutation queue
                                                          +-- file/Git watchers
                                                          +-- shared caches
```

Runtime files live in the user-private `/tmp/dexter-<uid>` directory, keyed by
the workspace. The location is environment-independent so a GUI editor and a
shell cannot derive different ownership locks for the same index. Dexter refuses
the directory if it is not a real directory owned by this user, is a symlink, or
would leave the socket path past `sockaddr_un`'s limit. The key is
the first 16 bytes of the SHA-256 of the workspace identity, rendered as hex, and
each workspace gets `<key>.sock`, `<key>.lock`, and `<key>.log`. Nothing is placed
under the repository: `sockaddr_un` paths are capped near 104 bytes, repository
paths can be read-only, and this is machine-local state. The socket is removed on
exit. The lock file is deliberately never unlinked — unlinking a lock file that
another process holds would let a second daemon take ownership of the same index.

Every connection starts with a small versioned handshake carrying the workspace
*identity*, the connection kind, and the frontend's `ContractVersion`. Identity
is the symlink-resolved root, and it decides which daemon owns the physical
workspace. The root the daemon *indexes* keeps the spelling its starter used,
because stored paths are matched against the URIs an editor sends: canonicalizing
them would break every path-keyed lookup for a project reached through a symlink
(on macOS a temp dir is `/var/...` to the editor and `/private/var/...` after
resolution). The handshake also carries that indexed spelling. A frontend using
another alias is refused with the daemon's root instead of receiving incorrect
path-keyed answers. A frontend that is not sitting
in the project names it with `dexter --root <path>` (or `-C`), which changes
directory before the workspace is resolved, so relative paths follow the named
root and the spelling an editor or agent chose is the one that gets indexed.

An LSP connection becomes a raw, bidirectional LSP byte stream after the
handshake; the stdio frontend never decodes or re-encodes JSON, so the hop costs
a copy. Control connections use a line-delimited JSON protocol: requests and
responses carry an id, and the daemon may push notifications (a method, no id)
between them. One connection therefore multiplexes concurrent calls and
subscriptions, and a slow reindex cannot block a lookup.

## The restart contract

`ContractVersion` is the one number that says whether a frontend and a daemon
can share a workspace: the handshake and control methods, plus the index
semantics a frontend relies on. It is bumped only when an older daemon must not
keep serving a newer build — a breaking wire change, or an index or parser
change whose answers would be wrong until the index is rebuilt. `IndexVersion`
(the store's own rebuild trigger) is bumped in the same release, so the new
daemon's startup rebuilds a populated stale index.

On a mismatch the newer side wins, and the daemon does the moving:

- A newer frontend replaces the daemon. A daemon that understands the contract
  answers the refusal with `exiting` and shuts itself down gracefully; one that
  predates the contract is signaled through the pid its refusal carries, or
  located by its command line when the refusal predates the pid too. The
  frontend then waits for the ownership lock and starts the current build.
- A newer daemon refuses the older frontend with a message telling its user to
  restart it. The daemon cannot force that: an LSP or MCP frontend is a process
  its host (the editor, the agent) started and supervises, and hosts restart
  those on their own terms or not at all.
- With nobody to notice, the idle timeout is the fallback: the daemon exits on
  its own and the next frontend starts the current build.

## Ownership and crash recovery

The daemon holds an advisory OS lock (`flock`, `LockFileEx`) on a deterministic
lock file for its entire lifetime. The lock file may outlive the process; file
existence never means ownership. The kernel releases the lock when the process
exits for any reason, so a crash cannot leave a workspace locked.

Startup is ordered as follows:

1. Resolve the workspace root and derive its endpoint.
2. Try the socket and validate its handshake.
3. Acquire the advisory lock. If another process owns it, wait for its socket.
4. After acquiring the lock, try the socket again to close the startup race.
5. Remove an unresponsive stale socket and bind. Binding happens *before* the
   workspace is opened: a client that races startup finds a listening socket
   immediately and its handshake completes from the backlog as soon as the
   runtime exists.
6. Open the store and start watcher setup asynchronously, so persisted-index
   queries can run immediately. The initial full-or-incremental reconciliation
   waits for watcher setup: changes made before coverage starts are found by the
   reconciliation, and later changes arrive as events.
7. Report readiness. Frontends that must not answer from a half-built index wait
   on it (`workspace/status` with `waitReadyMs`). The LSP path serves immediately
   and converges, exactly as the standalone server always has, so opening an
   editor never blocks on a cold build.

Only the lock owner may perform destructive index recovery. A transient error in
a client can therefore never delete a database another process is using.

## Workspace and session state

Workspace state is shared:

- SQLite store and index write coordination (`lsp.IndexCoordinator`)
- filesystem and Git watchers
- initial readiness and full/incremental reindex state
- stdlib and dependency discovery
- use-chain, generated-function, and compiled-module caches where safe

LSP session state is not shared:

- open and unsaved document overlays
- negotiated client capabilities and position encoding
- client connection and server-initiated requests
- session configuration

Each attached editor session is registered under an id that its handshake
response returns. Another frontend may attach to that id explicitly and answer
from that editor's overlay; an unknown id is refused at handshake, and a frontend
must never pick a session it was not handed, or it would answer from someone
else's unsaved buffers.

The disk-backed index remains globally authoritative. LSP requests prefer that
session's overlay where existing handlers already do so. Standalone CLI and MCP
requests see disk state.

## Index mutation rules

All mutation sources enter one coordinator:

- initial and explicit reindex
- filesystem notifications
- Git HEAD changes
- LSP save and watched-file notifications
- rename and other workspace edits

Duplicate path events are coalesced by path. An isolated event is applied
immediately; the loop opens a 25 ms window only when more events are already
queued, so a single save pays no debounce tax and a branch switch still becomes
one pass. Events that arrive during a sweep remain dirty and are reconciled after
it. A prune rechecks the filesystem under the shared write coordination before
removing a stored path.

Read queries are not serialized behind this queue. SQLite reads and pure source
parsing remain concurrent; only the operations whose ordering affects index
correctness are serialized. Cold parsing stays parallel and database write
transactions stay as short as the current indexer permits.

Subscribers — `Runtime.Subscribe` in process, `workspace/watch` over the socket —
receive each coalesced batch. Delivery never blocks indexing: a consumer that
falls behind loses detail and its next delivery reports a full change, so it
refreshes coarsely rather than silently missing an edit.

On macOS with cgo, one recursive FSEvents stream watches the project. Other
builds use fsnotify and register each project directory. `deps` and generated
trees are excluded from both backends. Changes to `mix.lock` or a Mix manifest
schedule dependency reconciliation instead, and an attached editor session still
registers `didChangeWatchedFiles`, whose glob covers `deps/` and path
dependencies. Events from both sources coalesce by path in the one queue, so the
overlap costs a map insert rather than a second reindex. A watcher event overflow
or an FSEvents dropped-event flag causes a full reconcile. If FSEvents cannot
start, Dexter falls back to fsnotify. A directory the kernel refuses to watch is
logged and tracked instead of aborting the fsnotify tree. The runtime reconciles
once when coverage is lost, retries only failed registrations, and reconciles
after each restored subtree to catch changes made during its gap. Failure to
create the native watcher is retried the same way. There is no periodic full-tree
reindex, so a persistent kernel watch limit does not cause recurring CPU spikes.

## Lifecycle

An open connection is a lease: an LSP or long-lived MCP connection keeps the
daemon alive. After the last connection closes, the daemon exits once the idle
timeout has passed with no connections and no active work. The default is 15
minutes; `DEXTER_DAEMON_IDLE_TIMEOUT` sets it for every daemon a machine spawns
(including one an editor starts), `dexter daemon --idle-timeout` sets it for one
run, and `0` means never exit. Idle cost is a 2 s `.git/HEAD` stat, so the reason
to bound it at all is the resident memory and watch descriptors a large workspace
holds after the project is abandoned.

Shutdown stops event sources, drains accepted mutation work, waits for background
index writes, checkpoints and closes the store, closes the socket, and finally
releases the ownership lock. `dexter stop` asks a daemon to run that shutdown on
demand and reports whether one was running; a plain stop is refused while
clients are attached, and `--force` skips the handshake entirely, locating the
daemon process by command line and signaling it. That is the path that works for
a daemon which is wedged, or one built by another version whose refusal cannot
carry a pid. A daemon that ignores SIGTERM is killed outright after a short
grace; the index is a derived cache, so an uncatchable kill only costs the next
daemon one recovery pass.

An editor's `exit` ends that session, not the daemon: the session closes its own
stream, which releases its registration and its lease. Other editors, and any
CLI call, keep the daemon alive.

Clients reconnect or start a replacement daemon after an unexpected exit. The
replacement always performs an incremental reconciliation before reporting the
workspace ready.

## Extension seams

Two registries let a protocol adapter land without editing the daemon:

- `daemon.RegisterFrontend(kind, Frontend)` serves a handshake kind in-process.
  The adapter receives the runtime, a language-service resolver, a notification
  writer, and a context canceled when the connection ends. `control` and `lsp`
  are reserved.
- `daemon.RegisterMethod(name, MethodHandler)` adds a control method. Built-in
  names are reserved: an adapter silently replacing shutdown, the reindex
  barrier, or the watch subscription would be a correctness bug, not an
  extension.

Both panic on a reserved or duplicate name. Registration happens during package
initialization, so a collision is a build-time programming error rather than a
runtime surprise.

Built-in control surface:

| Method | Purpose |
|---|---|
| `daemon/status` | pid, version, protocol, readiness, client count, uptime, registered frontends |
| `daemon/shutdown` | exit when no other client is attached; refuse otherwise |
| `workspace/status` | readiness, watcher state, stdlib root, index version and size, attached sessions; `waitReadyMs` turns it into an index barrier |
| `workspace/lookup` | module/function lookup with the CLI's non-strict module fallback |
| `workspace/references` | store-level references |
| `workspace/reindex` | whole workspace or one path, returning after the barrier |
| `workspace/impactSnapshot` | reconcile the mutation queue, validate impact evidence, and export an exact portable snapshot from the daemon-owned index |
| `workspace/watch`, `workspace/unwatch` | subscribe to coalesced index changes, pushed as `workspace/changed` notifications |

## The MCP frontend

MCP is not migrated onto the daemon yet. Its server still carries its own copy of
workspace ownership: a store handle, a headless `lsp.Server`, stdlib discovery,
a filesystem watcher, a Git HEAD poll, an initial index pass, and an index barrier.
The daemon exists so those can be deleted rather than duplicated a second time.
The table below is the mapping that migration applies, and the sections after it
are the design it should follow.

| MCP-side concept | Daemon equivalent |
|---|---|
| `binding.init`, `openStore` recovery | `workspace.Open` (same recovery path) |
| `binding.lsp = lsp.NewServer(...)` | `Runtime.LanguageServices()` |
| `stdlib.Resolve` + `SetStdlibRoot` | resolved once by the runtime, inherited by sessions |
| `WatchFiles` (own watcher) | `workspace.Watcher` → one mutation queue |
| `lsp.WatchGitHead` | `Runtime.startGitWatch` |
| `binding.awaitIndex`, `indexWaitLimit` | `workspace/status` with `waitReadyMs` |
| `binding.close` | close the connection; the daemon idles out |
| `mcp.Config{LSP, Store, ProjectRoot}` | `Runtime.LanguageServices()/Store/Root` accessors |

### Two ways to attach

Both shapes are reachable through the registries above, and they are not equally
cheap. Neither is wired up yet. The MCP Go SDK speaks JSON-RPC over an `io.ReadWriteCloser`, but its
`InMemoryTransport` keeps that field unexported and `newIOConn` is internal, so
there is no supported way to hand it a raw socket.

**Control methods (recommended).** Keep the MCP protocol server in the frontend
process over stdio, exactly as it is today, and reach the workspace through
control calls. Register one method per tool backend —
`workspace/definition`, `workspace/rename`, `workspace/callHierarchy`,
`workspace/implementations`, `workspace/outline`, `workspace/moduleAPI`,
`workspace/search` — each a thin wrapper over the equivalent `internal/lsp/api.go`
call made against `mc.LSP()`. The frontend's per-root binding becomes
`daemon.Ensure(ctx, root)`; its index barrier becomes `client.WorkspaceStatus(ctx,
waitReadyMs)`; its watcher becomes `client.Watch(ctx, buffer, onChange)`;
its `close` becomes `client.Close()`. The cost is one local JSON round trip per
tool call, which is noise next to the model latency that triggered the call, and
it needs no transport work and no SDK coupling. Multi-root negotiation stays in
the frontend: one control connection per negotiated root.

**Daemon-hosted frontend.** `RegisterFrontend("mcp", ...)` plus
`daemon.ProxyFrontend(ctx, root, "mcp", session, os.Stdin, os.Stdout)` would run
the MCP server inside the daemon, so tool calls never cross a socket and
`fc.LSP()` gives attached mode the editor's unsaved buffers. It requires
implementing the SDK's `Transport`/`Connection` pair over the socket — about the
same newline-delimited JSON framing the LSP stream already avoids by proxying
bytes — and that implementation tracks SDK internals. Worth revisiting only if
per-call latency ever becomes measurable, or if the SDK grows a constructor that
wraps an `io.ReadWriteCloser`.

Rules for that work:

- Attach per root. One daemon per negotiated workspace root, one control
  connection each; the daemon itself does not become multi-root.
- Reach name-based operations through `Runtime.LanguageServices()`, or through the
  attached editor session when the client explicitly named one. Add
  protocol-neutral methods to the runtime, or register a control method, rather
  than implementing queries in an adapter.
- Never open the store, start a watcher, or run a second LSP lifecycle in the
  frontend process.
- Gate a cold workspace with `workspace/status` instead of a private barrier.
- Bump `ContractVersion` when a wire change breaks an older frontend, and treat
  method names and payload shapes as the adapter contract.

## Performance requirements

The stdio LSP frontend is a raw stream proxy after the handshake. Control clients
multiplex concurrent calls over one reused connection. Reads are never queued
behind the mutation coordinator.

Benchmarks should cover warm definition/hover/completion latency, daemon startup,
CLI lookup, large reference responses, event-to-index latency, concurrent reads
during a reindex, and total memory/CPU with LSP and MCP connected together (MCP
once it is migrated).

The target for the daemon hop is no more than 1 ms added p95 latency for hot LSP
operations. `cmd/lspprobe` measures a real project over the wire; calling the
same handler against an in-process `lsp.Server` over the same store, as the
`internal/lsp` tests do, establishes the other end of the comparison. If the
target cannot be met, optimize the transport before adding a hybrid ownership
model.

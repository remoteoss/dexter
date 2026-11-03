# Workspace daemon architecture

Dexter runs one daemon per workspace. The daemon is the only process that opens
the workspace index for normal operation and the only owner of filesystem and Git
watchers. LSP, MCP, and ordinary CLI processes are frontends that connect to it over a
local socket.

Implementation status: the daemon serves the LSP frontend, the MCP frontend
(`dexter mcp`, see [The MCP frontend](#the-mcp-frontend)), and the `lookup`,
`references`, `reindex`, and `stop` commands.

`dexter init` remains an offline maintenance command. It acquires the same
workspace ownership lock as the daemon, asks an idle daemon to exit, and refuses
to run while a daemon with attached clients owns the workspace. There is no
in-process editor mode: the daemon belongs to the workspace, and an editor that
served the index itself would be one more owner competing with the CLI and
MCP for the same caches and the same writer.

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
MCP frontend (dexter mcp) ----------/                   |
CLI lookup/references/reindex -------/                   +-- SQLite store
                                                          +-- mutation queue
                                                          +-- file/Git watchers
                                                          +-- shared caches
```

Runtime files live in the user-private `/tmp/dexter-<uid>` directory, keyed by
the workspace. The location is environment-independent so a GUI editor and a
shell cannot derive different ownership locks for the same index. Dexter refuses
the directory if it is not a real directory owned by this user, is a symlink, or
would leave the socket path past `sockaddr_un`'s limit. On a multi-user host
another user can create `/tmp/dexter-<uid>` first; Dexter then uses a second
candidate under the home directory that the user database records for the uid
(not `$HOME`): `~/Library/Caches/dexter/run` on macOS, `~/.cache/dexter/run`
elsewhere. The first candidate that can be used wins, and that state is the same
for every process of the user, so the GUI editor and the shell still agree. A
daemon checks on each endpoint check (below) that new frontends still resolve
its directory, and exits when they do not; while a daemon holds the workspace in
one candidate, no process takes ownership in another. `DEXTER_RUNTIME_DIR`
replaces every candidate; it exists for tests and sandboxes, and every frontend
of a project must see the same value. Dexter does not change the directory that
it names: it uses its own subdirectory `dexter-<uid>` there, and only that one is
made private and cleaned.

The key is the first 16 bytes of the SHA-256 of the workspace identity, rendered
as hex, and each workspace gets `<key>.sock`, `<key>.lock`, and `<key>.log`. When
a candidate directory is so long that the socket path would pass the limit, the
key is the first 8 bytes instead. Nothing is placed under the repository:
`sockaddr_un` paths are capped near 104 bytes, repository paths can be
read-only, and this is machine-local state. The socket is removed on exit, but
only when it is still the one that this daemon bound.

A lock file is unlinked only by a process that holds its lock, and every lock
acquisition checks after `flock` that the path still names the file it locked;
otherwise it retries. That makes it safe for a starting daemon to clean up the
runtime files of workspaces that no daemon serves: sockets and lock files whose
lock it can take, and logs older than 14 days. Only names that Dexter makes are
touched, and a socket on which a process still answers is kept (a daemon whose
lock file was removed is alive). Such a cleanup, and `dexter stop`, hold the lock
of another workspace for a moment; a daemon that starts in that moment tries
again for 100 ms, and a frontend whose daemon exited because it found the
workspace owned while nobody serves it starts one again.

Every connection starts with a small versioned handshake carrying the workspace
*identity*, the connection kind, and the frontend's `ContractVersion`. Identity
is the symlink-resolved root, and it decides which daemon owns the physical
workspace. On macOS it also takes the case that the file system stores
(`F_GETPATH`), because the default file system ignores case and
`filepath.EvalSymlinks` keeps the case the caller typed: without it,
`~/Code/app` and `~/code/app` would get two locks and two daemons that build
one index at the same time. The root the daemon *indexes* keeps the spelling its starter used,
because stored paths are matched against the URIs an editor sends: canonicalizing
them would break every path-keyed lookup for a project reached through a symlink
(on macOS a temp dir is `/var/...` to the editor and `/private/var/...` after
resolution). The handshake also carries that indexed spelling. A frontend using
another alias is refused with the daemon's root instead of receiving incorrect
path-keyed answers. A frontend that is not sitting
in the project names it with `dexter --root <path>` (or `-C`), which changes
directory before the workspace is resolved, so relative paths follow the named
root and the spelling an editor or agent chose is the one that gets indexed.

An LSP connection becomes a bidirectional LSP byte stream after the
handshake; the stdio frontend never decodes or re-encodes JSON, so the hop costs
a copy and a short scan (see [Performance requirements](#performance-requirements)). Control connections use a line-delimited JSON protocol: requests and
responses carry an id, and the daemon may push notifications (a method, no id)
between them. One connection therefore multiplexes concurrent calls and
subscriptions, and a slow reindex cannot block a lookup.

A connection runs at most 64 requests at once; one more is refused at once
instead of blocking the reader. Each request has its own context, derived from
the connection's. A client that stops waiting for a request sends
`{"id":0,"method":"$/cancel","params":{"id":N}}`, the pattern of LSP's
`$/cancelRequest`: the reader handles it outside the request limit and cancels
request `N`, so its index waits end and its slot is free again. The canceled
request still answers; the client has dropped it. A subscription made by
`workspace/watch` belongs to the connection, not to the request that made it.

Each message is one line of at most 16 MiB, newline included. A writer refuses
a longer line before sending anything, so the stream stays in step: a result
that is too large fails only its own call, and a `workspace/changed`
notification with too many paths is sent as a full change instead. Location
lists from `workspace/lookup` and `workspace/references` keep that rare by
sending each path once:

```json
{"files":["/w/lib/a.ex","/w/lib/b.ex"],
 "locations":[{"file":1,"line":9,"kind":"call"},
              {"file":0,"line":1,"kind":"function","arity":2,"declaration":true}],
 "ready":true}
```

`file` indexes `files`, and the locations keep their result order. `kind`,
`arity`, and `declaration` are omitted when empty, zero, or false. A result
from an index that is being rebuilt or cannot be used also carries
`"notes":[{"severity":"warning","message":"..."}]`, the same text an editor
shows; the field is omitted when no index condition is active.

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

A `dexter lsp` proxy that cannot attach for any of these reasons, or for any
other startup error, does not only print to stderr: it reads the editor's
`initialize` request, sends `window/showMessage` (Error) with the explanation
and the fix, answers `initialize` with JSON-RPC error -32603 that carries the
same text, and exits. An editor that is older than the daemon is told to
restart from the current binary; a daemon that cannot be replaced, a root
spelling mismatch, and a workspace held by `dexter init` get their own fix.

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

The lock is not proof on its own. An age-based temp cleaner such as
systemd-tmpfiles, or `rm /tmp/dexter-*`, can remove the lock file of a running
daemon, and the next process would lock a new file at the same path. So a
process that gets the lock also refuses ownership while a daemon answers on the
workspace socket, and while the same workspace holds a lock in another runtime
directory candidate. `dexter init --force` therefore refuses, instead of deleting
the index under a daemon whose lock file is gone.

The running daemon checks its endpoint every 10 seconds, with a few `stat`
calls:

- The socket must be the one it bound. When it is gone, or another daemon bound
  a new one at the path, the daemon exits; its editors reconnect (see
  [Lifecycle](#lifecycle)) to whatever serves the path.
- The lock file must be the one it locked. When it was removed, or replaced by a
  file that nobody holds, the daemon locks the file at the path again. When
  another process holds that file, that process owns the workspace and the
  daemon exits.
- The socket and the lock file get new times once an hour, so age-based cleaners
  leave them alone.
- The index file must be the one the store opened. An older Dexter build that
  runs `dexter lsp` in-process (0.7.x) rebuilds an index of its own version at
  the same path, and `git clean -fdx` or `rm -rf .dexter` removes it; a daemon
  that kept its open file would write to a file that nobody reads again. The
  daemon restarts its runtime on the file that is there now, or on a new one,
  and every editor gets a warning that says why. A second replacement within 10
  minutes does not restart it again (that would be a rebuild loop against a
  process that keeps replacing the file); the editors get an error that says
  which process to stop. Builds without the daemon (0.7.x and earlier) and later
  builds must not serve one project at the same time.
- The project root must be there. When its volume is ejected (FSEvents reports
  the unmount at once; the check catches the other cases), the index work stops
  and the daemon exits without closing the store: SQLite maps the `-shm` file of
  that volume, and any access to it can end the process with SIGBUS. A daemon
  never starts for a root that is not there, because opening the index would
  create the directory again.

`dexter stop --force` stops every daemon process whose command line names the
root, not only the first one that `ps` lists.

## Workspace and session state

Workspace state is shared:

- SQLite store and index write coordination (`lsp.IndexCoordinator`)
- filesystem and Git watchers
- initial readiness and full/incremental reindex state
- stdlib and dependency discovery: the coordinator keeps the detected
  standard library (also when none was found) for 5 minutes or until
  `.tool-versions`, `mise.toml`, `.mise.toml`, `.elixir-version`, or
  `DEXTER_ELIXIR_LIB_ROOT` changes, so an editor after the first does not run
  version managers and login shells again. When the shared root is empty and
  a later detection finds a library, the workspace uses it. A `mix` that the
  search found is kept the same way; a search that found nothing is reused only
  by editors that started before it ended (editors that open together), so an
  editor restarted after the user installed mix searches again. The first
  explicit `stdlibPath` sets the standard library for the life of the
  workspace; two spellings of one directory (a trailing slash, a symlink) are
  one path, and an editor that asks for another directory is warned
- use-chain, generated-function, compiled-module, and dependency caches: they
  read only the disk and the shared index, so they live on the coordinator and
  every session uses the same ones
- remembered BEAM startup failures, for each build root and mix binary

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

One fact about the overlays is shared: which documents each session has open.
The `IndexCoordinator` keeps that registry (document keys only, never text), so
that a rename never writes a file on disk under a buffer that another editor
has open: an editor's rename puts the change of such a file into its own edit,
and a headless rename reads the other session's buffer only to compare it with
the disk. Each session updates the registry when it opens or closes a
document, and the end of a session removes all of its entries. See "What a
rename writes on disk" in `docs/architecture.md`.

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

`Runtime.FlushEvents` waits until the index has every file change that happened before the call: the native watcher's event latency (FSEvents coalesces for 25 ms), then a barrier event in the queue. A rename uses it when a file it plans from has moved and the watcher has not indexed the new path yet.

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
or an FSEvents dropped-event flag causes a full reconcile. A directory that is
created, renamed, or removed costs only its subtree: a directory that is there
is walked with the skip rules of the index walks, so it reports what a full
walk would index (`deps/` included; a nested worktree in it becomes a top), and
a directory that is gone is sent as one path, which removes what
was indexed below it with one range read. A directory that was renamed or
removed and is there again is sent as well, and the indexed files below it that
are gone are removed. FSEvents reports paths in the case that the file system
stores, so the stream compares them with that spelling of the root
(`F_GETPATH`) and maps them back to the spelling the runtime indexes; a root
typed in another case is watched. After a rename that changes only the case
(`mv lib/Area lib/area`), a stat of the old spelling still succeeds; the old
spelling is found with `F_GETPATH`, only for paths that are already suspect (a
path in an event, or a stored path that a full walk did not see), and it is
removed from the index. If FSEvents cannot
start, Dexter falls back to fsnotify. A directory the kernel refuses to watch is
logged and tracked instead of aborting the fsnotify tree. The runtime reconciles
once when coverage is lost, retries only failed registrations, and reconciles
after each restored subtree to catch changes made during its gap. Failure to
create the native watcher is retried the same way. There is no periodic full-tree
reindex, so a persistent kernel watch limit does not cause recurring CPU spikes.
Each of these states (no native watching, the fsnotify fallback, directories
that cannot be watched) is a condition in the workspace reporter: every attached
editor sees it, an editor that attaches later receives it, and the user is told
when coverage comes back. See "Telling the user" in `docs/architecture.md`.

A batch that holds a `mix.exs` or `mix.lock` of the workspace becomes one full
reconcile, however many manifests it holds; the full pass also covers the other
paths of the batch. A manifest in a nested worktree does not count.

A pass that cannot write some files (a full disk, an I/O error) reports them as
failed, and the runtime runs a full reconcile again after 30 s. Each later
failure doubles the delay, up to 10 minutes, and a complete full pass stops the
retries. The failed files keep their old stored mtime, so the retry writes them.

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

When the project root is moved, deleted, or unmounted, or its path names a
different directory without the daemon's open index file in it (a copy of the
index is another file), the daemon tells every attached editor, logs it, and
exits without changes to the index. A root that cannot be read for now
(a permission error, or EIO or ESTALE on a network mount) does not count: the
daemon changes nothing until the error clears, and then goes on. Neither does a
remount that gives the root a new inode while the index file is still there.

The index file is compared by inode only, because a remount can change the
device number. A remount that also gives the index file a new inode counts as a
root that is gone, and the daemon stops. A FUSE mount without stable inodes does
that, for example sshfs without `-o use_ino`. Mount with stable inodes (sshfs
`-o use_ino`), or restart the editor after the remount: the next frontend starts
a new daemon, which uses the index as it is.
The index file is in the root, so it moves with the root, and a walk of a root
that is gone finds no files: before, the prune then deleted every stored file and
the daemon served empty answers. The next frontend then fails clearly, or starts
a new daemon on the directory that the path now names.

An editor's `exit` ends that session, not the daemon: the session closes its own
stream, which releases its registration and its lease. Other editors, and any
CLI call, keep the daemon alive.

When the daemon stops during a session — a crash, `kill -9`, a replacement by a
newer build, `dexter stop --force`, an endpoint check that made it exit, or a
restart of its runtime — the `dexter lsp` proxy carries the editor's session to
a new daemon. It keeps what the session state is made of, as the raw messages:
the `initialize` request, the `initialized` notification, the last
`workspace/didChangeConfiguration`, and one copy of each open document: the
`didOpen`, and after the first change the last `didChange`. Dexter asks for full
document sync, so that message carries the whole text and is kept as it is,
without a parse; a change that has a range anyway is applied to the proxy's copy,
with characters counted in UTF-16 code units and lines ended by `\n`, `\r\n`,
or `\r`, as LSP defines them. The proxy also keeps the
work-done progress that the daemon began and did not end. On the end of the
daemon stream it:

1. ends the open progress in the editor (each daemon makes its own tokens, so a
   new daemon never continues an old one), answers each request that the
   daemon left open with ContentModified
   (-32801), which editors take as "ask again" rather than as a failure, and
   drops server requests that the old daemon sent (an editor's late answer to
   one is not passed to the new daemon);
2. reaches a daemon again through the normal start path, which starts one when
   necessary — five attempts with backoff, within 90 seconds. A daemon process
   that exits while it starts ends that attempt at once, and the cause, with the
   end of the daemon log, is what the editor is told, not the end of the wait;
3. replays the session: `initialize` with an id of its own (the answer is
   dropped; the editor has one), `initialized`, the configuration, and the open
   documents, each as one `didOpen` with its current text and version, and,
   for a buffer with unsaved changes (a `didChange` after its open or its last
   `didSave`), one full `didChange` with the same text, so that the new daemon
   gives MCP tools and the CLI the editor's text. An
   `initialize` that the old daemon never
   answered is sent with the editor's id, and its answer goes to the editor. A
   second `client/registerCapability` from the replayed session is answered by
   the proxy, because the editor holds that registration already.

Messages the editor sends meanwhile wait and follow the replay, so none is lost
and none overtakes it; a request that could not be sent to a daemon that went
away during the replay waits for the next one. `shutdown` and `exit` stop the
reconnection at once: the proxy answers `shutdown` itself and exits on `exit`. A
`$/cancelRequest` for a request that waits in the queue removes it and is
answered with RequestCancelled (-32800); a cancel for a request that the proxy
answered when the old daemon stopped is not passed to the new daemon.

In the daemon, LSP messages run one after the other, as before. A request that
can run long (references, workspace symbols, rename, formatting,
implementations, call hierarchy) runs off the reader's goroutine, with the
messages that follow it until the queue is empty, so the session keeps reading:
`$/cancelRequest` cancels the context of the request that it names, and that
request is answered with RequestCancelled. Hover, completion, and the other
short requests still run on the reader's goroutine and pay no hand-off. At
most 1,024 messages wait behind a slow request; then the reader waits, as it did
when every message ran on it. Every handler's context ends with its
connection: when the connection closes, the queued messages are dropped, the
running request gets up to 5 seconds to see its canceled context, and only then
is the session released. A released session starts nothing new: no BEAM, no
Erlang warm-up, no document warm-up. References (and the
`workspace/references` control method), workspace symbols, the planning of a
rename, and formatting stop when their context ends; a module rename stops
only before it starts to move and write files. When the daemon cannot be reached — a newer build took
over and this proxy is older, the root is spelled another way, the project
directory is gone, or every attempt failed — the proxy sends
`window/showMessage` (Error) with the reason and the fix, answers the requests
it still holds, and exits with a non-zero status. A session that the editor ends
(`shutdown` and `exit`, or the end of its input) is not carried over.

The replacement daemon always performs an incremental reconciliation before
reporting the workspace ready.

A panic does not stop the daemon. jsonrpc2 runs LSP handlers on its own
goroutine, so the session's handler chain recovers a panic itself: the request
gets an InternalError answer, the panic and its stack go to the log, and every
editor sees an `internal.<kind>` error condition once per kind of work. The
goroutines that the daemon owns (the mutation loop, the watchers, index passes,
report delivery, the client log, the parser) recover the same way. Work whose
state can be wrong after a panic — an index pass that left a write transaction
open — makes the daemon restart its runtime, at most once per 10 minutes; a
watcher event that was lost becomes a full reconcile, and a file that the parser
cannot handle is a file that could not be indexed.

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
| `workspace/status` | readiness, watcher state, stdlib root, index version and size, attached sessions, active failure and degraded conditions; `waitReadyMs` turns it into an index barrier |
| `workspace/lookup` | module/function lookup with the CLI's non-strict module fallback |
| `workspace/references` | semantic references through the shared language service |
| `workspace/reindex` | whole workspace or one path, returning after the barrier |
| `workspace/watch`, `workspace/unwatch` | subscribe to coalesced index changes, pushed as `workspace/changed` notifications |
| `$/cancel` | cancel one in-flight request of this connection; sent with id 0, no response |

## The MCP frontend

`dexter mcp` keeps the MCP protocol server in the frontend process (over stdio,
or streamable HTTP with `--listen`) and reaches the workspace through one
registered control method, `mcp/tool`. It opens no store, starts no watcher, and
runs no LSP lifecycle of its own.

- **Roots.** Multi-root negotiation stays in the frontend. Each MCP session's
  root comes from MCP roots, resolved with the same project-root search as the
  CLI, or from the command line. Sessions that resolve to the same root share
  one control connection (`daemon.Ensure`), which is the lease that keeps the
  daemon alive; the connection closes when the last such session ends. The
  frontend keeps the spelling the client gave. When the daemon already serves
  the workspace through another spelling, the frontend uses the daemon's root.
- **Tools.** `internal/mcp` registers `mcp/tool` at package initialization. A
  call carries the tool name, its arguments, and `waitReadyMs`. The daemon runs
  the tool body against `mc.LSP()` (the headless language service, since MCP
  never names an editor session) and the runtime's store. One method instead
  of one per tool keeps each tool's parameter type in one place, shared by the
  input schema the agent sees and the body that decodes it. Definitions and
  references use `LookupName` and `ReferenceNames`, the same name navigation as
  the editor and the CLI.
- **Cold and degraded index.** A tool waits up to `waitReadyMs` (capped at
  30 s) for the initial reconciliation, then answers from what is indexed. An
  answer from an index that is still building, or that has an active warning
  or error condition, ends with a note that says so. The rename tool refuses
  until the index is complete, because a rename from a partial index would
  change some call sites and leave others with the old name.
- **Editor buffers.** Tools that read file text (outlines, definition and
  module docs, reference lines) use the newest buffer that an attached editor
  session holds open with unsaved changes: the editor changed it after its
  last open or save (`didChange` after `didOpen`/`didSave`). A buffer with no
  unsaved changes can be older than the disk (an editor that has not reloaded
  the file after an agent wrote it), so the disk is used. When the file on
  disk changed after the buffer's last change, the disk is used too, and the
  answer warns that the editor's unsaved changes may conflict. A dirty buffer
  whose text is the disk's (CR characters and one final newline ignored), as
  after an editor reloads the file, counts as clean. With one-second file
  times, a disk change in the same second as the buffer's last change has no
  known order: the buffer is used, and the answer warns about it. The answer
  names the files that came from unsaved buffers. The index positions refer
  to the saved file, so a line is mapped into the buffer with a line diff
  (Myers, line endings ignored; above its bounds, a patience diff on the
  lines that occur once in each text); a position on a changed line
  shows the saved line, marked as such. Paths that the agent gives must be
  inside the project root after symlinks are resolved, compared in the
  spelling that the file system stores (so another case of a path inside the
  root is accepted where the file system ignores case), and must name a
  regular file of at most 10 MB.
- **Injected and generated functions.** `dexter_module_api` lists the public
  functions of a module that its source does not define, from
  `lsp.ModuleMembers`: defs in the quote of a `use`d module's `__using__`
  (followed through the use chain, as definition does), and functions that
  only the compiled module exports (the generated-function reader that
  completion uses). Each shows where it comes from and the location that
  `dexter_definition` gives; a compiled module older than the source is
  marked. `dexter_search` adds them for a `Module.function` query (the module
  part may be the end of the name); a query with no module part does not
  search them, because that would read the compiled module of every module.
  The editor's workspace symbols use the same search (`lsp.SearchNames`).
- **Limits and cancellation.** One frontend runs at most 32 tool calls at once
  on a workspace connection, below the daemon's 64, and a call past the limit
  waits for a slot. A call that the MCP client cancels sends `$/cancel`, so the
  daemon stops waiting for the index for it. A canceled rename says that it may
  have been applied. When the client's roots change to another project during
  a call, the call ends with an error that says so.
- **HTTP.** `--listen` accepts only a loopback address unless
  `--listen-unsafe` is given, because the server has no authentication. The
  SDK refuses a non-loopback `Host` on a loopback connection (DNS rebinding),
  cross-origin browser requests are refused, a request body over 4 MB gets
  413, and a session with no request for 30 minutes is closed, so a client
  that went away does not keep its daemon alive.
- **Reconnects.** When the daemon goes away (an upgrade replaced it, or
  `dexter stop --force`; a plain `dexter stop` is refused while MCP is
  attached), the next tool call connects again, which starts a new daemon.
  A read-only call that was in flight is sent once more; a rename is never
  repeated.
- **Rename.** The rename runs on the headless language service and writes the
  changed files on disk. It does not yet look at buffers that an editor attached
  to the same daemon has open. The shared rule for frontends without an editor
  (never write over unsaved editor work; refuse with an actionable error when an
  affected file has unsaved changes) will replace the call in
  `internal/mcp/rename.go` when it lands in `internal/lsp`.

A daemon-hosted frontend (`RegisterFrontend("mcp", ...)`, with the MCP server
inside the daemon) would save the local round trip per tool call. It needs an
SDK `Transport` over the socket, which tracks SDK internals, and the round trip
is noise next to the model latency that caused the call. Revisit it only if the
per-call latency becomes measurable.

Bump `ContractVersion` when a change to `mcp/tool` breaks an older frontend; the
method name and payload shapes are part of the adapter contract.

## Performance requirements

The stdio LSP frontend copies each message after the handshake; it does not
decode or re-encode it. To carry a session to a new daemon it reads the method
and the id of each message with a scan that stops as soon as it has them (the
daemon's encoder puts the id of an answer last, so that one is read from the
end), and the document URI of `didOpen`, `didChange`, and `didClose`. A string is
skipped with a byte search, so the scan of a 200 KB `didChange` that puts the
text before the URI costs about 70 µs, and the scan of an answer of any size
costs about 30 ns. Control clients multiplex concurrent calls over one reused
connection. Reads are never queued behind the mutation coordinator.

Benchmarks should cover warm definition/hover/completion latency, daemon startup,
CLI lookup, large reference responses, event-to-index latency, concurrent reads
during a reindex, and total memory/CPU with LSP and MCP connected together.

The target for the daemon hop is no more than 1 ms added p95 latency for hot LSP
operations. `cmd/lspprobe` measures a real project over the wire; calling the
same handler against an in-process `lsp.Server` over the same store, as the
`internal/lsp` tests do, establishes the other end of the comparison. If the
target cannot be met, optimize the transport before adding a hybrid ownership
model.

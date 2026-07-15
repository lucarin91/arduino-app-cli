# Remote-Edit Service — API + Implementation Plan

## Goals

- Generic remote filesystem feature (not Arduino-specific in protocol).
- Lives **inside `arduino-app-cli`** (no separate binary, no separate service).
- All code isolated under `internal/editor/` with a strict, minimal public surface,
  so the rest of the daemon does not depend on its internals and the module could
  be extracted later with little work.
- Transport: WebSocket. Envelope: JSON-RPC 2.0. Codec: **JSON** for the first
  iteration, so both sides can reuse existing JSON-RPC 2.0 libraries
  (`github.com/sourcegraph/jsonrpc2` on the server, `json-rpc-2.0` on the client)
  and avoid hand-rolling an RPC dispatcher. MessagePack is deferred: revisit
  only if a benchmark against SSH-tunneled JSON + `permessage-deflate` justifies
  committing to a hand-rolled Go dispatcher (no Go library supports JSON-RPC 2.0
  cleanly over msgpack; the JS side would stay cheap either way).
- **No authentication** — app-lab reaches the board via SSH/ADB port-forward, the
  WS listener binds to `127.0.0.1`; transport security is provided by the tunnel.
- Phase 1 ships **only the file-watch capability**, with **no changes** to the
  current `pkg/board/remotefs` reads/writes. The watcher is purely additive.
- Future phases extend the same module to own reads/writes/streams once the
  protocol has proven itself.

## Architectural rules for `internal/editor/`

1. The package exposes a small public surface (`editor.Handler` etc.); everything
   else is unexported or under `internal/editor/internal/...`.
2. `internal/editor/...` MUST NOT import any other `internal/...` package
   (`orchestrator`, `api`, `store`, `platform`, …). If you ever need something
   from outside, accept it through `Config`.
3. The daemon wires the editor in a single small file under
   `cmd/arduino-app-cli/daemon/` — that is the only place daemon ↔ editor coupling
   lives.
4. No global state; everything goes through an explicit `Config`.
5. Logging uses an injected `*slog.Logger`, not `slog.Default()`.
6. The editor must be **pluggable into a WebSocket already accepted by the
   daemon's HTTP router** — i.e. it exposes an `http.Handler` (or equivalent) that
   the daemon mounts on a route. The editor does not open its own listener.

---

## Full target API (vision; not all implemented in phase 1)

All methods use the JSON-RPC 2.0 envelope, encoded as JSON over WebSocket text
frames in the first iteration. Binary file content (future phases) travels as
base64-encoded strings until/unless the codec is revisited (see Goals). One WS
connection per client session; methods multiplexed by `id`.

### Handshake

| Method  | Params                                    | Result                                                            |
| ------- | ----------------------------------------- | ----------------------------------------------------------------- |
| `hello` | `{ clientName, clientVersion, protocol }` | `{ serverName, serverVersion, protocol, capabilities[], limits }` |

`capabilities` is the negotiation surface (e.g. `fs.watch`, `fs.read`, `fs.readStream`, `fs.write`, `fs.writeStream`, `fs.patch`, `fs.hash`, `fs.walk`, `fs.search`).
`limits` advertises server-imposed caps (`maxReadBytes`, `chunkSize`, `maxWatches`).

### File watching (phase 1)

| Method       | Params                                                   | Result               |
| ------------ | -------------------------------------------------------- | -------------------- |
| `fs.watch`   | `{ path, recursive, includes?, excludes?, debounceMs? }` | `{ subscriptionId }` |
| `fs.unwatch` | `{ subscriptionId }`                                     | `{}`                 |

Notifications (server → client):

- `fs.changed` — `{ subscriptionId, events: [{ type: "create"\|"update"\|"delete"\|"rename", path, isDir, oldPath? }] }`
  - `oldPath` is present only when `type == "rename"` (previous path of the entry); omitted for all other event types.
  - Events are coalesced over `debounceMs` (default 50 ms).
- `fs.watchError` — `{ subscriptionId, message, fatal }`

**Watcher events are NOT diffs.** `fs.changed` reports _that_ a file changed,
not _what_ changed. Server-pushed text diffs are a separate concern handled by
`doc.changed` in phase 4, where the server already keeps the last-known content
of explicitly-opened documents. The watcher must not read file content to compute
diffs — that would force per-watched-file caching and is wrong on a board.

### File read (future)

| Method                 | Params                                                    | Result                                                                  |
| ---------------------- | --------------------------------------------------------- | ----------------------------------------------------------------------- |
| `fs.stat`              | `{ path }`                                                | `{ size, mtime, isDir, etag }`                                          |
| `fs.walk`              | `{ path, depth?, includes?, excludes?, cursor?, limit? }` | `{ entries: [{ path, isDir, size, mtime, depth, etag }], nextCursor? }` |
| `fs.read`              | `{ path, offset?, length?, baseEtag?, encoding? }`        | `{ encoding, content, etag, size }` (see Phase 2)                       |
| `fs.readStream.open`   | `{ path, offset?, length?, chunkSize? }`                  | `{ streamId, size, etag }`                                              |
| `fs.readStream.cancel` | `{ streamId }`                                            | `{}`                                                                    |

Notifications:

- `fs.stream.data` — `{ streamId, seq, data: bin, last }`
- `fs.stream.error` — `{ streamId, message }`

#### `fs.walk` semantics

- **Base case**: `{ path }` walks everything under `path` and returns all
  entries in one response — no pagination knobs required.
- `depth` — unset = unlimited (default); `0` = only `path` itself; `N` = up
  to `N` levels below `path`. Each returned entry carries its own `depth`
  (relative to the request `path`), so the client can tell boundary entries
  from inner ones without parsing paths.
- `includes` / `excludes` — glob patterns. Excluded directories are pruned
  (not descended into).
- `path` in each returned entry is POSIX-style and relative to the request
  `path`. Entries are returned in lexicographic order of that relative path;
  same `(path, depth, includes, excludes)` always yields the same order.
- **Pagination (opt-in)**: pass `limit` to cap entries per response. When more
  entries remain the server returns an opaque `nextCursor`; passing it back on
  the next call resumes from where the previous response stopped. The cursor
  encodes only the resume position, not the request shape, so callers must
  keep the other params identical across calls. Omit `nextCursor` = walk
  complete. Kept optional so early clients can ignore it; useful later for
  infinite-scroll or any bounded-response consumer, and survives reconnects
  for free.

### File write (future)

| Method                  | Params                                            | Result                                       |
| ----------------------- | ------------------------------------------------- | -------------------------------------------- |
| `fs.write`              | `{ path, encoding, content, baseEtag?, atomic? }` | `{ etag, size }`                             |
| `fs.writeStream.open`   | `{ path, totalSize?, baseEtag?, atomic? }`        | `{ streamId, chunkSize }`                    |
| `fs.writeStream.chunk`  | `{ streamId, seq, data: bin, last }`              | `{}` (or final `{ etag, size }` when `last`) |
| `fs.writeStream.cancel` | `{ streamId }`                                    | `{}`                                         |
| `fs.delete`             | `{ path, recursive? }`                            | `{}`                                         |
| `fs.move`               | `{ from, to, overwrite? }`                        | `{}`                                         |
| `fs.mkdir`              | `{ path, recursive? }`                            | `{}`                                         |

### Incremental editing (future, for app-lab CodeMirror)

| Method      | Params                                               | Result                                      |
| ----------- | ---------------------------------------------------- | ------------------------------------------- |
| `doc.open`  | `{ path }`                                           | `{ docId, content: string, version, etag }` |
| `doc.patch` | `{ docId, baseVersion, changes: [{ range, text }] }` | `{ version, etag }`                         |
| `doc.close` | `{ docId }`                                          | `{}`                                        |

Notifications:

- `doc.changed` — `{ docId, version, changes, origin: "self"\|"external" }`

`changes[]` follows LSP `TextDocumentContentChangeEvent` shape so any editor
(CodeMirror, Monaco, Neovim…) can adapt without depending on a specific editor type.

### Search (future)

| Method          | Params                                           | Result         |
| --------------- | ------------------------------------------------ | -------------- |
| `search.find`   | `{ root, pattern, regex?, ignoreCase?, globs? }` | `{ searchId }` |
| `search.cancel` | `{ searchId }`                                   | `{}`           |

Notifications:

- `search.match` — `{ searchId, path, line, column, preview }`
- `search.done` — `{ searchId, matched, scanned }`

---

## Phase 1 — Watcher-only MVP

### Scope

- Lives inside `arduino-app-cli`, under `internal/editor/`.
- Implements only: `hello`, `fs.watch`, `fs.unwatch`, `fs.changed`, `fs.watchError`.
- Exposes an `http.Handler` that the daemon mounts on a WebSocket route.
- JSON codec, JSON-RPC 2.0 envelope, one connection = one session. MessagePack
  reconsidered only after Phase 1 lands and, at earliest, before Phase 2 binary
  reads.
- No changes to `pkg/board/remotefs` or any existing reader/writer code paths.

### Non-goals (explicit, to keep scope tight)

- Reads, writes, walk, stat, hash, search, doc, patches — all phase ≥2.
- TLS / auth — daemon already binds `127.0.0.1`; app-lab reaches it via SSH/ADB
  port-forward; transport security is provided by the tunnel.
- Reconnection / resumption — clients reopen the WS and re-subscribe.
- Separate process or binary.

### Module layout (inside `arduino-app-cli`)

```
internal/editor/
├── editor.go                 # public surface: Config + Handler()
├── doc.go                    # package doc + isolation rules
├── protocol/                 # wire types & codec, no I/O
│   ├── envelope.go           # JSON-RPC 2.0 shape
│   ├── methods.go            # method names, params, results
│   └── codec.go              # JSON encode/decode (single seam; swappable later)
├── server/                   # WS dispatch + session lifecycle
│   ├── session.go            # per-conn dispatch loop, subscription map
│   └── handler.go            # http.Handler that upgrades to WS
└── watcher/                  # fsnotify wrapper, coalesce, recursive walk
    ├── watcher.go
    ├── coalesce.go
    └── walk.go
```

### Dependencies

- `github.com/gorilla/websocket` — WS upgrade and frame I/O (already in `go.mod`).
- `github.com/sourcegraph/jsonrpc2` — JSON-RPC 2.0 dispatcher, id correlation,
  notifications, error codes. Avoids hand-rolling the RPC library.
- `github.com/fsnotify/fsnotify` — inotify/kqueue (already in `go.mod`).

MessagePack (`github.com/vmihailenco/msgpack/v5`) is intentionally **not** taken
as a dependency in Phase 1.

### Public surface (the only thing the daemon imports)

```go
package editor

type Config struct {
    Root       string         // canonicalized; paths outside are refused
    MaxWatches int            // default 1024
    Logger     *slog.Logger   // required
}

// New returns an http.Handler that upgrades to WebSocket and runs one editor
// session per connection. The handler is safe to mount under any route on the
// daemon's existing HTTP router.
func New(cfg Config) (http.Handler, error)
```

That is the _entire_ exported surface. The daemon's wiring file (~15 lines)
mounts the handler at e.g. `/v1/edit` and never touches the rest of the package.

### Wire details (phase 1)

- WS path: configured by the daemon when mounting (suggested: `/v1/edit`).
- Each WS text frame is one JSON-encoded JSON-RPC 2.0 message.
- Notifications use `{ jsonrpc: "2.0", method, params }` (no `id`).
- Codec revisit trigger: measure a representative `fs.read` (Phase 2) over the
  SSH/ADB-tunneled WS with `permessage-deflate` before committing to MessagePack.

### Server behaviour

- One `fsnotify.Watcher` per subscription; enforce `MaxWatches`.
- Recursive subscriptions: walk once at subscribe time and add each directory; on
  `CREATE` of a dir, walk and add it. On `REMOVE` of a watched dir, drop it.
- Coalesce events over a ~50 ms window per subscription; collapse `CREATE`+`WRITE`
  to a single `create`, multiple `WRITE`s to a single `update`.
- Respect `includes`/`excludes` globs server-side.
- Refuse any path that escapes `Root` after canonicalization.
- Follow symlinks transparently on all filesystem operations (read/walk/watch/stat);
  the client never sees a symlink as a distinct entity. The `Root` boundary is
  enforced on the resolved target — a symlink whose target lands outside `Root`
  is refused with the same "path escapes Root" error. Recursive traversals must
  detect symlink cycles (track visited canonical paths) to avoid infinite loops.
- Send WebSocket `Ping` frames every ~30 s and close the connection if no
  `Pong` arrives within ~10 s, so dead peers (crashed clients, dropped
  tunnels, NAT rebinds) release their subscriptions and inotify FDs promptly
  instead of surviving the OS-default TCP keepalive window.
- On WS close: cancel all subscriptions, release watchers.

### Integration with the daemon

A single new file under `cmd/arduino-app-cli/daemon/` (e.g. `editor.go`) builds
the editor `Handler` and mounts it on the existing HTTP router. No new listener,
no discovery endpoint, no token plumbing.

### Acceptance criteria (phase 1)

1. Client opens a WS to the daemon, calls `hello`, gets back capabilities
   including `fs.watch`.
2. Client calls `fs.watch` with `recursive: true`.
3. Editing a file via terminal/`sed` on the board produces a single coalesced
   `fs.changed` notification (`type: "update"`) within ~100 ms.
4. Create / update / delete / rename all surface as the correct event types.
5. Closing the WS releases all inotify FDs (verifiable via `/proc/<pid>/fd`).
6. `excludes` like `node_modules/**` filters events server-side.
7. Paths outside `Root` are refused with a JSON-RPC error.
8. `pkg/board/remotefs` works unchanged.

### Implementation notes (phase 1)

Non-normative guidance for the implementer; not part of the wire contract.

- **Recursive-watch race.** When the watcher observes `CREATE` of a directory
  and installs a new inotify watch on it, any files created inside between the
  `CREATE` firing and the `inotify_add_watch` returning will not produce
  events (inotify only reports events _after_ the watch is installed).
  Mitigation: after installing the watch on the new directory, `readdir` it
  and synthesize `create` events for whatever is already there. The
  coalescer's dedup window absorbs any overlap with events that _did_ arrive
  via inotify.
- **Symlink cycle detection.** Recursive walks (initial subscription setup,
  future `fs.walk`) must track the set of already-visited canonical paths and
  skip on re-entry, otherwise a loop like `a → ../a` will spin forever. Use
  `filepath.EvalSymlinks` (or an equivalent) once per directory and compare
  against the visited set.
- **`Root` boundary vs. symlinks.** Canonicalize the requested path (resolving
  symlinks) before every filesystem operation and confirm the result is still
  under the canonical `Root`. This must be done on _every_ call, not just at
  subscription time, because a symlink inside `Root` can be repointed by a
  concurrent shell.

---

## Phase 2 — Reads and metadata (no writes yet)

- Add `fs.stat`, `fs.walk`, `fs.read`, `fs.readStream.*`.
- Add `fs.hash` for sync diffing.
- Still no changes to `pkg/board/remotefs` — clients can choose old vs. new path.
- Once app-lab uses these, measure: latency, bandwidth, RTT count.

### ETag semantics

`etag` is an **opaque, short version token** the server returns with every
`fs.stat`/`fs.read` and that the client can pass back on the next operation to
detect concurrent changes. Same idea as HTTP's `ETag` + `If-Match`/`If-None-Match`.

**Purpose (why it's in the API at all).** The primary concurrent-editing story
lives in Phase 4 via `doc.open`/`doc.patch`/`version`: two clients editing the
same open document is fully covered there. Etags exist to protect the _other_
paths — quick sequential `fs.write`s that don't go through `doc.open` (save-as,
revert, "new file", tree operations, external scripts) — from silent
last-write-wins when two developers share a board over the network.

**Contract.**

- **Opaque to the client.** Clients MUST NOT parse or interpret the value; they
  only compare for equality.
- **Stable per unchanged content.** Two reads of the same unchanged file return
  the same etag; any content-affecting write changes it.
- **Optional on writes.** `fs.write.baseEtag` is optional. Omitted → last-write-wins
  (current behavior). Present → server compares against the current etag and
  rejects on mismatch with a JSON-RPC error carrying `data.currentEtag`. Clients
  that don't care can ignore the whole mechanism; clients that do opt in per-call.
- **Optional on reads.** `fs.read.baseEtag` is optional. Present and still
  matching → server returns `{ notModified: true, etag }` and skips the payload.

**Server implementation (recommended, cheap).**

The etag is built from `stat` fields the server already has — **no hashing, no
extra I/O**:

```go
// pseudo-Go
func etagOf(fi os.FileInfo) string {
    sys := fi.Sys().(*syscall.Stat_t)
    // Include ctime alongside mtime so metadata-only changes (rename, chmod)
    // and same-second delete+recreate still produce a distinct etag on
    // filesystems where mtime granularity is coarse.
    ctimeNs := sys.Ctim.Sec*1e9 + sys.Ctim.Nsec
    return fmt.Sprintf("%x-%x-%x-%x", sys.Ino, fi.ModTime().UnixNano(), ctimeNs, fi.Size())
}
```

Notes on precision:

- On Linux with ext4/xfs the tuple resolves at nanosecond granularity, so any
  single change flips at least one component.
- On filesystems with coarse `mtime` (e.g. FAT, some network mounts), including
  `ctime` still catches metadata updates within the same second. A pathological
  "delete + recreate with identical size and timings" is theoretically possible
  but requires the OS to reuse the inode _and_ preserve `ctime` — unlikely, and
  clients that need absolute certainty call `fs.hash`.
- `size` remains in the tuple as a cheap tiebreaker for filesystems that lack
  either timestamp field entirely.

Cost breakdown:

- `fs.stat`, `fs.read`: one extra `Sprintf` on a `stat` result the server was
  already computing. Nanoseconds, no I/O.
- `fs.write` with `baseEtag`: one extra `stat` syscall before writing to compare.
  A few microseconds; no data I/O.
- Wire overhead: ~30 bytes of string per response.

Clients that need **strong content equality** (sync diffing, rsync-style pulls)
call `fs.hash` explicitly — that is where SHA-256 lives, opt-in and never
computed as part of a normal read.

### Text-first payload encoding

Editor workloads are heavily text-skewed (`.ino`, `.h/.cpp`, `.py`, `.js/.ts`,
`.json`, `.yaml`, `.md`, configs). Rather than pay base64's ~33% overhead on
every read/write, `fs.read` and `fs.write` return/accept a small discriminated
union so text files ride the JSON wire natively and only binaries are base64'd:

```jsonc
// text file
{ "encoding": "utf-8",  "content": "void setup() { ... }", "etag": "...", "size": 123 }

// binary file (image, font, compiled artifact)
{ "encoding": "base64", "content": "iVBORw0KGgo...",        "etag": "...", "size": 8421 }
```

Server-side selection (`encoding` param on `fs.read` defaults to `"auto"`):

1. If the client passes `encoding: "utf-8"` or `"base64"` explicitly, honor it
   (error if the bytes are not valid UTF-8 when `utf-8` is forced).
2. Otherwise, extension allowlist (`.ino .h .hpp .c .cpp .py .js .ts .tsx .json
   .yaml .yml .md .txt .cfg .conf .ini .toml .sh .html .css .xml .svg …`) →
   `utf-8`.
3. Otherwise sniff the first ~512 bytes: valid UTF-8 with no NUL byte →
   `utf-8`; else `base64`.

`fs.write` mirrors the same shape: `{ path, encoding, content, baseEtag?, atomic? }`.

Rationale: with this scheme the base64 tax is paid only on the minority of
files that are actually binary, which are also rarely edited. Combined with
`permessage-deflate` on the SSH-tunneled WS, this removes msgpack's main
payload-efficiency argument for the editor use case. Revisit the codec choice
only if profiling on real boards shows JSON is the bottleneck.

## Phase 3 — Writes

- Add `fs.write`, `fs.writeStream.*`, `fs.delete`, `fs.move`, `fs.mkdir`.
- All writes require `baseEtag` for optimistic concurrency.
- Support atomic write (write tmp + rename).

## Phase 4 — Incremental document editing

- Add `doc.open`, `doc.patch`, `doc.close`, `doc.changed`.
- Server holds the open-document state, applies LSP-style content changes, persists
  with debounce. This is where CodeMirror's `ChangeSet` adapter pays off.
- Resolve conflicts using the watcher (`origin: "external"` events trigger a merge
  flow in the client).

## Phase 5 — Search and quality-of-life

- `search.*` powered by ripgrep on the board (cross-compile or rely on system pkg).
- Reconnection / resumption IDs for resilient sessions over flaky links (opaque
  correlation IDs only — still no auth; the transport remains the trust boundary).
- Optional `permessage-deflate` for text-heavy traffic; off by default.

## Phase 6 — Migrate `pkg/board/remotefs`

- Once the service owns reads/writes, the host-side `remotefs` can become a thin
  client of this service over the SSH-tunneled WS (replacing per-call SFTP/ADB).
- Old SFTP/ADB FS backends kept as fallback for boards without the service.

---

## Risks / open questions

- **Inotify watch limit** on embedded kernels (`/proc/sys/fs/inotify/max_user_watches`).
  Mitigation: expose `maxWatches` in `limits`, fail loud, document raising the sysctl.
- **Binary size** on space-constrained boards. Strip + UPX if needed; consider TinyGo
  later if it ever supports `fsnotify` cleanly (currently does not).
- **Module home**: confirm with the team where the repo should live and under which
  license (Apache-2.0 recommended for genuine reusability).

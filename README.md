# go-cache-lru

A [GOCACHEPROG](https://pkg.go.dev/cmd/go/internal/cacheprog) implementation
that keeps the Go build cache under a fixed size.

The go command trims its own cache only by age: entries unused for five days
are deleted, and there is no size limit. With many git worktrees, constant
test runs and builds, the cache grows by tens of gigabytes a day.
`-trimpath` helps, because it takes the worktree path out of cache keys, but
it does not bound the size.

go-cache-lru runs one daemon per cache directory. Every go command talks to
it, and it evicts least recently used entries once the cache grows past the
limit.

## Features

- **Size limit with LRU eviction.** Above `--max-size` (30 GiB by default)
  the daemon removes the least recently used entries until the cache is down
  to `--target-size` (25 GiB). "Used" means the last get or put; the order
  survives daemon restarts through file modification times.
- **One backend for all go commands.** The first go command starts the
  daemon. Later ones, including parallel builds from different worktrees,
  connect to the same daemon over a unix socket. A file lock guarantees a
  single daemon per directory. Writes are content-addressed and atomic
  (temp file + rename), so concurrent puts of the same object are safe.
- **Files in use are never deleted.** The protocol requires a returned file
  to exist until the go command closes the session. Every object handed to a
  session stays pinned until then; eviction and `clean` skip pinned objects
  and finish the job when the session ends.
- **Statistics.** Hits, misses, bytes served and written, evictions, cleans,
  current size, entry and object counts, remote tier counters. Counters
  persist across restarts.
- **`go clean -cache` works.** See [below](#go-clean--cache).
- **Graceful shutdown.** `SIGTERM`, `SIGINT` or `go-cache-lru stop` stop
  accepting connections, wait for running go commands (`--shutdown-timeout`,
  1 minute by default), flush queued uploads and save statistics. Ctrl-C on a
  build does not reach the daemon: it runs in its own session.
- **Pluggable remote storage.** Optional second tier behind the local disk:
  `file://` for a directory such as a network share, `s3://` for AWS S3,
  Cloudflare R2, MinIO and other S3-compatible services.

## Install

Prebuilt binaries for macOS and Linux (amd64, arm64) are on the
[releases page](https://github.com/juev/go-cache-lru/releases). Or build from
source:

```sh
go install github.com/juev/go-cache-lru/cmd/go-cache-lru@latest
```

Building from source needs Go 1.26 or newer (required by the SQLite
driver). The go command that uses the cache needs 1.24 or newer: GOCACHEPROG
is available without `GOEXPERIMENT` since 1.24. Windows is not supported
yet.

## Use

Point `GOCACHEPROG` at the binary. Flags go into the same variable:

```sh
export GOCACHEPROG="$HOME/go/bin/go-cache-lru \
  --dir=$HOME/.cache/go-lru \
  --max-size=30GiB \
  --target-size=25GiB"
```

fish:

```fish
set -gx GOCACHEPROG "$HOME/go/bin/go-cache-lru --dir=$HOME/.cache/go-lru --max-size=30GiB --target-size=25GiB"
```

Keep `GOFLAGS=-trimpath` as well: without it the worktree path is part of
every cache key, and each worktree fills the cache with its own copies.

The go command starts the daemon on first use. Its log is
`<dir>/gocacheprog/daemon.log`.

Without `--dir` the cache lives in `GOCACHE`. A separate directory is
cleaner: the go command still opens `GOCACHE` for the fuzzing corpus and
test-cache expiry, and leftovers of the go command's own cache there do not
mix with go-cache-lru's files. Remove those leftovers once with
`go clean -cache` before switching.

Tools started outside the shell (an IDE launched from the Dock) do not see
the variable and keep using the go command's own cache.

### Flags

Each flag defaults to an environment variable, and a flag wins over its
variable.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--dir` | `GOCACHELRU_DIR` | `GOCACHE` | cache directory |
| `--max-size` | `GOCACHELRU_MAX_SIZE` | `30GiB` | eviction starts above this |
| `--target-size` | `GOCACHELRU_TARGET_SIZE` | `25GiB` | eviction shrinks the cache to this |
| `--remote` | `GOCACHELRU_REMOTE` | none | remote storage URL |
| `--socket` | `GOCACHELRU_SOCKET` | `<dir>/gocacheprog/daemon.sock` | daemon socket |
| `--idle-timeout` | `GOCACHELRU_IDLE_TIMEOUT` | `0` (never) | daemon exits after this long without go commands |
| `--shutdown-timeout` | `GOCACHELRU_SHUTDOWN_TIMEOUT` | `1m` | how long shutdown waits for running go commands |

Sizes accept `K`, `M`, `G`, `T` and `KiB`…`TiB` (binary) or `KB`…`TB`
(decimal). The size counts file system blocks (4 KiB), so it is close to
what `du` reports.

The flags are part of the daemon's identity only through `--dir` and
`--socket`: a daemon keeps the limits it was started with. After changing
them, run `go-cache-lru stop` and the next go command starts a daemon with
the new values.

### Commands

Control commands need the same `--dir` (or `--socket`) as `GOCACHEPROG`.

```sh
go-cache-lru --dir=$HOME/.cache/go-lru stats         # human-readable
go-cache-lru --dir=$HOME/.cache/go-lru stats -json   # for scripts
go-cache-lru --dir=$HOME/.cache/go-lru stats -reset  # zero the counters
go-cache-lru --dir=$HOME/.cache/go-lru trim          # shrink to --target-size now
go-cache-lru --dir=$HOME/.cache/go-lru clean         # drop everything not in use
go-cache-lru --dir=$HOME/.cache/go-lru stop          # graceful shutdown
go-cache-lru --dir=$HOME/.cache/go-lru serve         # run the daemon in the foreground
```

Upgrading the binary: `go install` the new version, then `go-cache-lru stop`.

### go clean -cache

`go clean -cache` does not start GOCACHEPROG. It deletes the two-hex-digit
subdirectories of `GOCACHE`. go-cache-lru keeps a sentinel file in one of
them and checks it every two seconds:

- with the cache in `GOCACHE`, the clean has already removed its files, and
  the daemon drops the index entries whose objects are gone;
- with a separate `--dir`, the daemon removes every entry not in use.

`go clean -testcache` works unchanged: entries carry their put time, which
the go command compares with its expiry mark.

## Remote storage

The go command reads cache objects from local files, so the local directory
is always the primary store, and remote storage is a second tier. On a local
miss the daemon asks the remote (10 s timeout) and copies the object to disk.
After a local put it uploads the object in the background. When the upload
queue is full, it skips the object and counts a remote error.

```sh
# directory, e.g. a network share
--remote=file:///Volumes/share/go-cache

# AWS S3
--remote='s3://my-bucket/go-cache?region=eu-central-1'

# Cloudflare R2
--remote='s3://my-bucket/go-cache?endpoint=<account-id>.r2.cloudflarestorage.com&region=auto'

# MinIO without TLS
--remote='s3://go-cache?endpoint=localhost:9000&insecure=1'
```

S3 credentials come from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, the
shared credentials file or instance metadata. go-cache-lru never deletes
remote objects; use a bucket lifecycle rule to expire them. Empty objects are
not uploaded.

A new driver implements `storage.Backend` (`Get`, `Put`, `Close`) and
registers a URL scheme with `storage.Register`; `internal/storage/storagetest`
checks the expected behavior.

## Design

```
go build ──stdin/stdout──▶ go-cache-lru ──unix socket──┐
go test  ──stdin/stdout──▶ go-cache-lru ──unix socket──┼──▶ daemon ──▶ local dir
go vet   ──stdin/stdout──▶ go-cache-lru ──unix socket──┘      │      (LRU, pins)
                                                              └────▶ remote (optional)
```

Each go command starts `go-cache-lru` as its GOCACHEPROG. That process only
relays bytes between the go command and a daemon session. The daemon:

- owns the directory through an exclusive `flock` on
  `<dir>/gocacheprog/daemon.lock`; the lock dies with the process, so a
  crash never blocks the next start;
- stores an object once per output ID (`xx/<output>-pd`), so identical
  outputs of different actions share one file; the suffix differs from the
  go command's own `-a`/`-d` files, so its age-based trimming never touches
  them;
- verifies that each body matches its output ID (SHA-256) before storing it;
- keeps the LRU index in memory and persists it in SQLite
  (`<dir>/gocacheprog/index.db`, see [below](#index-in-sqlite));
- writes access times to the index at most once a minute per entry;
- evicts in batches of 256 entries per lock hold, in the background, right
  after the put that crossed the limit and again when a session releases its
  pins.

Clients that start at once take `<dir>/gocacheprog/spawn.lock`, so a
parallel build starts one daemon, not one per go command. A daemon that is
still draining holds the lock, and its successor waits for it
(`serve --lock-wait`, by default the shutdown timeout plus 15 s), so a go
command started during a shutdown waits instead of failing.

When the shutdown timeout expires, the daemon closes the remaining sessions.
Their go commands then fail with "GOCACHEPROG exited pre-Close", so set
`--shutdown-timeout` longer than the longest test run if that matters.

### Index in SQLite

The go command reads objects from the paths the daemon returns, so objects
are files. Only the index lives in SQLite (pure-Go `modernc.org/sqlite`, no
cgo): one row per action ID with the output ID, size, put time and last
access time.

The cache never waits for the database. Get and put work on the in-memory
index under one mutex, as before. Changes go to a queue in order, and a
background goroutine writes them in one transaction every 100 ms. Shutdown
writes what is left.

The database and the object files can disagree: `go clean -cache` deletes
files but not rows, and a crash loses up to 100 ms of queued changes. So
every start, and every detected `go clean -cache`, reconciles the two. The
daemon lists object files by name (`ReadDir`, no per-file open or stat),
deletes rows whose object is missing and deletes objects that no row
references. A crash therefore costs at most the last 100 ms of cache
entries, never consistency.

Index entry files of the earlier file-based index (`xx/<action>-pa`) are
imported into the database on the first start and removed.

### Measurements

Measured on 2026-10-27 on an Apple Silicon Mac (12 cores, APFS) with
Go 1.27.0, comparing the SQLite index with the earlier file-based index (one
`-pa` file per entry, its mtime as access time).

**Startup.** `TestOpenScale` in `internal/cache` generates a synthetic cache
and measures `Open`:

```sh
GOCACHELRU_SCALE=300000 go test -run TestOpenScale -v ./internal/cache
```

300,000 index entries over 150,000 objects. This is roughly a full 30 GiB
cache: the first real statistics showed 177.9 MiB in 875 objects (about
200 KiB each) and two index entries per object.

| | File-based index | SQLite index |
|---|---|---|
| `Open`, warm page cache | 6.6–15.4 s over 6 runs | 0.55–0.57 s over 9 runs |
| Memory for the index | about 150 MiB of heap | about 100 MiB of heap |
| On disk for the index | 300,000 files | 54 MB `index.db` |
| Binary size | 11.6 MiB | 17.4 MiB |

With files, 97% of startup went to system calls (CPU profile), 84% to
reading index entries one file at a time, and the time followed the load on
the machine. A cold page cache, right after boot, was measured for neither
(`purge` needs root).

Importing a file-based index of this size took 42 s once. A cache that was
cleaned with `go clean -cache` before switching has nothing to import.

**Hot path.** A parallel benchmark on the cache package (12 goroutines, 4 KiB
objects, 3 runs of 3 s each):

| | File-based index | SQLite index |
|---|---|---|
| Put | 251–265 µs/op | 110–117 µs/op |
| Get, hit | 4.5–4.9 µs/op | 1.43–1.47 µs/op |

A put no longer writes and renames an index file under the lock, and a get
no longer updates a file's mtime.

**Size accounting** counts only objects, rounded up to 4 KiB blocks. The
file-based index also counted 4 KiB per index file; the database is not
counted (54 MB at 300,000 entries).

## Other implementations

Checked in October 2026; none of them bounds the local cache.

- [tailscale/go-cache-plugin](https://github.com/tailscale/go-cache-plugin) —
  S3-backed, with a server mode reachable over a local socket. The README
  describes no size limit, eviction or pinning.
- [platacard/cacheprog](https://github.com/platacard/cacheprog) — S3/MinIO
  and HTTP backends, zstd compression, metrics push. Its README says it
  "does not perform any kind of garbage collection on disk storage".
- [richardartoul/gobuildcache](https://github.com/richardartoul/gobuildcache) —
  S3 Express One Zone, GCS and Azure backends with per-file locks. Its README
  says it "performs zero automatic GC or trimming of the local filesystem
  cache or the remote cache backend".
- [bradfitz/go-tool-cache](https://github.com/bradfitz/go-tool-cache) — the
  original demonstration of GOCACHEPROG for sharing the cache over the
  network; it now "just contains some misc examples".

These tools target CI, where the cache directory lives for one job. A
developer machine needs the opposite: a long-lived local cache under a hard
size limit.

## Development

```sh
go test -race ./...        # includes end-to-end tests with the real go command
go test -short ./...       # unit tests only
```

## License

[MIT](LICENSE)

# go-cache-lru

[![Release](https://img.shields.io/github/v/release/juev/go-cache-lru)](https://github.com/juev/go-cache-lru/releases)
[![CI](https://github.com/juev/go-cache-lru/actions/workflows/ci.yml/badge.svg)](https://github.com/juev/go-cache-lru/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/juev/go-cache-lru)](https://goreportcard.com/report/github.com/juev/go-cache-lru)
[![Go version](https://img.shields.io/github/go-mod/go-version/juev/go-cache-lru)](go.mod)
[![License](https://img.shields.io/github/license/juev/go-cache-lru)](LICENSE)

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

## Quick start

```sh
go install github.com/juev/go-cache-lru/cmd/go-cache-lru@latest
export GOCACHEPROG="$(go env GOPATH)/bin/go-cache-lru --dir=$HOME/.cache/go-lru --max-size=30GiB --target-size=25GiB"
go build ./...                                  # starts the daemon on first use
go-cache-lru stats                              # hits, misses, size, evictions
```

See [Use](#use) for fish, prebuilt binaries and all flags.

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
  persist across restarts. `stats -v` adds what occupies the cache: objects
  by size and by time since last use.
- **Hourly history and size advice.** The daemon keeps the counters per hour
  for 7 days. `stats` shows the last hour, 24 hours and 7 days and a hint
  about `--max-size`. See [below](#hourly-history-and-size-advice).
- **`go clean -cache` works.** See [below](#go-clean--cache).
- **Graceful shutdown.** `SIGTERM`, `SIGINT` or `go-cache-lru stop` stop
  accepting connections, wait for running go commands (`--shutdown-timeout`,
  1 minute by default), flush queued uploads and save statistics. Ctrl-C on a
  build does not reach the daemon: it runs in its own session.
- **Picks up upgrades.** A daemon whose binary was replaced on disk keeps
  serving and exits at the first moment no go command is connected, so the
  next one runs the new version and no go command waits for the switch.
- **Pluggable remote storage.** Optional second tier behind the local disk:
  `file://` for a directory such as a network share, `s3://` for AWS S3,
  Cloudflare R2, MinIO and other S3-compatible services.

## Install

Prebuilt binaries for macOS, Linux and Windows (amd64, arm64) are on the
[releases page](https://github.com/juev/go-cache-lru/releases). Or build from
source:

```sh
go install github.com/juev/go-cache-lru/cmd/go-cache-lru@latest
```

Building from source needs Go 1.26 or newer (required by the SQLite
driver). The go command that uses the cache needs 1.24 or newer: GOCACHEPROG
is available without `GOEXPERIMENT` since 1.24. On Windows the daemon
listens on a unix socket, which Windows supports since Windows 10 version
1803.

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
the variable and keep using the go command's own cache. `go env -w` stores
the setting where every go command finds it, whatever started it:

```sh
go env -w GOCACHEPROG="$HOME/go/bin/go-cache-lru --dir=$HOME/.cache/go-lru --max-size=30GiB --target-size=25GiB"
```

A variable in the environment still wins over `go env -w`.

Windows (PowerShell). Put a path with spaces in quotes inside the value:

```powershell
go env -w GOCACHEPROG="'$env:USERPROFILE\go\bin\go-cache-lru.exe' --dir='$env:LOCALAPPDATA\go-lru' --max-size=30GiB --target-size=25GiB"
```

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
| `--shutdown-timeout` | `GOCACHELRU_SHUTDOWN_TIMEOUT` | `1m` | how long shutdown waits for running go commands; after an upgrade, also how long the old daemon waits for a moment without them |

Sizes accept `K`, `M`, `G`, `T` and `KiB`…`TiB` (binary) or `KB`…`TB`
(decimal). The size counts file system blocks (4 KiB), so it is close to
what `du` reports.

The flags are part of the daemon's identity only through `--dir` and
`--socket`: a daemon keeps the limits it was started with. After changing
them, run `go-cache-lru stop` and the next go command starts a daemon with
the new values.

### Commands

Control commands talk to the daemon that the go commands use. Without
`--dir` or `--socket` on the command line or in `GOCACHELRU_DIR` /
`GOCACHELRU_SOCKET`, `stats`, `trim`, `clean` and `stop` take the flags from
`GOCACHEPROG` (the environment, or `go env -w`) when it runs go-cache-lru.
Flags given on the command line win.

```sh
go-cache-lru stats                                   # human-readable
go-cache-lru stats -v                                # plus objects by size, last access, hourly table
go-cache-lru stats -json                             # for scripts: History, Advice (-v adds Breakdown)
go-cache-lru stats -reset                            # zero the counters
go-cache-lru trim                                    # shrink to --target-size now
go-cache-lru clean                                   # drop everything not in use
go-cache-lru stop                                    # graceful shutdown
go-cache-lru --dir=$HOME/.cache/go-lru serve         # run the daemon in the foreground
go-cache-lru --version                               # version of the binary
```

`stats -v` shows where the space goes, which helps to choose the limits.
Output from a workstation with a 30 GiB limit, a day after the counters
were started, while a go command was running:

```
daemon          pid 31187, version v0.2.1-0.20261028125138-c819f9224beb c819f9224beb, up 16m47s, 1 active sessions
directory       /Users/me/.cache/go-lru
size            27.2 GiB of 30.0 GiB (target 25.0 GiB), 90.5%
entries         71159 actions, 18843 objects, 1707 pinned
counters since  2026-10-27 16:38:51
hits            1729571 (96.8%), 365.7 GiB served
misses          56864
puts            104025, 48.7 GiB written
evictions       19209 entries, 20.3 GiB
cleans          0 explicit, 1 by go clean -cache
sessions        1092
this hour       hit rate 98.3%, 110634 gets, 1.2 GiB written, 0 evictions
last 24 hours   hit rate 97.5%, 318152 gets, 6.9 GiB written, 1 evictions
last 7 days     hit rate 97.5%, 318152 gets, 6.9 GiB written, 1 evictions
advice          not enough history yet: 2 of 24 active hours

objects by size
  < 64 KiB               9706    69.9 MiB    0.3%
  64 KiB - 1 MiB         6482     2.5 GiB    9.2%
  1 MiB - 10 MiB         2214     5.1 GiB   18.8%
  10 MiB - 100 MiB        439    19.2 GiB   70.8%
  >= 100 MiB                2   253.8 MiB    0.9%

objects by last access
  < 1h                  14033    16.9 GiB   62.4%
  1h - 6h                4810    10.2 GiB   37.6%
  6h - 24h                  0         0 B    0.0%
  >= 24h                    0         0 B    0.0%

hourly, last 24 hours
  hour       gets  hit rate     written evictions   peak size
  15:00    207518     97.2%     5.8 GiB         1    30.0 GiB
  16:00    110634     98.3%     1.2 GiB         0    27.2 GiB
```

How to read it:

- `up 16m47s` with counters since the day before: the daemon was restarted
  after an upgrade. Counters, the index and the history persist across
  restarts.
- `hits` and `puts`: 96.8% of lookups hit, and the cache served 365.7 GiB
  while 48.7 GiB was written to it.
- `size` and `evictions`: the cache reached `--max-size` and was trimmed to
  `--target-size`; since the counters started, 19 209 entries (20.3 GiB)
  were evicted.
- `pinned`: objects handed to the running go command. Eviction skips them
  until its session ends.
- `cleans`: the daemon noticed one `go clean -cache` and removed every
  entry not in use (see [go clean -cache](#go-clean--cache)).
- In the period lines and the hourly table, `evictions` counts eviction runs,
  not entries. The single run at 15:00 matches the 30.0 GiB peak size.
- `objects by size`: 441 objects of 10 MiB and more hold 71.7% of the space,
  so they decide how much fits under the limit.
- `objects by last access`: everything left was used in the last 6 hours;
  older objects were evicted or removed by the clean. If builds of older
  branches should still hit, raise `--max-size`.
- `advice`: the history needs 24 hours with go commands before it suggests
  a limit.

### Hourly history and size advice

The cumulative counters hide how the cache behaves now: a hit rate since the
first start includes the warm-up. The daemon also keeps the change of the
counters per hour, with the peak size of each hour, for 7 days. It writes
them together with `stats.json`, when go commands start and finish and every
30 s while they run, so an idle daemon still does not wake up. `stats`
prints three periods and the advice:

```
this hour       hit rate 60.6%, 1413 gets, 198.1 MiB written, 0 evictions
last 24 hours   hit rate 60.6%, 1413 gets, 198.1 MiB written, 0 evictions
last 7 days     hit rate 60.6%, 1413 gets, 198.1 MiB written, 0 evictions
advice          not enough history yet: 1 of 24 active hours
```

`stats -v` adds a table of the last 24 hours with gets, hit rate, bytes
written, evictions and peak size per hour.

The advice is a hint, not a measurement. It looks at the last 7 days of
hours with go commands:

- with fewer than 24 such hours it waits for more data;
- if the cache never evicted and its peak stayed under 60% of `--max-size`,
  it suggests a limit of the peak plus 25%;
- if the cache evicted, it compares the miss rate of the hour after each
  eviction with the usual miss rate. When misses after at least 3 evictions
  are 1.5 times the usual rate or more, it suggests a 50% larger limit.

Code changes and branch switches cause misses too, so the advice prints the
numbers it is based on. The suggested `--target-size` keeps its current ratio
to `--max-size`.

Upgrading the binary: `go install` the new version, or write it next to the
old one and rename it into place. Do not overwrite the running file in place:
macOS may kill a process whose binary changed under it. The daemon checks its
binary whenever a go command or a control command connects. After it notices
the new binary, the old daemon keeps accepting go commands and exits as soon
as none is connected; the next go command starts the new binary. Shutting
down at once would make every go command started meanwhile wait: the new
daemon cannot open the cache until the old one has drained. If go commands
keep running for `--shutdown-timeout`, the old daemon stops accepting and
drains as on `stop`. `go-cache-lru stop` switches at once.

Windows does not let a running `.exe` be replaced, but lets it be renamed.
Rename `go-cache-lru.exe` to, for example, `go-cache-lru.exe.old`, put the
new binary in its place, and delete the old file once the old daemon has
exited.

### go clean -cache

`go clean -cache` does not start GOCACHEPROG. It deletes the two-hex-digit
subdirectories of `GOCACHE`. go-cache-lru keeps a sentinel file in one of
them and checks it when a go command or a control command connects, before
serving it, and every two seconds while go commands run:

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

# directory on Windows
--remote=file:///D:/share/go-cache

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

- owns the directory through an exclusive lock (`flock`, `LockFileEx` on
  Windows) on `<dir>/gocacheprog/daemon.lock`; the lock dies with the
  process, so a crash never blocks the next start;
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
index under one mutex, as before. Changes go to a queue in order. The
first change in an empty queue wakes a background goroutine, which waits
100 ms for the rest of the burst and writes the batch in one transaction.
Shutdown writes what is left.

An idle daemon does not wake up: the index goroutine sleeps until a change
arrives, the `go clean -cache` check and the periodic saving of statistics
run only while go commands are active, and statistics are written only
when they changed.

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

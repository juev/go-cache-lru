// Command go-cache-lru is a GOCACHEPROG implementation with a size-bounded
// LRU cache shared by all go commands through a background daemon.
//
// Usage:
//
//	export GOCACHEPROG="go-cache-lru --dir=$HOME/.cache/go-lru --max-size=30GiB --target-size=25GiB"
//	go-cache-lru [flags] serve|stats|trim|clean|stop|version
//
// Flags default to GOCACHELRU_* environment variables; run with -h for the
// list.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juev/go-cache-lru/internal/cache"
	"github.com/juev/go-cache-lru/internal/client"
	"github.com/juev/go-cache-lru/internal/config"
	"github.com/juev/go-cache-lru/internal/protocol"
	"github.com/juev/go-cache-lru/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "go-cache-lru: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("go-cache-lru", flag.ContinueOnError)
	fs.Usage = func() { usage(fs.Output(), fs) }
	flags := config.Register(fs)
	o := new(cmdOpts)
	fs.DurationVar(&o.lockWait, "lock-wait", 0, "serve: how long to wait for a previous daemon to exit (default: shutdown timeout + 15s)")
	fs.BoolVar(&o.json, "json", false, "stats: print JSON")
	fs.BoolVar(&o.reset, "reset", false, "stats: reset counters")
	fs.BoolVar(&o.verbose, "v", false, "stats: also show objects by size and by last access")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")

	// Flags may come before and after the command.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if o.version {
		fmt.Println(server.Version())
		return nil
	}
	cmd := fs.Arg(0)
	if fs.NArg() > 1 {
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected arguments: %q", fs.Args())
		}
	}
	switch cmd {
	case "version":
		fmt.Println(server.Version())
		return nil
	case "help":
		usage(os.Stdout, fs)
		return nil
	case "stats", "trim", "clean", "stop":
		if err := adoptGoCacheProg(fs); err != nil {
			return err
		}
	}
	cfg, err := flags.Resolve()
	if err != nil {
		return err
	}
	switch cmd {
	case "":
		return client.Bridge(cfg, os.Stdin, os.Stdout)
	case "serve":
		return serve(cfg, o)
	case "stats":
		return stats(cfg, o)
	case "trim":
		return cleanCmd(cfg, protocol.CmdTrim)
	case "clean":
		return cleanCmd(cfg, protocol.CmdClean)
	case "stop":
		return stop(cfg)
	default:
		usage(os.Stderr, fs)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// adoptGoCacheProg lets a control command find the daemon that go commands
// use. Without --dir or --socket on the command line or in the environment,
// the flags that GOCACHEPROG passes to go-cache-lru fill in those not given
// on the command line.
func adoptGoCacheProg(fs *flag.FlagSet) error {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["dir"] || set["socket"] || os.Getenv(config.EnvDir) != "" || os.Getenv(config.EnvSocket) != "" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	args := config.ProgArgs(self)
	if len(args) == 0 {
		return nil
	}
	pfs := flag.NewFlagSet("GOCACHEPROG", flag.ContinueOnError)
	pfs.SetOutput(io.Discard)
	config.Register(pfs)
	if err := pfs.Parse(args); err != nil {
		return fmt.Errorf("GOCACHEPROG: %w", err)
	}
	pfs.Visit(func(f *flag.Flag) {
		if !set[f.Name] {
			err = errors.Join(err, fs.Set(f.Name, f.Value.String()))
		}
	})
	if err != nil {
		return fmt.Errorf("GOCACHEPROG: %w", err)
	}
	return nil
}

type cmdOpts struct {
	lockWait                      time.Duration
	json, reset, verbose, version bool
}

func usage(w io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprint(w, `usage: go-cache-lru [flags] [command]

commands:
  (none)   relay a go command to the daemon; use as GOCACHEPROG
  serve    run the daemon in the foreground
  stats    show statistics (-json, -reset, -v)
  trim     shrink the cache to the target size now
  clean    remove all entries not in use
  stop     stop the daemon gracefully
  version  print the version

flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func serve(cfg config.Config, o *cmdOpts) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	lockWait := o.lockWait
	if lockWait == 0 {
		// A previous daemon may still be draining its sessions.
		lockWait = cfg.DrainTimeout + 15*time.Second
	}
	err := server.Run(ctx, cfg, lockWait, logger.Printf)
	if errors.Is(err, server.ErrAlreadyRunning) {
		logger.Printf("%v; exiting", err)
		return nil
	}
	return err
}

func stats(cfg config.Config, o *cmdOpts) error {
	conn, err := client.Dial(cfg)
	if err != nil {
		return fmt.Errorf("daemon is not running (socket %s)", cfg.Socket)
	}
	defer func() { _ = conn.Close() }()
	cmd := protocol.CmdStats
	switch {
	case o.reset:
		cmd = protocol.CmdStatsReset
	case o.verbose:
		cmd = protocol.CmdStatsFull
	}
	var st server.Status
	if err := client.Call(conn, cmd, &st); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") {
			return fmt.Errorf("%w: the daemon is older than this binary and restarts with the new one on the next go command", err)
		}
		return err
	}
	if o.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(os.Stdout, st)
	if st.Breakdown != nil {
		printBreakdown(os.Stdout, *st.Breakdown, st.Cache.Size)
		printHistory(os.Stdout, st.History)
	}
	return nil
}

func printStatus(w io.Writer, st server.Status) {
	c := st.Cache
	k := c.Counters
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	p := func(name, format string, args ...any) {
		_, _ = fmt.Fprintf(tw, "%s\t"+format+"\n", append([]any{name}, args...)...)
	}
	p("daemon", "pid %d, version %s, up %v, %d active sessions", st.PID, st.Version, time.Since(st.Started).Round(time.Second), st.ActiveSessions)
	p("directory", "%s", c.Dir)
	p("size", "%s of %s (target %s), %.1f%%", config.FormatSize(c.Size), config.FormatSize(c.MaxSize), config.FormatSize(c.TargetSize), 100*float64(c.Size)/float64(c.MaxSize))
	p("entries", "%d actions, %d objects, %d pinned", c.Entries, c.Objects, c.Pinned)
	p("counters since", "%s", k.Since.Format(time.DateTime))
	total := k.Hits + k.Misses
	rate := 0.0
	if total > 0 {
		rate = 100 * float64(k.Hits) / float64(total)
	}
	p("hits", "%d (%.1f%%), %s served", k.Hits, rate, config.FormatSize(k.HitBytes))
	p("misses", "%d", k.Misses)
	p("puts", "%d, %s written", k.Puts, config.FormatSize(k.PutBytes))
	p("evictions", "%d entries, %s", k.Evictions, config.FormatSize(k.EvictedBytes))
	p("cleans", "%d explicit, %d by go clean -cache", k.Cleans, k.ExternalCleans)
	if k.RemoteHits+k.RemoteMisses+k.RemotePuts+k.RemoteErrors > 0 {
		p("remote", "%d hits, %d misses, %d puts, %d errors", k.RemoteHits, k.RemoteMisses, k.RemotePuts, k.RemoteErrors)
	}
	p("sessions", "%d", k.Sessions)

	now := time.Now()
	hour := now.Truncate(time.Hour)
	windows := []struct {
		name string
		from time.Time
	}{
		{"this hour", hour},
		{"last 24 hours", hour.Add(-23 * time.Hour)},
		{"last 7 days", now.Add(-cache.HistoryWindow)},
	}
	for _, w := range windows {
		var sum cache.HourStats
		for _, h := range st.History {
			if !h.Hour.Before(w.from) {
				sum = sum.Add(h)
			}
		}
		p(w.name, "%s", periodLine(sum))
	}
	p("advice", "%s", st.Advice.Text)
}

func periodLine(h cache.HourStats) string {
	if h.Gets() == 0 && h.Puts == 0 {
		return "no go commands"
	}
	return fmt.Sprintf("hit rate %.1f%%, %d gets, %s written, %d evictions",
		100*(1-h.MissRate()), h.Gets(), config.FormatSize(h.PutBytes), h.EvictionRuns)
}

func printHistory(w io.Writer, hist []cache.HourStats) {
	from := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	_, _ = fmt.Fprintln(w, "\nhourly, last 24 hours")
	_, _ = fmt.Fprintf(w, "  %-6s %8s %9s %11s %9s %11s\n", "hour", "gets", "hit rate", "written", "evictions", "peak size")
	for _, h := range hist {
		if h.Hour.Before(from) {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %-6s %8d %8.1f%% %11s %9d %11s\n", h.Hour.Local().Format("15:04"), h.Gets(),
			100*(1-h.MissRate()), config.FormatSize(h.PutBytes), h.EvictionRuns, config.FormatSize(h.PeakSize))
	}
}

func printBreakdown(w io.Writer, d cache.Breakdown, total int64) {
	row := func(label string, objects int, bytes int64) {
		share := 0.0
		if total > 0 {
			share = 100 * float64(bytes) / float64(total)
		}
		_, _ = fmt.Fprintf(w, "  %-18s %8d %11s %6.1f%%\n", label, objects, config.FormatSize(bytes), share)
	}
	_, _ = fmt.Fprintln(w, "\nobjects by size")
	for i, b := range d.BySize {
		var lo int64
		if i > 0 {
			lo = d.BySize[i-1].Below
		}
		row(rangeLabel(i, sizeLabel(lo), sizeLabel(b.Below), b.Below == 0), b.Objects, b.Bytes)
	}
	_, _ = fmt.Fprintln(w, "\nobjects by last access")
	for i, b := range d.ByAccess {
		var lo time.Duration
		if i > 0 {
			lo = d.ByAccess[i-1].Below
		}
		row(rangeLabel(i, ageLabel(lo), ageLabel(b.Below), b.Below == 0), b.Objects, b.Bytes)
	}
}

// rangeLabel names bucket i: "< hi" for the first, ">= lo" for the open
// last one and "lo - hi" between.
func rangeLabel(i int, lo, hi string, open bool) string {
	switch {
	case open:
		return ">= " + lo
	case i == 0:
		return "< " + hi
	default:
		return lo + " - " + hi
	}
}

func sizeLabel(n int64) string {
	return strings.Replace(config.FormatSize(n), ".0 ", " ", 1)
}

// ageLabel prints whole hours as "6h" instead of "6h0m0s".
func ageLabel(d time.Duration) string {
	return strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
}

func cleanCmd(cfg config.Config, cmd protocol.Cmd) error {
	conn, err := client.Connect(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var res server.CleanResult
	if err := client.Call(conn, cmd, &res); err != nil {
		return err
	}
	fmt.Printf("removed %d entries (%s), size now %s\n", res.Entries, config.FormatSize(res.Bytes), config.FormatSize(res.Size))
	return nil
}

func stop(cfg config.Config) error {
	conn, err := client.Dial(cfg)
	if err != nil {
		return fmt.Errorf("daemon is not running (socket %s)", cfg.Socket)
	}
	defer func() { _ = conn.Close() }()
	var st server.Status
	if err := client.Call(conn, protocol.CmdShutdown, &st); err != nil {
		return err
	}
	fmt.Printf("daemon %d is shutting down (%d active sessions will finish first)\n", st.PID, st.ActiveSessions)
	return nil
}

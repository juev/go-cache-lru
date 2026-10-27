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
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juev/go-cache-plugin/internal/client"
	"github.com/juev/go-cache-plugin/internal/config"
	"github.com/juev/go-cache-plugin/internal/protocol"
	"github.com/juev/go-cache-plugin/internal/server"
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
	fs.DurationVar(&o.lockWait, "lock-wait", 10*time.Second, "serve: how long to wait for a previous daemon to exit")
	fs.BoolVar(&o.json, "json", false, "stats: print JSON")
	fs.BoolVar(&o.reset, "reset", false, "stats: reset counters")

	// Flags may come before and after the command.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
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

type cmdOpts struct {
	lockWait    time.Duration
	json, reset bool
}

func usage(w io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprint(w, `usage: go-cache-lru [flags] [command]

commands:
  (none)   relay a go command to the daemon; use as GOCACHEPROG
  serve    run the daemon in the foreground
  stats    show statistics (-json, -reset)
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
	err := server.Run(ctx, cfg, o.lockWait, logger.Printf)
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
	if o.reset {
		cmd = protocol.CmdStatsReset
	}
	var st server.Status
	if err := client.Call(conn, cmd, &st); err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(os.Stdout, st)
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

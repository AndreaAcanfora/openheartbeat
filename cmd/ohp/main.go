// Command ohp is the reference client and server of the Open Heartbeat
// Protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"time"
	_ "time/tzdata" // cron time zones work on systems without zone files

	"github.com/AndreaAcanfora/openheartbeat/conformance"
	"github.com/AndreaAcanfora/openheartbeat/cron"
	"github.com/AndreaAcanfora/openheartbeat/ohp"
	"github.com/AndreaAcanfora/openheartbeat/server"
)

var version = "dev"

const usage = `ohp — Open Heartbeat Protocol client and reference server

Usage:
  ohp run   [flags] -- command [args...]   run a job and report it
  ohp ping  [flags] [signal]               send one signal (success, start, fail, log, 0-255)
  ohp serve [flags]                        run the in-memory reference server
  ohp conform <check-url>                  test a server against the spec
  ohp version

The check URL comes from -url or the OHP_URL environment variable.
Run "ohp <command> -h" for the flags of a command.
`

func main() {
	// Binaries installed with "go install" carry no ldflags; their module
	// version is in the build info.
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdin, stdout, stderr)
	case "ping":
		return cmdPing(args[1:], stdin, stderr)
	case "serve":
		return cmdServe(args[1:], stderr)
	case "conform":
		return cmdConform(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "ohp %s (protocol %s)\n", version, ohp.Version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "ohp: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func checkURLFlag(fs *flag.FlagSet) *string {
	return fs.String("url", os.Getenv("OHP_URL"), "check URL (default $OHP_URL)")
}

func cmdPing(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkURL := checkURLFlag(fs)
	rid := fs.String("rid", "", "run ID")
	data := fs.String("data", "", "file whose content is sent as the body (- for stdin)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *checkURL == "" {
		fmt.Fprintln(stderr, "ohp: no check URL: use -url or set OHP_URL")
		return 2
	}
	sig := ohp.Success
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "ohp: ping takes at most one signal")
		return 2
	}
	if fs.NArg() == 1 && fs.Arg(0) != "success" {
		var err error
		if sig, err = ohp.ParseSignal(fs.Arg(0)); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	var body []byte
	if *data != "" {
		var err error
		if *data == "-" {
			body, err = io.ReadAll(stdin)
		} else {
			body, err = os.ReadFile(*data)
		}
		if err != nil {
			fmt.Fprintln(stderr, "ohp:", err)
			return 1
		}
	}
	host, _ := os.Hostname()
	_, err := ohp.NewClient().Send(context.Background(), *checkURL, ohp.Ping{Signal: sig, RunID: *rid, Body: body, Host: host})
	if err != nil {
		fmt.Fprintf(stderr, "ohp: %s signal to %s failed: %v\n", sig, ohp.Redact(*checkURL), trimPrefix(err))
		return 1
	}
	return 0
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	key := fs.String("key", os.Getenv("OHP_KEY"), "secret key in check URLs (default $OHP_KEY, random if empty)")
	period := fs.Duration("period", 24*time.Hour, "expected period of checks created with create=1")
	grace := fs.Duration("grace", time.Hour, "grace time of checks created with create=1")
	var checks checkList
	fs.Var(&checks, "check", `predefined check as slug:period:grace ("backup:24h:1h") or slug:cron:grace[:time zone] ("backup:0 3 * * *:1h:Europe/Rome"); repeatable`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *key == "" {
		*key = strings.ReplaceAll(ohp.NewRunID(), "-", "")
	}
	srv := server.New(server.Config{Key: *key, AllowCreate: true, DefaultPeriod: *period, DefaultGrace: *grace})
	for _, c := range checks {
		if c.schedule != nil {
			srv.AddCronCheck(c.slug, c.schedule, c.grace)
		} else {
			srv.AddCheck(c.slug, c.period, c.grace)
		}
	}
	base := "http://" + *addr
	fmt.Fprintf(stderr, "ohp: reference server on %s\n", base)
	fmt.Fprintf(stderr, "ohp: check URLs: %s (add ?create=1 to create a check on first use)\n", srv.CheckURL(base, "<slug>"))
	fmt.Fprintf(stderr, "ohp: check states: curl -H 'Authorization: Bearer %s' %s/api/checks\n", *key, base)

	hs := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, "ohp:", err)
		return 1
	}
	return 0
}

func cmdConform(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conform", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pace := fs.Duration("pace", 0, "wait between requests, for servers that rate-limit pings (e.g. 15s)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: ohp conform [-pace 15s] <check-url>")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// The timeout covers each request plus the pause before it.
	client := &http.Client{Timeout: 10*time.Second + *pace}
	if *pace > 0 {
		client.Transport = &pacedTransport{next: http.DefaultTransport, every: *pace}
	}
	results, err := conformance.Run(ctx, client, fs.Arg(0))
	for _, r := range results {
		mark := "PASS"
		if !r.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(stdout, "%s  %-6s §%-4s %s", mark, r.Level, r.Section, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(stdout, " (%s)", r.Detail)
		}
		fmt.Fprintln(stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ohp:", err)
		return 1
	}
	if !conformance.Passed(results) {
		fmt.Fprintln(stdout, "\nnot conforming: at least one MUST case failed")
		return 1
	}
	fmt.Fprintln(stdout, "\nconforming to OHP", ohp.Version)
	return 0
}

type checkSpec struct {
	slug          string
	period, grace time.Duration
	schedule      *cron.Schedule
}

type checkList []checkSpec

func (l *checkList) String() string { return "" }

// Set parses slug:period:grace or slug:cron expression:grace[:time zone],
// e.g. "backup:24h:1h" or "backup:0 3 * * *:1h:Europe/Rome".
func (l *checkList) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) != 3 && len(parts) != 4 {
		return fmt.Errorf("want slug:period:grace or slug:cron:grace[:time zone], got %q", v)
	}
	spec := checkSpec{slug: parts[0]}
	g, err := time.ParseDuration(parts[2])
	if err != nil {
		return err
	}
	spec.grace = g
	if p, err := time.ParseDuration(parts[1]); err == nil && len(parts) == 3 {
		spec.period = p
	} else {
		loc := time.UTC
		if len(parts) == 4 {
			if loc, err = time.LoadLocation(parts[3]); err != nil {
				return fmt.Errorf("time zone: %w", err)
			}
		}
		if spec.schedule, err = cron.Parse(parts[1], loc); err != nil {
			return err
		}
	}
	*l = append(*l, spec)
	return nil
}

func trimPrefix(err error) string {
	return strings.TrimPrefix(err.Error(), "ohp: ")
}

// pacedTransport spaces requests out, for servers that rate-limit pings.
type pacedTransport struct {
	next  http.RoundTripper
	every time.Duration
	last  time.Time
}

func (p *pacedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if wait := p.every - time.Since(p.last); !p.last.IsZero() && wait > 0 {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	p.last = time.Now()
	return p.next.RoundTrip(r)
}

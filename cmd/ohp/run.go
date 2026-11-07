package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
)

// sender is the part of ohp.Client that run needs; tests replace it.
type sender interface {
	Send(ctx context.Context, checkURL string, p ohp.Ping) (*ohp.Result, error)
}

var newSender = func() sender { return ohp.NewClient() }

func cmdRun(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkURL := checkURLFlag(fs)
	tail := fs.Int("tail", 10_000, "bytes of output (the last ones) sent with the result; 0 sends none")
	noStart := fs.Bool("no-start", false, "do not send a start signal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "usage: ohp run [flags] -- command [args...]")
		return 2
	}

	client := newSender()
	ctx := context.Background()
	host, _ := os.Hostname()
	rid := ohp.NewRunID()
	report := func(p ohp.Ping) {
		if *checkURL == "" {
			return
		}
		p.RunID, p.Host = rid, host
		if _, err := client.Send(ctx, *checkURL, p); err != nil {
			// Never change the job's outcome because monitoring is down.
			fmt.Fprintf(stderr, "ohp: %s signal to %s failed: %v\n", p.Signal, ohp.Redact(*checkURL), trimPrefix(err))
		}
	}
	if *checkURL == "" {
		fmt.Fprintln(stderr, "ohp: no check URL (-url or OHP_URL): running the job without reporting")
	}
	if !*noStart {
		report(ohp.Ping{Signal: ohp.Start})
	}

	out := newTailBuffer(*tail)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = io.MultiWriter(stdout, out)
	cmd.Stderr = io.MultiWriter(stderr, out)

	began := time.Now()
	code, runErr := runForwardingSignals(cmd)
	elapsed := time.Since(began)

	sig := ohp.ExitCode(code)
	body := out.Bytes()
	if runErr != nil {
		// The job could not start, or was killed by a signal.
		sig = ohp.Fail
		body = append(body, []byte("\nohp: "+runErr.Error()+"\n")...)
		fmt.Fprintln(stderr, "ohp:", runErr)
	}
	report(ohp.Ping{Signal: sig, Body: body, Duration: elapsed})
	return code
}

// runForwardingSignals runs cmd, passing interrupts on to it, and returns the
// exit code to use. The error is non-nil when the command did not exit
// normally.
func runForwardingSignals(cmd *exec.Cmd) (int, error) {
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return 127, err
		}
		return 126, err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		// Killed by a signal: report like a shell does.
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), fmt.Errorf("job killed by %s", ws.Signal())
		}
	}
	return 1, err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	if t.max <= 0 {
		return len(p), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return len(p), nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

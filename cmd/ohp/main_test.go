package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
)

// The test binary doubles as the job that "ohp run" executes.
func TestMain(m *testing.M) {
	if os.Getenv("OHP_TEST_JOB") == "1" {
		fmt.Println("job output line")
		fmt.Fprintln(os.Stderr, "job error line")
		code := 0
		fmt.Sscan(os.Getenv("OHP_TEST_EXIT"), &code)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type fakeSender struct {
	mu    sync.Mutex
	pings []ohp.Ping
	err   error
}

func (f *fakeSender) Send(_ context.Context, _ string, p ohp.Ping) (*ohp.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings = append(f.pings, p)
	if f.err != nil {
		return nil, f.err
	}
	return &ohp.Result{StatusCode: 200, Attempts: 1}, nil
}

func runJob(t *testing.T, f *fakeSender, exit string, args ...string) (int, string) {
	t.Helper()
	newSender = func() sender { return f }
	t.Cleanup(func() { newSender = func() sender { return ohp.NewClient() } })
	t.Setenv("OHP_TEST_JOB", "1")
	t.Setenv("OHP_TEST_EXIT", exit)
	var stdout, stderr bytes.Buffer
	argv := append([]string{"run", "-url", "https://hb.example/ping/secret/job"}, args...)
	argv = append(argv, "--", os.Args[0])
	code := run(argv, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func TestRunReportsSuccess(t *testing.T) {
	f := &fakeSender{}
	code, out := runJob(t, f, "0")
	if code != 0 || !strings.Contains(out, "job output line") {
		t.Fatalf("code %d, output %q", code, out)
	}
	if len(f.pings) != 2 || f.pings[0].Signal != ohp.Start || f.pings[1].Signal != ohp.ExitCode(0) {
		t.Fatalf("pings = %+v", f.pings)
	}
	if f.pings[0].RunID == "" || f.pings[0].RunID != f.pings[1].RunID {
		t.Error("start and completion must share a run ID")
	}
	body := string(f.pings[1].Body)
	if !strings.Contains(body, "job output line") || !strings.Contains(body, "job error line") {
		t.Errorf("body = %q", body)
	}
	if f.pings[1].Duration <= 0 {
		t.Error("no duration")
	}
}

func TestRunPreservesExitCode(t *testing.T) {
	f := &fakeSender{}
	code, _ := runJob(t, f, "3", "-no-start")
	if code != 3 {
		t.Errorf("exit code %d, want 3", code)
	}
	if len(f.pings) != 1 || f.pings[0].Signal != ohp.ExitCode(3) {
		t.Errorf("pings = %+v", f.pings)
	}
}

func TestRunSurvivesMonitoringOutage(t *testing.T) {
	f := &fakeSender{err: errors.New("connection refused")}
	code, out := runJob(t, f, "0")
	if code != 0 {
		t.Errorf("job outcome changed by monitoring outage: %d", code)
	}
	if !strings.Contains(out, "connection refused") || strings.Contains(out, "secret") {
		t.Errorf("output = %q", out)
	}
}

func TestRunTailOnly(t *testing.T) {
	f := &fakeSender{}
	runJob(t, f, "0", "-tail", "10")
	if body := string(f.pings[1].Body); len(body) != 10 || !strings.HasSuffix(body, "line\n") {
		t.Errorf("body = %q", body)
	}
}

func TestRunMissingCommand(t *testing.T) {
	f := &fakeSender{}
	newSender = func() sender { return f }
	defer func() { newSender = func() sender { return ohp.NewClient() } }()
	var stderr bytes.Buffer
	code := run([]string{"run", "-url", "https://hb.example/x", "--", "ohp-no-such-command"}, nil, &bytes.Buffer{}, &stderr)
	if code != 127 {
		t.Errorf("code %d, want 127", code)
	}
	if last := f.pings[len(f.pings)-1]; last.Signal != ohp.Fail || !strings.Contains(string(last.Body), "ohp-no-such-command") {
		t.Errorf("last ping = %+v", last)
	}
}

func TestTailBuffer(t *testing.T) {
	b := newTailBuffer(5)
	b.Write([]byte("abc"))
	b.Write([]byte("defg"))
	if got := string(b.Bytes()); got != "cdefg" {
		t.Errorf("got %q", got)
	}
	b.Write([]byte("0123456789"))
	if got := string(b.Bytes()); got != "56789" {
		t.Errorf("got %q", got)
	}
}

func TestPingValidation(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"ping", "-url", "https://hb.example/x", "999"}, nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("invalid signal: code %d", code)
	}
	t.Setenv("OHP_URL", "")
	if code := run([]string{"ping"}, nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("missing URL: code %d", code)
	}
}

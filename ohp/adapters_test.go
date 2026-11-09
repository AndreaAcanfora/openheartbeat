package ohp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// capture records outgoing requests instead of sending them.
type capture struct{ reqs []*http.Request }

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.reqs = append(c.reqs, r)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("OK")), Header: http.Header{}}, nil
}

func captured(t *testing.T, checkURL string, p Ping) (*Result, *http.Request) {
	t.Helper()
	cap := &capture{}
	c := testClient()
	c.HTTP = &http.Client{Transport: cap}
	res, err := c.Send(context.Background(), checkURL, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cap.reqs) == 0 {
		return res, nil
	}
	return res, cap.reqs[0]
}

const rid = "0190d6a8-0000-7000-8000-00000000000a"

func TestServiceFor(t *testing.T) {
	cases := map[string]string{
		"https://cronitor.link/p/KEY/nightly":                             "cronitor",
		"https://eu.cronitor.link/p/KEY/nightly":                          "cronitor",
		"https://cronitor.link/d3x0c1":                                    "", // anonymous short codes are not supported
		"https://o123.ingest.sentry.io/api/456/cron/nightly/abcdef/":      "sentry",
		"https://o123.ingest.us.sentry.io/api/456/cron/nightly/abcdef/":   "sentry",
		"https://sentry.io/api/456/cron/nightly/abcdef/":                  "", // not an ingest host
		"https://nosnch.in/c2354d53d2":                                    "deadmanssnitch",
		"https://hc-ping.com/4d7ea345-8494-40f0-8dbe-bb98e63c3a99":        "",
		"http://cronitor.link/p/KEY/nightly":                              "", // only https
		"https://status.example.com/ping/0123456789abcdef/nightly-backup": "",
	}
	for u, want := range cases {
		if got := ServiceFor(u); got != want {
			t.Errorf("ServiceFor(%s) = %q, want %q", u, got, want)
		}
	}
}

func TestCronitor(t *testing.T) {
	u := "https://cronitor.link/p/KEY/nightly?env=prod"
	res, req := captured(t, u, Ping{Signal: Start, RunID: rid, Host: "web-1"})
	q := req.URL.Query()
	if res.Service != "cronitor" || req.Method != "GET" || req.URL.Path != "/p/KEY/nightly" || q.Get("state") != "run" || q.Get("series") != rid || q.Get("host") != "web-1" || q.Get("env") != "prod" {
		t.Fatalf("start: %s %s", req.Method, req.URL)
	}
	long := strings.Repeat("x", 3000) + "pg_dump: disk full"
	_, req = captured(t, u, Ping{Signal: ExitCode(4), RunID: rid, Body: []byte(long), Duration: 1500 * time.Millisecond})
	q = req.URL.Query()
	if q.Get("state") != "fail" || q.Get("status_code") != "4" || q.Get("metric") != "duration:1.500" || len([]rune(q.Get("message"))) != 2000 || !strings.HasSuffix(q.Get("message"), "disk full") {
		t.Fatalf("failure: %v", q)
	}
	if req.Header.Get("OHP-Version") != "" {
		t.Error("OHP headers sent to Cronitor")
	}
	_, req = captured(t, u, Ping{Signal: ExitCode(0)})
	if q := req.URL.Query(); q.Get("state") != "complete" || q.Get("status_code") != "0" {
		t.Errorf("success: %v", q)
	}
	_, req = captured(t, u, Ping{Signal: Log, Body: []byte("50%")})
	if q := req.URL.Query(); q.Get("state") != "tick" || q.Get("message") != "50%" {
		t.Errorf("log: %v", q)
	}
}

func TestSentry(t *testing.T) {
	u := "https://o123.ingest.sentry.io/api/456/cron/nightly/abcdef/"
	_, req := captured(t, u, Ping{Signal: Start, RunID: rid})
	if q := req.URL.Query(); req.Method != "GET" || q.Get("status") != "in_progress" || q.Get("check_in_id") != rid || req.URL.Path != "/api/456/cron/nightly/abcdef/" {
		t.Fatalf("start: %s", req.URL)
	}
	_, req = captured(t, u, Ping{Signal: ExitCode(1), RunID: rid})
	if q := req.URL.Query(); q.Get("status") != "error" || q.Get("check_in_id") != rid {
		t.Errorf("failure: %s", req.URL)
	}
	_, req = captured(t, u, Ping{Signal: Success})
	if q := req.URL.Query(); q.Get("status") != "ok" || q.Has("check_in_id") {
		t.Errorf("success: %s", req.URL)
	}
	res, req := captured(t, u, Ping{Signal: Log, Body: []byte("x")})
	if req != nil || !res.Skipped {
		t.Error("log signals must not reach Sentry")
	}
}

func TestDeadMansSnitch(t *testing.T) {
	u := "https://nosnch.in/c2354d53d2"
	for sig, want := range map[Signal]string{ExitCode(0): "s=0", ExitCode(3): "s=3", Fail: "s=1", Success: "s=0"} {
		_, req := captured(t, u, Ping{Signal: sig})
		body, _ := io.ReadAll(req.Body)
		if req.Method != "POST" || string(body) != want || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("%s: %s %s %q", sig, req.Method, req.URL, body)
		}
	}
	for _, sig := range []Signal{Start, Log} {
		if res, req := captured(t, u, Ping{Signal: sig}); req != nil || !res.Skipped {
			t.Errorf("%s should be skipped", sig)
		}
	}
}

package ohp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSignal(t *testing.T) {
	valid := map[string]Signal{"": Success, "start": Start, "fail": Fail, "log": Log, "0": "0", "7": "7", "255": "255"}
	for in, want := range valid {
		got, err := ParseSignal(in)
		if err != nil || got != want {
			t.Errorf("ParseSignal(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"256", "01", "-1", "1a", "START", "success", "1000", " 1"} {
		if _, err := ParseSignal(in); err == nil {
			t.Errorf("ParseSignal(%q) succeeded, want error", in)
		}
	}
}

func TestSignalSemantics(t *testing.T) {
	cases := []struct {
		s                 Signal
		completes, failed bool
	}{
		{Success, true, false}, {Start, false, false}, {Log, false, false},
		{Fail, true, true}, {ExitCode(0), true, false}, {ExitCode(2), true, true},
	}
	for _, c := range cases {
		if c.s.Completes() != c.completes || c.s.Failed() != c.failed {
			t.Errorf("%s: completes=%v failed=%v", c.s, c.s.Completes(), c.s.Failed())
		}
	}
	if ExitCode(300) != Fail || ExitCode(-1) != Fail {
		t.Error("out of range exit codes must map to Fail")
	}
}

func TestSignalURL(t *testing.T) {
	cases := []struct {
		check string
		s     Signal
		rid   string
		want  string
	}{
		{"https://hb.example/p/abc", Success, "", "https://hb.example/p/abc"},
		{"https://hb.example/p/abc/", Start, "0190d6a8-0000-7000-8000-00000000000a", "https://hb.example/p/abc/start?rid=0190d6a8-0000-7000-8000-00000000000a"},
		{"https://hb.example/p/abc?create=1", ExitCode(3), "", "https://hb.example/p/abc/3?create=1"},
		{"http://localhost:8080/ping/k/backup", Log, "", "http://localhost:8080/ping/k/backup/log"},
	}
	for _, c := range cases {
		got, err := SignalURL(c.check, c.s, c.rid)
		if err != nil || got != c.want {
			t.Errorf("SignalURL(%q, %q, %q) = %q, %v; want %q", c.check, c.s, c.rid, got, err, c.want)
		}
	}
	for _, bad := range []string{"ftp://x/y", "not a url", "https:///nohost"} {
		if _, err := SignalURL(bad, Success, ""); err == nil {
			t.Errorf("SignalURL(%q) succeeded", bad)
		}
	}
	for id, ok := range map[string]bool{"0190D6A8-0000-7000-8000-00000000000A": true, "run-42": false, "0190d6a8000070008000000000000000": false} {
		if ValidRunID(id) != ok {
			t.Errorf("ValidRunID(%q) = %v", id, !ok)
		}
	}
	if _, err := SignalURL("https://x/y", Success, "bad rid"); err == nil {
		t.Error("invalid rid accepted")
	}
}

func TestNewRunID(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == b || !ValidRunID(a) || len(a) != 36 || a[14] != '4' {
		t.Errorf("bad run IDs %q %q", a, b)
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("https://hb.example/ping/secret/job?rid=1"); strings.Contains(got, "secret") {
		t.Errorf("Redact leaked the secret: %q", got)
	}
}

func testClient() *Client {
	c := NewClient()
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestSendRequest(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, body = r, string(b)
		w.Header().Set("OHP-Log-Truncated", "1")
		io.WriteString(w, "OK\n")
	}))
	defer srv.Close()

	res, err := testClient().Send(context.Background(), srv.URL+"/ping/k/job", Ping{
		Signal: ExitCode(1), RunID: "0190d6a8-0000-7000-8000-00000000000a", Body: []byte("boom"), Duration: 1500 * time.Millisecond, Host: "h1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Attempts != 1 {
		t.Errorf("result = %+v", res)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/ping/k/job/1" || got.URL.Query().Get("rid") != "0190d6a8-0000-7000-8000-00000000000a" {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	if body != "boom" || got.Header.Get("OHP-Duration") != "1.500" || got.Header.Get("OHP-Host") != "h1" || got.Header.Get("OHP-Version") != Version {
		t.Errorf("body %q headers %v", body, got.Header)
	}
}

func TestSendRetries(t *testing.T) {
	cases := []struct {
		name     string
		statuses []int
		attempts int32
		wantErr  bool
	}{
		{"5xx then ok", []int{503, 502, 200}, 3, false},
		{"429 then ok", []int{429, 200}, 2, false},
		{"404 is final", []int{404}, 1, true},
		{"400 is final", []int{400}, 1, true},
		{"gives up after 5", []int{500, 500, 500, 500, 500, 500}, 5, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var n atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := n.Add(1) - 1
				w.WriteHeader(c.statuses[i])
			}))
			defer srv.Close()
			_, err := testClient().Send(context.Background(), srv.URL+"/c", Ping{})
			if (err != nil) != c.wantErr || n.Load() != c.attempts {
				t.Errorf("err=%v attempts=%d, want err=%v attempts=%d", err, n.Load(), c.wantErr, c.attempts)
			}
		})
	}
}

func TestSendRetryAfterAndBackoff(t *testing.T) {
	var waits []time.Duration
	c := NewClient()
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		case 2:
			w.Header().Set("Retry-After", "3600") // too long: ignored
			w.WriteHeader(429)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c.Send(context.Background(), srv.URL+"/c", Ping{})
	want := []time.Duration{7 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
}

func TestSendNetworkErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/ping/topsecret/job"
	srv.Close()
	c := testClient()
	c.MaxAttempts = 2
	_, err := c.Send(context.Background(), url, Ping{})
	var se *StatusError
	if err == nil || errors.As(err, &se) {
		t.Fatalf("want network error, got %v", err)
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Errorf("error leaks the check URL: %v", err)
	}
}

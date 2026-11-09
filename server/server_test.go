package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(t *testing.T) (*Server, *clock) {
	t.Helper()
	clk := &clock{time.Date(2026, 11, 7, 22, 0, 0, 0, time.UTC)}
	s := New(Config{Key: "k3y", AllowCreate: true, Now: clk.now, MaxBodyBytes: 100})
	s.AddCheck("backup", time.Hour, 10*time.Minute)
	return s, clk
}

func send(s *Server, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestResponses(t *testing.T) {
	s, _ := newTest(t)
	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/ping/k3y/backup", 200},
		{"HEAD", "/ping/k3y/backup", 200},
		{"POST", "/ping/k3y/backup/start?rid=0190d6a8-0000-7000-8000-00000000000a", 200},
		{"POST", "/ping/k3y/backup/255", 200},
		{"POST", "/ping/k3y/backup/log?unknown=1", 200},
		{"POST", "/ping/k3y/backup/256", 400},
		{"POST", "/ping/k3y/backup/01", 400},
		{"POST", "/ping/k3y/backup/nope", 400},
		{"POST", "/ping/k3y/backup?rid=a%20b", 400},
		{"POST", "/ping/k3y/backup?rid=run-42", 400},
		{"PUT", "/ping/k3y/backup", 405},
		{"POST", "/ping/wrong/backup", 404},
		{"POST", "/ping/k3y/missing", 404},
		{"POST", "/ping/k3y/Bad.Slug?create=1", 404},
		{"POST", "/ping/k3y/fresh?create=1", 201},
		{"POST", "/ping/k3y/fresh?create=1", 200},
	}
	for _, c := range cases {
		w := send(s, c.method, c.path, "")
		if w.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, w.Code, c.want)
		}
		if w.Header().Get("OHP-Version") != "0.2" {
			t.Errorf("%s %s: missing OHP-Version", c.method, c.path)
		}
	}
}

func TestBodyTruncation(t *testing.T) {
	s, _ := newTest(t)
	w := send(s, "POST", "/ping/k3y/backup/log", strings.Repeat("x", 500))
	if w.Code != 200 || w.Header().Get("OHP-Log-Truncated") != "1" {
		t.Fatalf("code %d, truncated header %q", w.Code, w.Header().Get("OHP-Log-Truncated"))
	}
	st, _ := s.Status("backup")
	if ev := st.Events[0]; len(ev.Body) != 100 || !ev.Truncated {
		t.Errorf("stored %d bytes, truncated=%v", len(ev.Body), ev.Truncated)
	}
	if w := send(s, "POST", "/ping/k3y/backup/log", "short"); w.Header().Get("OHP-Log-Truncated") != "" {
		t.Error("short body marked as truncated")
	}
}

func state(t *testing.T, s *Server) State {
	t.Helper()
	st, ok := s.Status("backup")
	if !ok {
		t.Fatal("no check")
	}
	return st.State
}

func TestStateMachine(t *testing.T) {
	s, clk := newTest(t)
	if got := state(t, s); got != StateNew {
		t.Fatalf("initial state %s", got)
	}
	send(s, "POST", "/ping/k3y/backup", "")
	if got := state(t, s); got != StateUp {
		t.Fatalf("after success: %s", got)
	}
	clk.advance(time.Hour)
	if got := state(t, s); got != StateUp {
		t.Errorf("exactly at the deadline: %s", got)
	}
	clk.advance(5 * time.Minute)
	if got := state(t, s); got != StateLate {
		t.Errorf("within grace: %s", got)
	}
	clk.advance(6 * time.Minute)
	if got := state(t, s); got != StateDown {
		t.Errorf("after grace: %s", got)
	}
	send(s, "GET", "/ping/k3y/backup/0", "")
	if got := state(t, s); got != StateUp {
		t.Errorf("after exit 0: %s", got)
	}
	send(s, "POST", "/ping/k3y/backup/log", "note")
	if got := state(t, s); got != StateUp {
		t.Errorf("log changed the state: %s", got)
	}
	send(s, "POST", "/ping/k3y/backup/2", "")
	if got := state(t, s); got != StateDown {
		t.Errorf("after exit 2: %s", got)
	}
	send(s, "POST", "/ping/k3y/backup/fail", "")
	send(s, "POST", "/ping/k3y/backup", "")
	if got := state(t, s); got != StateUp {
		t.Errorf("recovery: %s", got)
	}
}

func TestRunsAndDurations(t *testing.T) {
	s, clk := newTest(t)
	send(s, "POST", "/ping/k3y/backup", "")

	// Two overlapping runs, told apart by rid.
	send(s, "POST", "/ping/k3y/backup/start?rid=0190d6a8-0000-7000-8000-00000000000a", "")
	clk.advance(time.Minute)
	send(s, "POST", "/ping/k3y/backup/start?rid=0190d6a8-0000-7000-8000-00000000000b", "")
	st, _ := s.Status("backup")
	if !st.Running {
		t.Fatal("not running after start")
	}
	clk.advance(30 * time.Second)
	send(s, "POST", "/ping/k3y/backup/0?rid=0190d6a8-0000-7000-8000-00000000000a", "")
	if st, _ = s.Status("backup"); st.LastDuration != 90*time.Second || !st.Running {
		t.Errorf("run a: duration %s running %v", st.LastDuration, st.Running)
	}
	clk.advance(30 * time.Second)
	send(s, "POST", "/ping/k3y/backup/0?rid=0190d6a8-0000-7000-8000-00000000000b", "")
	if st, _ = s.Status("backup"); st.LastDuration != time.Minute || st.Running {
		t.Errorf("run b: duration %s running %v", st.LastDuration, st.Running)
	}

	// Without a matching start, the client's duration is used.
	req := httptest.NewRequest("POST", "/ping/k3y/backup", nil)
	req.Header.Set("OHP-Duration", "2.5")
	s.ServeHTTP(httptest.NewRecorder(), req)
	if st, _ = s.Status("backup"); st.LastDuration != 2500*time.Millisecond {
		t.Errorf("client duration: %s", st.LastDuration)
	}

	// A run that never completes takes the check down after the grace time.
	send(s, "POST", "/ping/k3y/backup/start", "")
	clk.advance(11 * time.Minute)
	if got := state(t, s); got != StateDown {
		t.Errorf("hung run: %s", got)
	}
}

func TestDiscoveryAndList(t *testing.T) {
	s, _ := newTest(t)
	w := send(s, "GET", "/.well-known/ohp", "")
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc["versions"].([]any)[0] != "0.2" {
		t.Fatalf("discovery: %s", w.Body)
	}
	if w := send(s, "GET", "/api/checks", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("list without key: %d", w.Code)
	}
	req := httptest.NewRequest("GET", "/api/checks", nil)
	req.Header.Set("Authorization", "Bearer k3y")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var list []Status
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].Slug != "backup" {
		t.Errorf("list: %s", w.Body)
	}
}

func TestEventsAreCapped(t *testing.T) {
	s, _ := newTest(t)
	for range 30 {
		send(s, "POST", "/ping/k3y/backup/log", "")
	}
	if st, _ := s.Status("backup"); len(st.Events) != 20 {
		t.Errorf("kept %d events", len(st.Events))
	}
}

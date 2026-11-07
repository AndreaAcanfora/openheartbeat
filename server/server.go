// Package server is a reference Open Heartbeat Protocol server.
//
// It keeps checks in memory and serves check URLs of the form
// /ping/{key}/{slug}. It is meant for testing clients, for the conformance
// suite and as a readable model of the protocol, not as a production monitor.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
)

// State of a check, as described in section 8 of the spec.
type State string

const (
	StateNew  State = "new"
	StateUp   State = "up"
	StateLate State = "late"
	StateDown State = "down"
)

// Event is one recorded signal.
type Event struct {
	Time      time.Time     `json:"time"`
	Signal    string        `json:"signal"`
	RunID     string        `json:"rid,omitempty"`
	ExitCode  *int          `json:"exit_code,omitempty"`
	Duration  time.Duration `json:"duration_ns,omitempty"`
	Body      string        `json:"body,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Host      string        `json:"host,omitempty"`
}

// Status is the public view of a check.
type Status struct {
	Slug         string        `json:"slug"`
	State        State         `json:"state"`
	Running      bool          `json:"running"`
	Period       time.Duration `json:"period_ns"`
	Grace        time.Duration `json:"grace_ns"`
	LastSuccess  *time.Time    `json:"last_success,omitempty"`
	LastFailure  *time.Time    `json:"last_failure,omitempty"`
	LastDuration time.Duration `json:"last_duration_ns,omitempty"`
	Events       []Event       `json:"events"`
}

type check struct {
	slug          string
	period, grace time.Duration
	lastSuccess   time.Time
	lastFailure   time.Time
	lastDuration  time.Duration
	// failing is true when the last completion was a failure.
	failing bool
	// open runs: run ID ("" for runs without one) → start time
	runs   map[string]time.Time
	events []Event
}

// Config configures a Server.
type Config struct {
	// Key is the secret part of every check URL. It should hold at least
	// 128 bits of entropy.
	Key string
	// AllowCreate lets create=1 create unknown checks.
	AllowCreate bool
	// DefaultPeriod and DefaultGrace apply to checks created on the fly.
	DefaultPeriod time.Duration
	DefaultGrace  time.Duration
	// MaxBodyBytes is the number of body bytes kept per signal.
	MaxBodyBytes int
	// KeepEvents is the number of events kept per check.
	KeepEvents int
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// Server is an http.Handler implementing OHP 0.1.
type Server struct {
	cfg    Config
	mu     sync.Mutex
	checks map[string]*check
	mux    *http.ServeMux
}

var slugPattern = regexp.MustCompile(`^[a-z0-9_-]{1,100}$`)

// New returns a server with sensible defaults for unset fields.
func New(cfg Config) *Server {
	if cfg.DefaultPeriod == 0 {
		cfg.DefaultPeriod = 24 * time.Hour
	}
	if cfg.DefaultGrace == 0 {
		cfg.DefaultGrace = time.Hour
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 100_000
	}
	if cfg.KeepEvents == 0 {
		cfg.KeepEvents = 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{cfg: cfg, checks: map[string]*check{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("/ping/{key}/{slug}", s.handlePing)
	s.mux.HandleFunc("/ping/{key}/{slug}/{signal}", s.handlePing)
	s.mux.HandleFunc("GET /.well-known/ohp", s.handleDiscovery)
	s.mux.HandleFunc("GET /api/checks", s.handleList)
	return s
}

// AddCheck registers a check, or updates the schedule of an existing one.
func (s *Server) AddCheck(slug string, period, grace time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.checks[slug]; ok {
		c.period, c.grace = period, grace
		return
	}
	s.checks[slug] = &check{slug: slug, period: period, grace: grace, runs: map[string]time.Time{}}
}

// CheckURL returns the check URL of slug on a server reachable at base.
func (s *Server) CheckURL(base, slug string) string {
	return strings.TrimRight(base, "/") + "/ping/" + s.cfg.Key + "/" + slug
}

// Status returns the state of one check.
func (s *Server) Status(slug string) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checks[slug]
	if !ok {
		return Status{}, false
	}
	return c.status(s.cfg.Now()), true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("OHP-Version", ohp.Version)
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		reply(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Read the body before answering, so that the connection can be reused.
	body, truncated := s.readBody(r)

	sig, err := ohp.ParseSignal(r.PathValue("signal"))
	if err != nil {
		reply(w, r, http.StatusBadRequest, "unknown signal")
		return
	}
	rid := r.URL.Query().Get("rid")
	if r.URL.Query().Has("rid") && !ohp.ValidRunID(rid) {
		reply(w, r, http.StatusBadRequest, "invalid rid")
		return
	}
	slug := r.PathValue("slug")
	if r.PathValue("key") != s.cfg.Key || !slugPattern.MatchString(slug) {
		reply(w, r, http.StatusNotFound, "not found")
		return
	}

	now := s.cfg.Now()
	s.mu.Lock()
	c, ok := s.checks[slug]
	created := false
	if !ok {
		if !s.cfg.AllowCreate || r.URL.Query().Get("create") != "1" {
			s.mu.Unlock()
			reply(w, r, http.StatusNotFound, "not found")
			return
		}
		c = &check{slug: slug, period: s.cfg.DefaultPeriod, grace: s.cfg.DefaultGrace, runs: map[string]time.Time{}}
		s.checks[slug] = c
		created = true
	}
	ev := Event{
		Time:      now,
		Signal:    sig.String(),
		RunID:     rid,
		Body:      string(body),
		Truncated: truncated,
		Host:      r.Header.Get("OHP-Host"),
	}
	if code, ok := sig.Code(); ok {
		ev.ExitCode = &code
	}
	c.record(ev, sig, clientDuration(r))
	if len(c.events) > s.cfg.KeepEvents {
		c.events = c.events[len(c.events)-s.cfg.KeepEvents:]
	}
	s.mu.Unlock()

	if truncated {
		w.Header().Set("OHP-Log-Truncated", "1")
	}
	if created {
		reply(w, r, http.StatusCreated, "Created")
		return
	}
	reply(w, r, http.StatusOK, "OK")
}

func (s *Server) readBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxBodyBytes)+1))
	if len(body) <= s.cfg.MaxBodyBytes {
		return body, false
	}
	// Drain a bounded amount of the rest so the client sees a response
	// instead of a reset connection.
	io.Copy(io.Discard, io.LimitReader(r.Body, 10<<20))
	return body[:s.cfg.MaxBodyBytes], true
}

func clientDuration(r *http.Request) time.Duration {
	secs, err := strconv.ParseFloat(r.Header.Get("OHP-Duration"), 64)
	if err != nil || secs <= 0 || secs > 1e9 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

func (c *check) record(ev Event, sig ohp.Signal, clientDur time.Duration) {
	switch {
	case sig == ohp.Start:
		c.runs[ev.RunID] = ev.Time
	case sig.Completes():
		// Prefer the server's own measurement (spec section 6.2).
		if started, ok := c.runs[ev.RunID]; ok {
			ev.Duration = ev.Time.Sub(started)
			delete(c.runs, ev.RunID)
		} else {
			ev.Duration = clientDur
		}
		if ev.Duration > 0 {
			c.lastDuration = ev.Duration
		}
		c.failing = sig.Failed()
		if c.failing {
			c.lastFailure = ev.Time
		} else {
			c.lastSuccess = ev.Time
		}
	}
	c.events = append(c.events, ev)
}

func (c *check) state(now time.Time) State {
	for _, started := range c.runs {
		if now.Sub(started) > c.grace {
			return StateDown
		}
	}
	switch {
	case c.lastSuccess.IsZero() && c.lastFailure.IsZero():
		return StateNew
	case c.failing:
		return StateDown
	}
	due := c.lastSuccess.Add(c.period)
	switch {
	case !now.After(due):
		return StateUp
	case !now.After(due.Add(c.grace)):
		return StateLate
	default:
		return StateDown
	}
}

func (c *check) status(now time.Time) Status {
	st := Status{
		Slug:         c.slug,
		State:        c.state(now),
		Running:      len(c.runs) > 0,
		Period:       c.period,
		Grace:        c.grace,
		LastDuration: c.lastDuration,
		Events:       append([]Event(nil), c.events...),
	}
	if !c.lastSuccess.IsZero() {
		t := c.lastSuccess
		st.LastSuccess = &t
	}
	if !c.lastFailure.IsZero() {
		t := c.lastFailure
		st.LastFailure = &t
	}
	return st
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"versions":       []string{ohp.Version},
		"max_body_bytes": s.cfg.MaxBodyBytes,
		"create":         s.cfg.AllowCreate,
	})
}

// handleList is not part of the protocol: it lets operators and tests see
// check states. It requires the key as a bearer token.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.cfg.Key {
		reply(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mu.Lock()
	now := s.cfg.Now()
	out := make([]Status, 0, len(s.checks))
	for _, c := range s.checks {
		out = append(out, c.status(now))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func reply(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		io.WriteString(w, msg+"\n")
	}
}

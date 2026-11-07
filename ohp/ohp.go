// Package ohp is a client for the Open Heartbeat Protocol (OHP), version 0.1.
//
// A job reports to a heartbeat server by sending signals to its check URL:
//
//	c := ohp.NewClient()
//	rid := ohp.NewRunID()
//	c.Send(ctx, checkURL, ohp.Ping{Signal: ohp.Start, RunID: rid})
//	// ... run the job ...
//	c.Send(ctx, checkURL, ohp.Ping{Signal: ohp.ExitCode(code), RunID: rid, Body: output})
//
// See SPEC.md for the protocol.
package ohp

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Version is the protocol version implemented by this package.
const Version = "0.1"

// Signal selects what a ping reports. Its value is the path suffix appended
// to the check URL.
type Signal string

const (
	Success Signal = ""
	Start   Signal = "start"
	Fail    Signal = "fail"
	Log     Signal = "log"
)

// ExitCode returns the signal that reports a job's exit status. Codes outside
// 0..255 are reported as Fail, since the protocol cannot carry them.
func ExitCode(code int) Signal {
	if code < 0 || code > 255 {
		return Fail
	}
	return Signal(strconv.Itoa(code))
}

// ParseSignal parses a path suffix into a signal. It accepts exactly the
// suffixes defined in section 4 of the spec.
func ParseSignal(s string) (Signal, error) {
	switch Signal(s) {
	case Success, Start, Fail, Log:
		return Signal(s), nil
	}
	if code, ok := parseCode(s); ok {
		return ExitCode(code), nil
	}
	return "", fmt.Errorf("ohp: unknown signal %q", s)
}

// Code reports the exit code carried by the signal, if any.
func (s Signal) Code() (int, bool) {
	return parseCode(string(s))
}

// Completes reports whether the signal ends a run (success, fail or exit code).
func (s Signal) Completes() bool {
	return s != Start && s != Log
}

// Failed reports whether the signal reports a failure.
func (s Signal) Failed() bool {
	if s == Fail {
		return true
	}
	code, ok := s.Code()
	return ok && code != 0
}

func (s Signal) String() string {
	if s == Success {
		return "success"
	}
	return string(s)
}

func parseCode(s string) (int, bool) {
	if s == "" || len(s) > 3 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, _ := strconv.Atoi(s)
	return n, n <= 255
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidRunID reports whether id is a valid run ID.
func ValidRunID(id string) bool {
	return runIDPattern.MatchString(id)
}

// NewRunID returns a random UUID (version 4), the recommended run ID format.
func NewRunID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SignalURL builds the URL of a signal: the check URL with the signal suffix
// appended to its path and the run ID, if any, added to its query.
func SignalURL(checkURL string, s Signal, runID string) (string, error) {
	u, err := url.Parse(checkURL)
	if err != nil {
		return "", fmt.Errorf("ohp: invalid check URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("ohp: check URL must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("ohp: check URL has no host")
	}
	if runID != "" && !ValidRunID(runID) {
		return "", fmt.Errorf("ohp: invalid run ID %q", runID)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	if s != Success {
		u.Path += "/" + string(s)
	}
	if runID != "" {
		q := u.Query()
		q.Set("rid", runID)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Redact hides the path and query of a check URL, which hold its secret, so
// that the URL can appear in logs and error messages.
func Redact(checkURL string) string {
	u, err := url.Parse(checkURL)
	if err != nil || u.Host == "" {
		return "<check URL>"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

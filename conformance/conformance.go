// Package conformance checks, over the wire, that a server implements the
// Open Heartbeat Protocol.
//
// The suite sends real signals to an existing check, so the check's history
// will show them. It ends with a success signal, leaving the check up.
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/ohp"
)

// Level says how strongly the spec requires a behaviour.
type Level string

const (
	Must   Level = "MUST"
	Should Level = "SHOULD"
)

// Result is the outcome of one test case.
type Result struct {
	Name    string
	Level   Level
	Section string
	Passed  bool
	Detail  string
}

type testCase struct {
	name    string
	level   Level
	section string
	run     func(ctx context.Context, t *tester) error
}

type tester struct {
	http     *http.Client
	checkURL string
}

// Run runs the suite against checkURL, which must identify an existing check.
func Run(ctx context.Context, client *http.Client, checkURL string) ([]Result, error) {
	if _, err := ohp.SignalURL(checkURL, ohp.Success, ""); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	t := &tester{http: client, checkURL: strings.TrimRight(checkURL, "/")}
	results := make([]Result, 0, len(cases))
	for _, c := range cases {
		err := c.run(ctx, t)
		r := Result{Name: c.name, Level: c.level, Section: c.section, Passed: err == nil}
		if err != nil {
			r.Detail = err.Error()
		}
		results = append(results, r)
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
	}
	return results, nil
}

// Passed reports whether every MUST case passed.
func Passed(results []Result) bool {
	for _, r := range results {
		if r.Level == Must && !r.Passed {
			return false
		}
	}
	return true
}

var cases = []testCase{
	{"success with GET", Must, "4.1", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodGet, t.checkURL, nil)
	}},
	{"success with HEAD", Must, "4.1", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodHead, t.checkURL, nil)
	}},
	{"success with POST and a body", Must, "6.1", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.checkURL, []byte("conformance: success with a body\n"))
	}},
	{"start then exit code 0 with the same rid", Must, "5", func(ctx context.Context, t *tester) error {
		rid := ohp.NewRunID()
		if err := t.expectOK(ctx, http.MethodPost, t.url("start", "rid="+rid), nil); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		return t.expectOK(ctx, http.MethodPost, t.url("0", "rid="+rid), nil)
	}},
	{"fail", Must, "4", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.url("fail", ""), []byte("conformance: fail\n"))
	}},
	{"log", Must, "4", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.url("log", ""), []byte("conformance: log\n"))
	}},
	{"exit codes 1 and 255", Must, "4", func(ctx context.Context, t *tester) error {
		for _, code := range []string{"1", "255"} {
			if err := t.expectOK(ctx, http.MethodPost, t.url(code, ""), nil); err != nil {
				return fmt.Errorf("code %s: %w", code, err)
			}
		}
		return nil
	}},
	{"unknown query parameters are ignored", Must, "5", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.url("log", "ohp-conformance-unknown=1"), nil)
	}},
	{"10 000 byte body is accepted", Must, "6.1", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.url("log", ""), filler(10_000))
	}},
	{"oversized body is truncated, not rejected", Must, "6.1", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.url("log", ""), filler(1_000_000))
	}},
	{"oversized body sets OHP-Log-Truncated", Should, "6.1", func(ctx context.Context, t *tester) error {
		resp, err := t.do(ctx, http.MethodPost, t.url("log", ""), filler(1_000_000))
		if err != nil {
			return err
		}
		if resp.Header.Get("OHP-Log-Truncated") != "1" {
			return fmt.Errorf("header missing")
		}
		return nil
	}},
	{"exit code 256 is rejected", Must, "4", func(ctx context.Context, t *tester) error {
		return t.expectStatus(ctx, t.url("256", ""), 400, 404)
	}},
	{"exit code with a leading zero is rejected", Should, "4", func(ctx context.Context, t *tester) error {
		return t.expectStatus(ctx, t.url("01", ""), 400, 404)
	}},
	{"unknown suffix is rejected", Must, "4", func(ctx context.Context, t *tester) error {
		return t.expectStatus(ctx, t.url("ohp-conformance-bogus", ""), 400, 404)
	}},
	{"invalid rid is rejected", Must, "5", func(ctx context.Context, t *tester) error {
		return t.expectStatus(ctx, t.url("log", "rid="+url.QueryEscape("not a valid rid!")), 400)
	}},
	{"rid that is not a UUID is rejected", Must, "5", func(ctx context.Context, t *tester) error {
		return t.expectStatus(ctx, t.url("log", "rid=run-42"), 400)
	}},
	{"unknown check returns 404", Should, "7", func(ctx context.Context, t *tester) error {
		return t.expectStatus(ctx, t.sibling(), 404)
	}},
	{"responses carry OHP-Version", Should, "7", func(ctx context.Context, t *tester) error {
		resp, err := t.do(ctx, http.MethodPost, t.url("log", ""), nil)
		if err != nil {
			return err
		}
		if v := resp.Header.Get("OHP-Version"); v == "" {
			return fmt.Errorf("header missing")
		}
		return nil
	}},
	// Last, so that the check is left up.
	{"final success", Must, "4", func(ctx context.Context, t *tester) error {
		return t.expectOK(ctx, http.MethodPost, t.checkURL, []byte("conformance: done\n"))
	}},
}

func (t *tester) url(suffix, query string) string {
	u := t.checkURL
	if suffix != "" {
		if i := strings.IndexByte(u, '?'); i >= 0 {
			u = u[:i] + "/" + suffix + u[i:]
		} else {
			u += "/" + suffix
		}
	}
	if query != "" {
		if strings.Contains(u, "?") {
			u += "&" + query
		} else {
			u += "?" + query
		}
	}
	return u
}

// sibling returns the check URL with its last path segment replaced by random
// characters, which should name no check.
func (t *tester) sibling() string {
	u, err := url.Parse(t.checkURL)
	if err != nil {
		return t.checkURL
	}
	i := strings.LastIndexByte(u.Path, '/')
	var b [16]byte
	rand.Read(b[:])
	u.Path = u.Path[:i+1] + hex.EncodeToString(b[:])
	u.RawPath = ""
	return u.String()
}

func (t *tester) do(ctx context.Context, method, target string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("OHP-Version", ohp.Version)
	if body != nil {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	resp, err := t.http.Do(req)
	if err != nil {
		// Do not echo the URL: it holds the check secret.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp, nil
}

func (t *tester) expectOK(ctx context.Context, method, target string, body []byte) error {
	resp, err := t.do(ctx, method, target, body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("got %d, want 200", resp.StatusCode)
	}
	return nil
}

func (t *tester) expectStatus(ctx context.Context, target string, want ...int) error {
	resp, err := t.do(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	for _, w := range want {
		if resp.StatusCode == w {
			return nil
		}
	}
	return fmt.Errorf("got %d, want %v", resp.StatusCode, want)
}

func filler(n int) []byte {
	line := []byte("ohp conformance filler line 0123456789abcdefghijklmnopqrstuvwxyz\n")
	b := bytes.Repeat(line, n/len(line)+1)
	return b[:n]
}

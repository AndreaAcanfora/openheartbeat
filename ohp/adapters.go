package ohp

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Some monitoring services have their own ping API instead of OHP. When a
// check URL belongs to one of them, Send translates the signal into that API,
// using only what the service documents:
//
//   - Cronitor telemetry URLs (https://cronitor.link/p/<key>/<monitor>):
//     state run / complete / fail / tick, series (run ID), status_code,
//     message (last 2,000 characters of the body), host, metric=duration.
//   - Sentry Crons HTTP check-ins (https://o<org>.ingest.sentry.io/api/<project>/cron/<monitor>/<key>/):
//     status in_progress / ok / error, check_in_id (run ID). Log signals are
//     not sent.
//   - Dead Man's Snitch (https://nosnch.in/<token>): one check-in per run with
//     s=<exit code>. Start and log signals are not sent.
type adapter struct {
	name    string
	match   func(u *url.URL) bool
	request func(ctx context.Context, u *url.URL, p Ping) (*http.Request, error) // nil: nothing to send
}

var adapters = []adapter{
	{"cronitor", matchCronitor, cronitorRequest},
	{"sentry", matchSentry, sentryRequest},
	{"deadmanssnitch", func(u *url.URL) bool { return u.Host == "nosnch.in" }, snitchRequest},
}

// ServiceFor names the service whose own API a check URL uses ("cronitor",
// "sentry", "deadmanssnitch"), or returns "" for an OHP server.
func ServiceFor(checkURL string) string {
	if a := adapterFor(checkURL); a != nil {
		return a.name
	}
	return ""
}

func adapterFor(checkURL string) *adapter {
	u, err := url.Parse(checkURL)
	if err != nil || u.Scheme != "https" {
		return nil
	}
	for i := range adapters {
		if adapters[i].match(u) {
			return &adapters[i]
		}
	}
	return nil
}

func matchCronitor(u *url.URL) bool {
	return (u.Host == "cronitor.link" || u.Host == "eu.cronitor.link") && strings.HasPrefix(u.Path, "/p/")
}

func matchSentry(u *url.URL) bool {
	return strings.HasSuffix(u.Hostname(), ".sentry.io") && strings.Contains(u.Hostname(), "ingest") && strings.Contains(u.Path, "/cron/")
}

func tail(b []byte, n int) string {
	s := strings.ToValidUTF8(string(b), "")
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

func cronitorRequest(ctx context.Context, u *url.URL, p Ping) (*http.Request, error) {
	q := u.Query()
	switch {
	case p.Signal == Start:
		q.Set("state", "run")
	case p.Signal == Log:
		q.Set("state", "tick")
	case p.Signal.Failed():
		q.Set("state", "fail")
	default:
		q.Set("state", "complete")
	}
	if p.RunID != "" {
		q.Set("series", p.RunID)
	}
	if code, ok := p.Signal.Code(); ok {
		q.Set("status_code", strconv.Itoa(code))
	}
	if len(p.Body) > 0 {
		q.Set("message", tail(p.Body, 2000))
	}
	if p.Host != "" {
		q.Set("host", p.Host)
	}
	if p.Duration > 0 && p.Signal.Completes() {
		q.Set("metric", "duration:"+strconv.FormatFloat(p.Duration.Seconds(), 'f', 3, 64))
	}
	target := *u
	target.RawQuery = q.Encode()
	return http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
}

func sentryRequest(ctx context.Context, u *url.URL, p Ping) (*http.Request, error) {
	q := u.Query()
	switch {
	case p.Signal == Log:
		return nil, nil
	case p.Signal == Start:
		q.Set("status", "in_progress")
	case p.Signal.Failed():
		q.Set("status", "error")
	default:
		q.Set("status", "ok")
	}
	if p.RunID != "" {
		q.Set("check_in_id", p.RunID)
	}
	target := *u
	target.RawQuery = q.Encode()
	return http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
}

func snitchRequest(ctx context.Context, u *url.URL, p Ping) (*http.Request, error) {
	if !p.Signal.Completes() {
		return nil, nil
	}
	code := 0
	if c, ok := p.Signal.Code(); ok {
		code = c
	} else if p.Signal == Fail {
		code = 1
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(url.Values{"s": {strconv.Itoa(code)}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

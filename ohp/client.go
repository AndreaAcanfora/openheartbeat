package ohp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Ping is one signal to send.
type Ping struct {
	Signal Signal
	// RunID ties a start signal to its completion. Optional.
	RunID string
	// Body is the job output or a message. Optional; callers that capture
	// output should pass its tail.
	Body []byte
	// Duration is the client-measured run duration, sent as OHP-Duration.
	// Optional.
	Duration time.Duration
	// Host is sent as OHP-Host. Optional.
	Host string
}

// Result describes a signal the server accepted.
type Result struct {
	StatusCode int
	// Truncated reports that the server stored only part of the body.
	Truncated bool
	// Attempts is the number of requests made, including retries.
	Attempts int
}

// StatusError is returned when the server rejects a signal.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("ohp: server returned %d", e.StatusCode)
	}
	return fmt.Sprintf("ohp: server returned %d: %s", e.StatusCode, e.Body)
}

// Client sends signals. The zero value is not usable; use NewClient.
type Client struct {
	HTTP *http.Client
	// MaxAttempts is the total number of attempts per signal, retries included.
	MaxAttempts int
	// BaseDelay is the first retry delay; it doubles on each retry.
	BaseDelay time.Duration
	// AttemptTimeout bounds each attempt.
	AttemptTimeout time.Duration
	UserAgent      string

	sleep func(context.Context, time.Duration) error
}

// NewClient returns a client with the defaults of section 7.1 of the spec.
func NewClient() *Client {
	return &Client{
		HTTP:           &http.Client{},
		MaxAttempts:    5,
		BaseDelay:      time.Second,
		AttemptTimeout: 10 * time.Second,
		UserAgent:      "ohp-go/" + Version,
		sleep:          sleepCtx,
	}
}

const maxRetryAfter = 60 * time.Second

// Send delivers a signal to the check, retrying on network errors, 5xx and
// 429 responses. Errors never include the check URL, which is a secret.
func (c *Client) Send(ctx context.Context, checkURL string, p Ping) (*Result, error) {
	target, err := SignalURL(checkURL, p.Signal, p.RunID)
	if err != nil {
		return nil, err
	}
	delay := c.BaseDelay
	var lastErr error
	for attempt := 1; ; attempt++ {
		res, retryAfter, err := c.attempt(ctx, target, p)
		if err == nil {
			res.Attempts = attempt
			return res, nil
		}
		lastErr = err
		if !retryable(err) || attempt >= c.MaxAttempts || ctx.Err() != nil {
			return nil, lastErr
		}
		wait := delay
		if retryAfter > 0 && retryAfter <= maxRetryAfter {
			wait = retryAfter
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, lastErr
		}
		delay *= 2
	}
}

func (c *Client) attempt(ctx context.Context, target string, p Ping) (*Result, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(p.Body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("OHP-Version", Version)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if len(p.Body) > 0 {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	if p.Duration > 0 {
		req.Header.Set("OHP-Duration", strconv.FormatFloat(p.Duration.Seconds(), 'f', 3, 64))
	}
	if p.Host != "" {
		req.Header.Set("OHP-Host", p.Host)
	}
	req.Header.Set("OHP-Timestamp", time.Now().UTC().Format(time.RFC3339))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Strip the URL from the error: it carries the check secret.
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) {
			err = uerr.Unwrap()
		}
		return nil, 0, &netError{err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &Result{
			StatusCode: resp.StatusCode,
			Truncated:  resp.Header.Get("OHP-Log-Truncated") == "1",
		}, 0, nil
	}
	var retryAfter time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	return nil, retryAfter, &StatusError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(body))}
}

type netError struct{ err error }

func (e *netError) Error() string { return "ohp: " + e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode == http.StatusTooManyRequests || se.StatusCode >= 500
	}
	var ne *netError
	return errors.As(err, &ne)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

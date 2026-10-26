package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RetryPolicy controls how hard dhaara tries before giving up.
type RetryPolicy struct {
	MaxRetries   int           // retries per request for server errors and rate limits
	NetworkGrace time.Duration // keep retrying connection errors at least this long (Wi-Fi drops)
	BaseDelay    time.Duration // first wait; doubles on each retry
	MaxDelay     time.Duration // longest wait between retries
	FileAttempts int           // whole-file attempts before a file is reported as failed
	FilePause    time.Duration // wait between whole-file attempts
	StallTimeout time.Duration // abort a request when no data has moved for this long
	Notice       func(string)  // optional: tell the user when retries drag on
}

var DefaultRetry = RetryPolicy{
	MaxRetries:   10,
	NetworkGrace: 15 * time.Minute,
	BaseDelay:    time.Second,
	MaxDelay:     32 * time.Second,
	FileAttempts: 3,
	FilePause:    15 * time.Second,
	StallTimeout: 90 * time.Second,
}

// FatalError stops the whole run (missing setup, Drive full, sign-in revoked).
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

func Fatalf(format string, a ...any) error { return &FatalError{Msg: fmt.Sprintf(format, a...)} }

// NetError is a connection problem: timeout, reset, DNS failure, stall.
type NetError struct{ Err error }

func (e *NetError) Error() string { return "network: " + e.Err.Error() }
func (e *NetError) Unwrap() error { return e.Err }

// LocalError is a problem reading the file on this computer. Not retried.
type LocalError struct{ Err error }

func (e *LocalError) Error() string { return "local file: " + e.Err.Error() }
func (e *LocalError) Unwrap() error { return e.Err }

// IntegrityError means Drive's copy did not match the local file.
type IntegrityError struct{ Msg string }

func (e *IntegrityError) Error() string { return e.Msg }

// APIError is an error reply from Google.
type APIError struct {
	Status     int
	Reason     string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	reason := ""
	if e.Reason != "" {
		reason = " " + e.Reason
	}
	return fmt.Sprintf("Google replied %d%s: %s", e.Status, reason, e.Message)
}

func (e *APIError) Retryable() bool {
	switch {
	case e.Status == 401, e.Status == 408, e.Status == 429, e.Status >= 500:
		return true
	case e.Status == 403:
		return e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded"
	}
	return false
}

func ParseAPIError(resp *Response) *APIError {
	msg := strings.TrimSpace(string(resp.Body[:min(len(resp.Body), 300)]))
	e := &APIError{Status: resp.Status, Message: msg}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(resp.Body, &body) == nil {
		if body.Error.Message != "" {
			e.Message = body.Error.Message
		}
		if len(body.Error.Errors) > 0 {
			e.Reason = body.Error.Errors[0].Reason
		}
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

func IsRetryable(err error) bool {
	var ne *NetError
	var ae *APIError
	switch {
	case errors.As(err, &ne):
		return true
	case errors.As(err, &ae):
		return ae.Retryable()
	}
	return false
}

// Retrier implements truncated exponential backoff with jitter, as Google
// recommends: 1s, 2s, 4s ... capped, plus up to one BaseDelay of randomness.
type Retrier struct {
	p         RetryPolicy
	attempt   int
	firstFail time.Time
}

func NewRetrier(p RetryPolicy) *Retrier { return &Retrier{p: p} }

func (r *Retrier) Reset() { r.attempt, r.firstFail = 0, time.Time{} }

// Wait sleeps before the next try. It returns err when it is time to give up,
// or the context error if the run was stopped.
func (r *Retrier) Wait(ctx context.Context, err error) error {
	r.attempt++
	if r.firstFail.IsZero() {
		r.firstFail = time.Now()
	}
	var ne *NetError
	stillInGrace := errors.As(err, &ne) && time.Since(r.firstFail) < r.p.NetworkGrace
	if r.attempt > r.p.MaxRetries && !stillInGrace {
		return err
	}
	if r.p.Notice != nil && (r.attempt == 3 || r.attempt%10 == 0) {
		msg := err.Error()
		if len(msg) > 160 {
			msg = msg[:160] + "..."
		}
		r.p.Notice(fmt.Sprintf("Having trouble talking to Google (%s). Still retrying...", msg))
	}
	delay := r.p.BaseDelay << min(r.attempt-1, 16)
	if delay > r.p.MaxDelay || delay <= 0 {
		delay = r.p.MaxDelay
	}
	if r.p.BaseDelay > 0 {
		delay += rand.N(r.p.BaseDelay)
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > delay {
		delay = min(ae.RetryAfter, 5*time.Minute)
	}
	return SleepCtx(ctx, delay)
}

func SleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pacer spaces out API requests so all workers together stay under a fixed rate.
type pacer struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func (p *pacer) wait(ctx context.Context) error {
	if p.interval <= 0 {
		return ctx.Err()
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	at := p.next
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	return SleepCtx(ctx, time.Until(at))
}

// Headers builds an http.Header from key/value pairs.
func Headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

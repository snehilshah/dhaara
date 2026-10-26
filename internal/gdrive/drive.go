// Package gdrive is a small Google Drive v3 client: sign-in, requests with
// retries and pacing, and the folder and file calls dhaara needs.
package gdrive

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	FolderMime = "application/vnd.google-apps.folder"
	FileFields = "id,name,mimeType,size,md5Checksum,modifiedTime"
)

var errStalled = errors.New("stalled")

type File struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     string `json:"size"`
	MD5      string `json:"md5Checksum"`
	Modified string `json:"modifiedTime"`
}

func (f File) SizeBytes() int64 {
	n, err := strconv.ParseInt(f.Size, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func (f File) IsFolder() bool { return f.MimeType == FolderMime }

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Drive is a small client for the parts of the Drive v3 REST API we need.
type Drive struct {
	hc       *http.Client
	ep       Endpoints
	auth     *Auth
	retry    RetryPolicy
	pace     *pacer
	sent     atomic.Int64 // file bytes handed to the network, for the speed display
	received atomic.Int64 // file bytes downloaded, for the speed display
}

// NewHTTPClient builds the HTTP client from scratch. roots is nil except in
// tests (nil means the system's certificates).
func NewHTTPClient(roots *x509.CertPool) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		// HTTP/1.1 only: Drive uploads are known to stall over HTTP/2 (rclone
		// turns it off for Drive too). Offer only "http/1.1" when connecting
		// and register no HTTP/2 handler. (Cloning http.DefaultTransport is
		// not safe here: the copy can still offer "h2" to the server.)
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}, RootCAs: roots},
		TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{
		Transport: t,
		// Upload progress replies are "308 Resume Incomplete"; never follow them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func New(hc *http.Client, ep Endpoints, auth *Auth, rp RetryPolicy, pace time.Duration) *Drive {
	return &Drive{hc: hc, ep: ep, auth: auth, retry: rp, pace: &pacer{interval: pace}}
}

// activityReader notes every time data moves, for stall detection and speed.
type activityReader struct {
	r     io.Reader
	touch func()
	sent  *atomic.Int64
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.touch()
		if a.sent != nil {
			a.sent.Add(int64(n))
		}
	}
	return n, err
}

// once sends a single request, without retries. It returns the access token
// it used so a 401 can trigger exactly one refresh.
func (d *Drive) once(ctx context.Context, method, rawURL string, body []byte, hdr http.Header) (*Response, string, error) {
	if err := d.pace.wait(ctx); err != nil {
		return nil, "", err
	}
	tok, err := d.auth.AccessToken(ctx)
	if err != nil {
		return nil, "", err
	}

	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()

	var rd io.Reader
	if len(body) > 0 {
		var sent *atomic.Int64
		if strings.HasPrefix(rawURL, d.ep.Upload) {
			sent = &d.sent
		}
		rd = &activityReader{r: bytes.NewReader(body), touch: touch, sent: sent}
	}
	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, rd)
	if err != nil {
		return nil, tok, err
	}
	req.ContentLength = int64(len(body))
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	done := make(chan struct{})
	defer close(done)
	go d.watch(reqCtx, cancel, &last, done)

	resp, err := d.hc.Do(req)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(&activityReader{r: io.LimitReader(resp.Body, 64<<20), touch: touch})
		resp.Body.Close()
	}
	if err != nil {
		return nil, tok, d.netErr(ctx, reqCtx, err)
	}
	return &Response{resp.StatusCode, resp.Header, data}, tok, nil
}

// netErr turns a failed request into the stop error, a stall, or a netError.
func (d *Drive) netErr(ctx, reqCtx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(context.Cause(reqCtx), errStalled) {
		return &NetError{fmt.Errorf("no data moved for %s", d.retry.StallTimeout)}
	}
	return &NetError{Err: err}
}

// ErrRestart asks a download to start the file again from byte 0.
var ErrRestart = errors.New("restart download from the beginning")

// Stream sends one GET and hands the open reply to fn while the stall
// watchdog runs, so large downloads never sit in memory. No retries here.
// Errors from fn pass through if they are local, API or restart errors; any
// other error (a broken connection while reading) becomes a netError.
func (d *Drive) Stream(ctx context.Context, rawURL string, hdr http.Header, fn func(resp *http.Response, body io.Reader) error) error {
	if err := d.pace.wait(ctx); err != nil {
		return err
	}
	tok, err := d.auth.AccessToken(ctx)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()

	req, err := http.NewRequestWithContext(reqCtx, "GET", rawURL, nil)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	done := make(chan struct{})
	defer close(done)
	go d.watch(reqCtx, cancel, &last, done)

	resp, err := d.hc.Do(req)
	if err != nil {
		return d.netErr(ctx, reqCtx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		if err := d.auth.Rejected(ctx, tok); err != nil {
			return err
		}
		return &APIError{Status: 401, Message: "sign-in needed refreshing"}
	}
	err = fn(resp, &activityReader{r: resp.Body, touch: touch, sent: &d.received})
	var le *LocalError
	var ae *APIError
	if err == nil || errors.As(err, &le) || errors.As(err, &ae) || errors.Is(err, ErrRestart) {
		return err
	}
	return d.netErr(ctx, reqCtx, err)
}

// watch cancels a request when no data has moved for StallTimeout, so a dead
// Wi-Fi link is noticed in about a minute instead of the kernel's ~15.
func (d *Drive) watch(ctx context.Context, cancel context.CancelCauseFunc, last *atomic.Int64, done <-chan struct{}) {
	limit := d.retry.StallTimeout
	if limit <= 0 {
		return
	}
	tick := time.NewTicker(min(limit/4, 5*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			if time.Since(time.Unix(0, last.Load())) > limit {
				cancel(errStalled)
				return
			}
		}
	}
}

// Call sends a request and retries network errors, rate limits and server
// errors with backoff. ok lists the statuses that count as success.
func (d *Drive) Call(ctx context.Context, method, rawURL string, body []byte, hdr http.Header, ok ...int) (*Response, error) {
	if len(ok) == 0 {
		ok = []int{http.StatusOK}
	}
	r := NewRetrier(d.retry)
	for {
		resp, tok, err := d.once(ctx, method, rawURL, body, hdr)
		if err == nil {
			if slices.Contains(ok, resp.Status) {
				return resp, nil
			}
			ae := ParseAPIError(resp)
			if ae.Status == http.StatusUnauthorized {
				if rerr := d.auth.Rejected(ctx, tok); rerr != nil {
					return nil, rerr
				}
			}
			err = ae
		}
		if !IsRetryable(err) {
			return nil, err
		}
		slog.Warn("request failed, retrying", "method", method, "url", trimQuery(rawURL), "err", err)
		if werr := r.Wait(ctx, err); werr != nil {
			return nil, werr
		}
	}
}

func (d *Drive) CallJSON(ctx context.Context, method, rawURL string, in, out any) error {
	var body []byte
	hdr := http.Header{}
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		hdr.Set("Content-Type", "application/json; charset=UTF-8")
	}
	resp, err := d.Call(ctx, method, rawURL, body, hdr)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return fmt.Errorf("unexpected reply from Google: %w", err)
		}
	}
	return nil
}

func (d *Drive) APIURL(path string, q url.Values) string {
	if len(q) == 0 {
		return d.ep.API + path
	}
	return d.ep.API + path + "?" + q.Encode()
}

type AboutInfo struct {
	User struct {
		EmailAddress string `json:"emailAddress"`
	} `json:"user"`
	StorageQuota struct {
		Limit string `json:"limit"`
		Usage string `json:"usage"`
	} `json:"storageQuota"`
}

func (d *Drive) About(ctx context.Context) (AboutInfo, error) {
	var a AboutInfo
	err := d.CallJSON(ctx, "GET", d.APIURL("/about", url.Values{"fields": {"user(emailAddress),storageQuota(limit,usage)"}}), nil, &a)
	return a, err
}

// ListChildren returns the non-trashed items in a folder, oldest first.
func (d *Drive) ListChildren(ctx context.Context, parentID string) ([]File, error) {
	var all []File
	page := ""
	for {
		q := url.Values{
			"q":        {fmt.Sprintf("'%s' in parents and trashed = false", parentID)},
			"fields":   {"nextPageToken,files(" + FileFields + ")"},
			"pageSize": {"1000"},
			"orderBy":  {"createdTime"},
			"spaces":   {"drive"},
		}
		if page != "" {
			q.Set("pageToken", page)
		}
		var out struct {
			NextPageToken string `json:"nextPageToken"`
			Files         []File `json:"files"`
		}
		if err := d.CallJSON(ctx, "GET", d.APIURL("/files", q), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Files...)
		if out.NextPageToken == "" {
			return all, nil
		}
		page = out.NextPageToken
	}
}

func (d *Drive) CreateFolder(ctx context.Context, name, parentID string) (string, error) {
	var out File
	in := map[string]any{"name": name, "mimeType": FolderMime, "parents": []string{parentID}}
	err := d.CallJSON(ctx, "POST", d.APIURL("/files", url.Values{"fields": {"id"}}), in, &out)
	return out.ID, err
}

// Trash moves a file to Drive's trash (recoverable for 30 days). dhaara
// never deletes anything permanently.
func (d *Drive) Trash(ctx context.Context, id string) error {
	return d.CallJSON(ctx, "PATCH", d.APIURL("/files/"+url.PathEscape(id), url.Values{"fields": {"id"}}),
		map[string]any{"trashed": true}, nil)
}

func (d *Drive) GetFile(ctx context.Context, id string) (File, error) {
	var f File
	err := d.CallJSON(ctx, "GET", d.APIURL("/files/"+url.PathEscape(id), url.Values{"fields": {FileFields}}), nil, &f)
	return f, err
}

func trimQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

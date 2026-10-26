package gdrive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/snehilshah/dhaara/internal/platform"
)

type Endpoints struct{ Auth, Token, API, Upload string }

var GoogleEndpoints = Endpoints{
	Auth:   "https://accounts.google.com/o/oauth2/v2/auth",
	Token:  "https://oauth2.googleapis.com/token",
	API:    "https://www.googleapis.com/drive/v3",
	Upload: "https://www.googleapis.com/upload/drive/v3",
}

// Only files this app creates are visible to it.
const driveScope = "https://www.googleapis.com/auth/drive.file"

const setupHelp = `No Google API credentials found at:
  %s

One-time setup (about 5 minutes, free):
  1. Open https://console.cloud.google.com/ and create a project (any name).
  2. APIs & Services > Library: search "Google Drive API" and click Enable.
  3. Google Auth Platform (OAuth consent screen): click Get started, give the app
     a name and your email, choose "External", and under Audience add your own
     Gmail address as a Test user.
  4. Clients > Create client > Application type "Desktop app" > Create.
  5. Download the JSON file and save it as:
       %s
Then run this command again.`

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

type oauthError struct{ Code, Description string }

func (e *oauthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// Auth holds the Google sign-in and refreshes it when it expires.
type Auth struct {
	mu           sync.Mutex
	clientID     string
	clientSecret string
	tokenPath    string
	tok          *Token
	hc           *http.Client
	ep           Endpoints
	retry        RetryPolicy
}

func LoadAuth(clientSecretPath, tokenPath string, hc *http.Client, ep Endpoints, rp RetryPolicy) (*Auth, error) {
	raw, err := os.ReadFile(clientSecretPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Fatalf(setupHelp, clientSecretPath, clientSecretPath)
	}
	if err != nil {
		return nil, Fatalf("cannot read %s: %v", clientSecretPath, err)
	}
	var file struct {
		Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, Fatalf("cannot read %s: %v", clientSecretPath, err)
	}
	if file.Installed == nil || file.Installed.ClientID == "" {
		return nil, Fatalf("%s is not a \"Desktop app\" OAuth client. Create a client with application type \"Desktop app\".", clientSecretPath)
	}
	a := &Auth{
		clientID:     file.Installed.ClientID,
		clientSecret: file.Installed.ClientSecret,
		tokenPath:    tokenPath,
		hc:           hc,
		ep:           ep,
		retry:        rp,
	}
	if raw, err := os.ReadFile(tokenPath); err == nil {
		var t Token
		if json.Unmarshal(raw, &t) == nil && t.RefreshToken != "" {
			a.tok = &t
		}
	}
	return a, nil
}

func (a *Auth) SignedIn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tok != nil
}

// AccessToken returns a valid access token, refreshing it if it is about to expire.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return "", Fatalf("not signed in to Google; run the same command again to sign in")
	}
	if time.Until(a.tok.ExpiresAt) < 2*time.Minute {
		if err := a.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return a.tok.AccessToken, nil
}

// Rejected is called when Google answers 401 for a token. It refreshes,
// unless another worker already did.
func (a *Auth) Rejected(ctx context.Context, used string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok != nil && a.tok.AccessToken == used {
		return a.refreshLocked(ctx)
	}
	return nil
}

func (a *Auth) refreshLocked(ctx context.Context) error {
	reply, err := a.tokenCall(ctx, url.Values{
		"client_id":     {a.clientID},
		"client_secret": {a.clientSecret},
		"refresh_token": {a.tok.RefreshToken},
		"grant_type":    {"refresh_token"},
	})
	var oe *oauthError
	if errors.As(err, &oe) && oe.Code == "invalid_grant" {
		os.Remove(a.tokenPath)
		a.tok = nil
		return Fatalf("Google sign-in expired or was revoked. Run the same command again to sign in; the upload will continue where it stopped")
	}
	if err != nil {
		return err
	}
	a.tok.AccessToken = reply.AccessToken
	a.tok.ExpiresAt = time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	if reply.RefreshToken != "" {
		a.tok.RefreshToken = reply.RefreshToken
	}
	return platform.WritePrivateJSON(a.tokenPath, a.tok)
}

func (a *Auth) tokenCall(ctx context.Context, form url.Values) (*tokenReply, error) {
	body := form.Encode()
	r := NewRetrier(a.retry)
	for {
		reply, err := a.tokenOnce(ctx, body)
		if err == nil {
			return reply, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var oe *oauthError
		if errors.As(err, &oe) {
			return nil, err
		}
		if !IsRetryable(err) {
			return nil, Fatalf("Google sign-in failed: %v", err)
		}
		slog.Warn("sign-in request failed, retrying", "err", err)
		if werr := r.Wait(ctx, err); werr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, Fatalf("could not reach Google to sign in (%v); check the internet connection and run again", werr)
		}
	}
}

func (a *Auth) tokenOnce(ctx context.Context, body string) (*tokenReply, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", a.ep.Token, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, &NetError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &NetError{Err: err}
	}
	if resp.StatusCode == http.StatusOK {
		var reply tokenReply
		if err := json.Unmarshal(data, &reply); err != nil || reply.AccessToken == "" {
			return nil, errors.New("bad sign-in reply from Google")
		}
		return &reply, nil
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return nil, &APIError{Status: resp.StatusCode, Message: "sign-in server busy"}
	}
	var oe struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(data, &oe) != nil || oe.Error == "" {
		oe.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return nil, &oauthError{oe.Error, oe.Description}
}

// Login runs the browser sign-in: Google redirects back to a temporary web
// page on 127.0.0.1 (OAuth "loopback" flow with PKCE).
func (a *Auth) Login(ctx context.Context, ui Printer) error {
	verifier := randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(16)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Fatalf("cannot start local sign-in page: %v", err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)

	results := make(chan url.Values, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if !q.Has("code") && !q.Has("error") {
				http.NotFound(w, r)
				return
			}
			msg := "Sign-in failed. Check the terminal."
			if q.Has("code") && q.Get("state") == state {
				msg = "Signed in. You can close this tab and go back to the terminal."
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<html><body><h2>%s</h2></body></html>", msg)
			select {
			case results <- q:
			default:
			}
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := a.ep.Auth + "?" + url.Values{
		"client_id":             {a.clientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {driveScope},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	ui.Println("Sign in to Google in your browser. If it doesn't open, visit:\n  " + authURL)
	openBrowser(authURL)

	var q url.Values
	select {
	case q = <-results:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Minute):
		return Fatalf("timed out waiting for Google sign-in")
	}
	if q.Get("state") != state {
		return Fatalf("sign-in reply did not match this request; please try again")
	}
	if e := q.Get("error"); e != "" {
		return Fatalf("Google sign-in was not completed: %s", e)
	}

	reply, err := a.tokenCall(ctx, url.Values{
		"client_id":     {a.clientID},
		"client_secret": {a.clientSecret},
		"code":          {q.Get("code")},
		"redirect_uri":  {redirect},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	})
	if err != nil {
		var oe *oauthError
		if errors.As(err, &oe) {
			return Fatalf("Google sign-in failed: %v", err)
		}
		return err
	}
	if reply.Scope != "" && !strings.Contains(reply.Scope, driveScope) {
		return Fatalf("Google Drive access was not granted; run again and allow access")
	}
	if reply.RefreshToken == "" {
		return Fatalf("Google did not return a long-lived sign-in; remove this app at https://myaccount.google.com/permissions and try again")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = &Token{
		AccessToken:  reply.AccessToken,
		RefreshToken: reply.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second),
	}
	if err := platform.WritePrivateJSON(a.tokenPath, a.tok); err != nil {
		return Fatalf("cannot save sign-in: %v", err)
	}
	ui.Println("Signed in.")
	return nil
}

func randomString(nBytes int) string {
	b := make([]byte, nBytes)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func openBrowser(u string) {
	path, err := exec.LookPath("xdg-open")
	if err != nil {
		return
	}
	cmd := exec.Command(path, u)
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

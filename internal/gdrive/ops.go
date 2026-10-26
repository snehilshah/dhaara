package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Copy makes a copy of a file on Drive, inside parent. The copy happens on
// Google's side: no file data passes through this machine.
func (d *Drive) Copy(ctx context.Context, id, name, parent, modified string) (File, error) {
	in := map[string]any{"name": name, "parents": []string{parent}}
	if modified != "" {
		in["modifiedTime"] = modified
	}
	var out File
	err := d.CallJSON(ctx, "POST", d.APIURL("/files/"+url.PathEscape(id)+"/copy", url.Values{"fields": {FileFields}}), in, &out)
	return out, err
}

// Move renames a file or folder and, if newParent differs from oldParent,
// moves it there. Nothing is re-uploaded.
func (d *Drive) Move(ctx context.Context, id, name, oldParent, newParent string) error {
	q := url.Values{"fields": {"id"}}
	if newParent != oldParent {
		q.Set("addParents", newParent)
		q.Set("removeParents", oldParent)
	}
	return d.CallJSON(ctx, "PATCH", d.APIURL("/files/"+url.PathEscape(id), q), map[string]any{"name": name}, nil)
}

// ReadSmall downloads a small file (dhaara's own bookkeeping) into memory.
func (d *Drive) ReadSmall(ctx context.Context, id string) ([]byte, error) {
	resp, err := d.Call(ctx, "GET", d.APIURL("/files/"+url.PathEscape(id), url.Values{"alt": {"media"}}), nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// WriteSmall replaces the content of file id, or creates the file in parent
// when id is "". It returns the file's ID.
func (d *Drive) WriteSmall(ctx context.Context, id, name, parent string, data []byte) (string, error) {
	if id != "" {
		resp, err := d.Call(ctx, "PATCH", d.ep.Upload+"/files/"+url.PathEscape(id)+"?uploadType=media&fields=id",
			data, Headers("Content-Type", "application/json"))
		if err != nil {
			return "", err
		}
		var got File
		if err := json.Unmarshal(resp.Body, &got); err != nil || got.ID == "" {
			return "", fmt.Errorf("unexpected reply from Google")
		}
		return got.ID, nil
	}
	meta, _ := json.Marshal(map[string]any{"name": name, "parents": []string{parent},
		"modifiedTime": time.Now().UTC().Format(time.RFC3339)})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	part.Write(meta)
	part, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	part.Write(data)
	mw.Close()
	resp, err := d.Call(ctx, "POST", d.ep.Upload+"/files?uploadType=multipart&fields=id",
		buf.Bytes(), Headers("Content-Type", "multipart/related; boundary="+mw.Boundary()))
	if err != nil {
		return "", err
	}
	var got File
	if err := json.Unmarshal(resp.Body, &got); err != nil || got.ID == "" {
		return "", fmt.Errorf("unexpected upload reply from Google")
	}
	return got.ID, nil
}

// EnsureFolder returns the oldest folder called name in parent, creating it
// if create is set (otherwise "" when missing).
func EnsureFolder(ctx context.Context, d *Drive, parent, name string, create bool) (string, error) {
	id, err := FindFolder(ctx, d, parent, name)
	if err != nil || id != "" || !create {
		return id, err
	}
	return d.CreateFolder(ctx, name, parent)
}

// SplitDrivePath turns "Work/Reports" into its folder names. A Drive path
// starts at My Drive.
func SplitDrivePath(p string) ([]string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, fmt.Errorf("empty Drive folder name")
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if !SafeName(part) {
			return nil, fmt.Errorf("bad Drive folder path %q", p)
		}
	}
	return parts, nil
}

// ResolveFolder finds the folder at path (from My Drive), creating missing
// folders along the way if create is set. Where several folders share a
// name, the oldest is used. id is "" if it doesn't exist (and create is
// unset); parent is the folder holding it ("" if that is missing too).
func ResolveFolder(ctx context.Context, d *Drive, path string, create bool) (id, parent string, err error) {
	parts, err := SplitDrivePath(path)
	if err != nil {
		return "", "", Fatalf("%v", err)
	}
	cur := "root"
	for _, name := range parts {
		parent = cur
		if cur, err = EnsureFolder(ctx, d, cur, name, create); err != nil || cur == "" {
			return "", parent, err
		}
	}
	return cur, parent, nil
}

// FindFolder returns the ID of the oldest folder called name inside parent.
func FindFolder(ctx context.Context, d *Drive, parent, name string) (string, error) {
	kids, err := d.ListChildren(ctx, parent)
	if err != nil {
		return "", err
	}
	for _, k := range kids {
		if k.IsFolder() && k.Name == name {
			return k.ID, nil
		}
	}
	return "", nil
}

// SafeName rejects names that could escape the target folder or are not
// valid file names on Linux. Drive allows "/" and ".." in names; we don't.
func SafeName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00")
}

// Send sends one request without retries (resumable upload chunks), and
// refreshes the sign-in if Google rejects it, so the caller's retry works.
func (d *Drive) Send(ctx context.Context, method, rawURL string, body []byte, hdr http.Header) (*Response, error) {
	resp, tok, err := d.once(ctx, method, rawURL, body, hdr)
	if err != nil {
		return nil, err
	}
	if resp.Status == http.StatusUnauthorized {
		if err := d.auth.Rejected(ctx, tok); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// UploadURL is the base address for uploads ("…/upload/drive/v3").
func (d *Drive) UploadURL() string { return d.ep.Upload }

// Sent and Received count file bytes moved, for the speed display.
func (d *Drive) Sent() int64     { return d.sent.Load() }
func (d *Drive) Received() int64 { return d.received.Load() }

// Retry is the retry policy the client was made with.
func (d *Drive) Retry() RetryPolicy { return d.retry }

// Printer is where sign-in messages go.
type Printer interface {
	Printf(format string, a ...any)
	Println(msg string)
}

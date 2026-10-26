package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
)

// Google forgets unfinished upload sessions after about a week.
const sessionMaxAge = 6 * 24 * time.Hour

type uploader struct {
	d       *gdrive.Drive
	st      *State
	prog    *Progress
	chunk   int64
	retry   gdrive.RetryPolicy
	resumed atomic.Int64
}

func fileMetadata(f *LocalFile) map[string]any {
	return map[string]any{
		"name":         f.Name,
		"parents":      []string{f.ParentID},
		"modifiedTime": f.ModTime().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
}

// uploadOne uploads one file and checks Drive's copy against the local MD5,
// retrying the whole file a few times if needed.
func (u *uploader) uploadOne(ctx context.Context, f *LocalFile) error {
	var lastErr error
	for attempt := 1; attempt <= u.retry.FileAttempts; attempt++ {
		err := u.tryOnce(ctx, f)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fe *gdrive.FatalError
		var le *gdrive.LocalError
		var ae *gdrive.APIError
		switch {
		case errors.As(err, &fe), errors.As(err, &le):
			return err
		case errors.As(err, &ae) && (ae.Reason == "storageQuotaExceeded" || ae.Reason == "quotaExceeded"):
			return gdrive.Fatalf("Google Drive is full. Free up space or upgrade storage, then run again")
		case errors.As(err, &ae) && !ae.Retryable() && ae.Status != 404 && ae.Status != 410:
			return err
		}
		lastErr = err
		slog.Warn("file attempt failed", "file", f.Rel, "attempt", attempt, "err", err)
		if attempt < u.retry.FileAttempts {
			if err := gdrive.SleepCtx(ctx, u.retry.FilePause); err != nil {
				return err
			}
		}
	}
	return lastErr
}

func (u *uploader) tryOnce(ctx context.Context, f *LocalFile) error {
	localMD5, err := u.st.MD5(f)
	if err != nil {
		return err
	}
	var got gdrive.File
	if f.Size <= u.chunk {
		got, err = u.uploadSmall(ctx, f)
	} else {
		got, err = u.uploadResumable(ctx, f)
	}
	if err != nil {
		return err
	}
	if got.MD5 == "" || got.Size == "" {
		if got, err = u.d.GetFile(ctx, got.ID); err != nil {
			return err
		}
	}
	md5OK := got.MD5 == localMD5 || (f.Size == 0 && got.MD5 == "")
	if got.SizeBytes() != f.Size || !md5OK {
		if err := u.d.Trash(ctx, got.ID); err != nil {
			return err
		}
		return &gdrive.IntegrityError{Msg: fmt.Sprintf("Drive's copy did not match (size %s, md5 %s; local md5 %s); moved it to Drive's trash",
			got.Size, got.MD5, localMD5)}
	}
	for _, id := range f.ReplaceIDs {
		if err := u.d.Trash(ctx, id); err != nil {
			return err
		}
	}
	f.ReplaceIDs = nil
	return nil
}

// uploadSmall sends a small file and its details in one request (Drive's
// "multipart" upload). Saves a round trip per file compared to a session.
func (u *uploader) uploadSmall(ctx context.Context, f *LocalFile) (gdrive.File, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return gdrive.File{}, &gdrive.LocalError{Err: err}
	}
	if int64(len(data)) != f.Size {
		return gdrive.File{}, &gdrive.LocalError{Err: fmt.Errorf("%s changed while uploading", f.Path)}
	}
	meta, _ := json.Marshal(fileMetadata(f))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	part.Write(meta)
	part, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {f.Mime}})
	part.Write(data)
	mw.Close()

	resp, err := u.d.Call(ctx, "POST", u.d.UploadURL()+"/files?uploadType=multipart&fields="+gdrive.FileFields,
		buf.Bytes(), gdrive.Headers("Content-Type", "multipart/related; boundary="+mw.Boundary()))
	if err != nil {
		return gdrive.File{}, err
	}
	var got gdrive.File
	if err := json.Unmarshal(resp.Body, &got); err != nil || got.ID == "" {
		return gdrive.File{}, fmt.Errorf("unexpected upload reply from Google")
	}
	u.prog.Set(f, f.Size)
	return got, nil
}

type chunkResult int

const (
	chunkPartial chunkResult = iota // Google has some bytes; continue from the offset
	chunkDone                       // file complete
	chunkExpired                    // session gone; start the file over
)

// uploadResumable sends a large file in chunks over a resumable session.
func (u *uploader) uploadResumable(ctx context.Context, f *LocalFile) (gdrive.File, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return gdrive.File{}, &gdrive.LocalError{Err: err}
	}
	defer fh.Close()

	uri, offset, done, err := u.resumeOrStart(ctx, f)
	if err != nil {
		return gdrive.File{}, err
	}
	u.prog.Set(f, offset)
	buf := make([]byte, u.chunk)
	r := gdrive.NewRetrier(u.retry)
	restarts := 0

	for done == nil {
		var kind chunkResult
		var newOffset int64
		var file *gdrive.File
		if offset >= f.Size {
			// Google has every byte, but its final reply was lost: ask again.
			if err := r.Wait(ctx, errors.New("upload did not finish on Google's side")); err != nil {
				return gdrive.File{}, err
			}
			if kind, newOffset, file, err = u.status(ctx, uri, f.Size); err != nil {
				return gdrive.File{}, err
			}
		} else {
			n := min(u.chunk, f.Size-offset)
			if got, err := fh.ReadAt(buf[:n], offset); int64(got) != n {
				return gdrive.File{}, &gdrive.LocalError{Err: fmt.Errorf("reading %s: %v (file changed while uploading?)", f.Path, err)}
			}
			kind, newOffset, file, err = u.sendChunk(ctx, uri, f.Size, offset, buf[:n])
			if err != nil {
				if !gdrive.IsRetryable(err) {
					return gdrive.File{}, err
				}
				slog.Warn("chunk failed, retrying", "file", f.Rel, "offset", offset, "err", err)
				if werr := r.Wait(ctx, err); werr != nil {
					return gdrive.File{}, werr
				}
				// Ask Google how much actually arrived before sending more.
				if kind, newOffset, file, err = u.status(ctx, uri, f.Size); err != nil {
					return gdrive.File{}, err
				}
			}
		}

		switch kind {
		case chunkDone:
			done = file
		case chunkExpired:
			if restarts++; restarts > 3 {
				return gdrive.File{}, &gdrive.APIError{Status: 404, Message: "upload session kept expiring"}
			}
			slog.Warn("upload session expired, starting file over", "file", f.Rel)
			if uri, err = u.start(ctx, f); err != nil {
				return gdrive.File{}, err
			}
			offset = 0
			u.prog.Set(f, 0)
		case chunkPartial:
			if newOffset > offset {
				r.Reset()
			}
			offset = newOffset
			u.prog.Set(f, offset)
		}
	}
	if err := u.st.ClearSession(f.Rel); err != nil {
		slog.Warn("cannot update session file", "err", err)
	}
	return *done, nil
}

func (u *uploader) resumeOrStart(ctx context.Context, f *LocalFile) (uri string, offset int64, done *gdrive.File, err error) {
	if s, ok := u.st.Session(f.Rel); ok && s.Size == f.Size && s.MtimeNs == f.MtimeNs &&
		s.Parent == f.ParentID && time.Since(s.Created) < sessionMaxAge {
		kind, off, file, err := u.status(ctx, s.URI, f.Size)
		switch {
		case ctx.Err() != nil:
			return "", 0, nil, ctx.Err()
		case err != nil:
			slog.Warn("cannot resume earlier upload", "file", f.Rel, "err", err)
		case kind == chunkPartial:
			slog.Info("resuming upload", "file", f.Rel, "offset", off)
			u.resumed.Add(1)
			return s.URI, off, nil, nil
		case kind == chunkDone:
			u.resumed.Add(1)
			return s.URI, f.Size, file, nil
		}
	}
	uri, err = u.start(ctx, f)
	return uri, 0, nil, err
}

func (u *uploader) start(ctx context.Context, f *LocalFile) (string, error) {
	meta, _ := json.Marshal(fileMetadata(f))
	resp, err := u.d.Call(ctx, "POST", u.d.UploadURL()+"/files?uploadType=resumable&fields="+gdrive.FileFields, meta,
		gdrive.Headers("Content-Type", "application/json; charset=UTF-8",
			"X-Upload-Content-Type", f.Mime,
			"X-Upload-Content-Length", strconv.FormatInt(f.Size, 10)))
	if err != nil {
		return "", err
	}
	uri := resp.Header.Get("Location")
	if uri == "" {
		return "", errors.New("Google did not return an upload address")
	}
	err = u.st.SetSession(f.Rel, session{URI: uri, Size: f.Size, MtimeNs: f.MtimeNs, Parent: f.ParentID, Created: time.Now()})
	if err != nil {
		slog.Warn("cannot save upload session", "err", err)
	}
	return uri, nil
}

// sendChunk sends one chunk, once. The caller decides what to do on failure.
func (u *uploader) sendChunk(ctx context.Context, uri string, total, offset int64, chunk []byte) (chunkResult, int64, *gdrive.File, error) {
	hdr := gdrive.Headers("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(chunk))-1, total))
	resp, err := u.d.Send(ctx, "PUT", uri, chunk, hdr)
	if err != nil {
		return 0, 0, nil, err
	}
	return interpretUpload(resp)
}

// status asks Google how many bytes of the session it has.
func (u *uploader) status(ctx context.Context, uri string, total int64) (chunkResult, int64, *gdrive.File, error) {
	resp, err := u.d.Call(ctx, "PUT", uri, nil, gdrive.Headers("Content-Range", fmt.Sprintf("bytes */%d", total)),
		200, 201, 308, 404, 410)
	if err != nil {
		return 0, 0, nil, err
	}
	return interpretUpload(resp)
}

func interpretUpload(resp *gdrive.Response) (chunkResult, int64, *gdrive.File, error) {
	switch resp.Status {
	case 200, 201:
		var f gdrive.File
		if err := json.Unmarshal(resp.Body, &f); err != nil || f.ID == "" {
			return 0, 0, nil, &gdrive.NetError{Err: errors.New("incomplete upload reply from Google")}
		}
		return chunkDone, 0, &f, nil
	case 308:
		// "Range: bytes=0-12345" means bytes 0..12345 arrived. No header: none did.
		rng := resp.Header.Get("Range")
		if rng == "" {
			return chunkPartial, 0, nil, nil
		}
		i := strings.LastIndexByte(rng, '-')
		if i < 0 {
			return 0, 0, nil, fmt.Errorf("unexpected Range reply %q", rng)
		}
		end, err := strconv.ParseInt(rng[i+1:], 10, 64)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("unexpected Range reply %q", rng)
		}
		return chunkPartial, end + 1, nil, nil
	case 404, 410:
		return chunkExpired, 0, nil, nil
	}
	return 0, 0, nil, gdrive.ParseAPIError(resp)
}

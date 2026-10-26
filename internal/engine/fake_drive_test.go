package engine

import (
	"crypto/md5"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snehilshah/dhaara/internal/gdrive"
)

// fakeDrive is an in-memory stand-in for the Drive API that can misbehave on
// purpose: random 5xx/429/401 replies, dropped connections, half-stored
// chunks, lost replies, stalls, expired sessions and corrupted checksums.
type fakeDrive struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	rng      *rand.Rand
	files    map[string]*fakeFile
	order    []string // creation order, for orderBy=createdTime
	sessions map[string]*fakeSession
	tokens   map[string]bool
	nextID   int
	counts   map[string]int

	// knobs
	failRate   float64 // chance of a generic failure on any API call
	chunkChaos bool    // drop/half-store/lose replies on uploads
	limit      int64
	dlChaos    bool                       // cut downloads short, ignore Range, corrupt once
	hangMedia  time.Duration              // stall one download mid-body this long
	onMedia    func(n, size int)          // called (without the lock) mid-body for each download
	revoked    bool                       // refresh token rejected with invalid_grant
	hangOnce   time.Duration              // stall one chunk reply this long
	onCopy     func(n int)                // called (without the lock) after each copy
	onTrash    func(f *fakeFile)          // called (with the lock held) when a file is trashed
	onChunk    func(n int, complete bool) // called (without the lock) after each stored chunk
}

type fakeFile struct {
	ID, Name, Mime, Parent, MD5, ModifiedTime string
	Data                                      []byte
	Trashed                                   bool
}

type fakeSession struct {
	meta  map[string]any
	mime  string
	size  int64
	buf   []byte
	done  *fakeFile
	reply []byte
}

func newFakeDrive(t *testing.T) *fakeDrive {
	fd := &fakeDrive{
		t: t, rng: rand.New(rand.NewPCG(1, 2)),
		files: map[string]*fakeFile{}, sessions: map[string]*fakeSession{},
		tokens: map[string]bool{}, counts: map[string]int{}, limit: 1 << 40,
	}
	fd.srv = httptest.NewServer(http.HandlerFunc(fd.serve))
	t.Cleanup(fd.srv.Close)
	return fd
}

// newFakeDriveTLS serves over HTTPS and offers HTTP/2, like Google does.
func newFakeDriveTLS(t *testing.T) *fakeDrive {
	fd := newFakeDrive(t)
	fd.srv.Close()
	fd.srv = httptest.NewUnstartedServer(http.HandlerFunc(fd.serve))
	fd.srv.EnableHTTP2 = true
	fd.srv.StartTLS()
	t.Cleanup(fd.srv.Close)
	return fd
}

func (fd *fakeDrive) roots() *x509.CertPool {
	if fd.srv.TLS == nil {
		return nil
	}
	pool := x509.NewCertPool()
	pool.AddCert(fd.srv.Certificate())
	return pool
}

func (fd *fakeDrive) endpoints() gdrive.Endpoints {
	u := fd.srv.URL
	return gdrive.Endpoints{Auth: u + "/auth", Token: u + "/token", API: u + "/drive/v3", Upload: u + "/upload/drive/v3"}
}

func (fd *fakeDrive) count(k string) int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.counts[k]
}

func (fd *fakeDrive) snapshotCounts() map[string]int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	out := map[string]int{}
	for k, v := range fd.counts {
		out[k] = v
	}
	return out
}

func (fd *fakeDrive) roll(p float64) bool { return fd.rng.Float64() < p }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, reason, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": msg, "errors": []map[string]string{{"reason": reason}},
	}})
}

// drop closes the connection without answering, like a Wi-Fi drop.
func drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func (fd *fakeDrive) meta(f *fakeFile) map[string]any {
	m := map[string]any{"id": f.ID, "name": f.Name, "mimeType": f.Mime}
	if f.Mime != gdrive.FolderMime {
		m["size"] = strconv.Itoa(len(f.Data))
		m["md5Checksum"] = f.MD5
	}
	if f.ModifiedTime != "" {
		m["modifiedTime"] = f.ModifiedTime
	}
	return m
}

func (fd *fakeDrive) create(meta map[string]any, data []byte, mimeType string) *fakeFile {
	fd.nextID++
	id := fmt.Sprintf("f%d", fd.nextID)
	parents, _ := meta["parents"].([]any)
	if mt, ok := meta["mimeType"].(string); ok {
		mimeType = mt
	}
	sum := md5.Sum(data)
	f := &fakeFile{ID: id, Name: meta["name"].(string), Mime: mimeType, Parent: parents[0].(string),
		Data: data, MD5: hex.EncodeToString(sum[:])}
	f.ModifiedTime, _ = meta["modifiedTime"].(string)
	fd.files[id] = f
	fd.order = append(fd.order, id)
	return f
}

var parentRe = regexp.MustCompile(`'([^']+)' in parents`)

func (fd *fakeDrive) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return // client went away mid-request; a real server discards it too
	}
	fd.mu.Lock()
	if r.ProtoMajor != 1 {
		fd.counts["http2"]++
	}

	if r.URL.Path == "/token" {
		defer fd.mu.Unlock()
		form, _ := parseForm(string(body))
		if form["grant_type"] == "refresh_token" && fd.revoked {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
			return
		}
		fd.counts["token"]++
		tok := fmt.Sprintf("tok-%d", fd.counts["token"])
		fd.tokens[tok] = true
		writeJSON(w, 200, map[string]any{"access_token": tok, "expires_in": 3600})
		return
	}
	if !fd.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		fd.mu.Unlock()
		apiErr(w, 401, "authError", "invalid token")
		return
	}
	if fd.roll(fd.failRate) {
		fd.counts["chaos"]++
		kind := fd.rng.IntN(5)
		fd.mu.Unlock()
		switch kind {
		case 0:
			drop(w)
		case 1:
			w.Header().Set("Retry-After", "0")
			apiErr(w, 429, "rateLimitExceeded", "slow down")
		case 2:
			apiErr(w, 403, "userRateLimitExceeded", "slow down")
		case 3:
			apiErr(w, 401, "authError", "token expired")
		default:
			apiErr(w, 503, "backendError", "try again")
		}
		return
	}

	path, q := r.URL.Path, r.URL.Query()
	switch {
	case path == "/drive/v3/about":
		var used int64
		for _, f := range fd.files {
			used += int64(len(f.Data))
		}
		fd.mu.Unlock()
		writeJSON(w, 200, map[string]any{"user": map[string]string{"emailAddress": "test@example.com"},
			"storageQuota": map[string]string{"limit": strconv.FormatInt(fd.limit, 10), "usage": strconv.FormatInt(used, 10)}})

	case path == "/drive/v3/files" && r.Method == "GET":
		parent := parentRe.FindStringSubmatch(q.Get("q"))[1]
		var kids []map[string]any
		for _, id := range fd.order {
			if f := fd.files[id]; f.Parent == parent && !f.Trashed {
				kids = append(kids, fd.meta(f))
			}
		}
		start, _ := strconv.Atoi(q.Get("pageToken"))
		end := min(start+3, len(kids)) // tiny pages, to exercise paging
		out := map[string]any{"files": kids[start:end]}
		if end < len(kids) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
		fd.mu.Unlock()
		writeJSON(w, 200, out)

	case path == "/drive/v3/files" && r.Method == "POST":
		var meta map[string]any
		json.Unmarshal(body, &meta)
		f := fd.create(meta, nil, "application/octet-stream")
		fd.mu.Unlock()
		writeJSON(w, 200, fd.meta(f))

	case strings.HasPrefix(path, "/drive/v3/files/") && strings.HasSuffix(path, "/copy") && r.Method == "POST":
		src := fd.files[strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/copy")]
		if src == nil || src.Mime == gdrive.FolderMime {
			fd.mu.Unlock()
			apiErr(w, 404, "notFound", "no such file")
			return
		}
		var meta map[string]any
		json.Unmarshal(body, &meta)
		if _, ok := meta["modifiedTime"]; !ok {
			meta["modifiedTime"] = src.ModifiedTime
		}
		f := fd.create(meta, append([]byte(nil), src.Data...), src.Mime)
		fd.counts["copy"]++
		notify := fd.onCopy
		m := fd.meta(f)
		fd.mu.Unlock()
		if notify != nil {
			notify(fd.count("copy"))
		}
		writeJSON(w, 200, m)

	case strings.HasPrefix(path, "/upload/drive/v3/files/") && r.Method == "PATCH" && q.Get("uploadType") == "media":
		f := fd.files[strings.TrimPrefix(path, "/upload/drive/v3/files/")]
		if f == nil {
			fd.mu.Unlock()
			apiErr(w, 404, "notFound", "no such file")
			return
		}
		f.Data = append([]byte(nil), body...)
		sum := md5.Sum(f.Data)
		f.MD5 = hex.EncodeToString(sum[:])
		fd.counts["update"]++
		m := fd.meta(f)
		fd.mu.Unlock()
		writeJSON(w, 200, m)

	case strings.HasPrefix(path, "/drive/v3/files/"):
		f := fd.files[strings.TrimPrefix(path, "/drive/v3/files/")]
		if f == nil {
			fd.mu.Unlock()
			apiErr(w, 404, "notFound", "no such file")
			return
		}
		if r.Method == "GET" && q.Get("alt") == "media" {
			fd.media(w, r, f) // unlocks
			return
		}
		if r.Method == "PATCH" {
			var upd map[string]any
			json.Unmarshal(body, &upd)
			if upd["trashed"] == true {
				f.Trashed = true
				fd.counts["trash"]++
				if fd.onTrash != nil {
					fd.onTrash(f)
				}
			}
			if name, ok := upd["name"].(string); ok && name != f.Name {
				f.Name = name
				fd.counts["rename"]++
			}
			if add := q.Get("addParents"); add != "" && q.Get("removeParents") == f.Parent {
				f.Parent = add
				fd.counts["move"]++
			}
		}
		m := fd.meta(f)
		fd.mu.Unlock()
		writeJSON(w, 200, m)

	case path == "/upload/drive/v3/files" && r.Method == "POST" && q.Get("uploadType") == "multipart":
		fd.multipart(w, r, body)

	case path == "/upload/drive/v3/files" && r.Method == "POST" && q.Get("uploadType") == "resumable":
		var meta map[string]any
		json.Unmarshal(body, &meta)
		size, _ := strconv.ParseInt(r.Header.Get("X-Upload-Content-Length"), 10, 64)
		fd.nextID++
		sid := strconv.Itoa(fd.nextID)
		fd.sessions[sid] = &fakeSession{meta: meta, mime: r.Header.Get("X-Upload-Content-Type"), size: size}
		fd.counts["initiate"]++
		fd.mu.Unlock()
		w.Header().Set("Location", fd.srv.URL+"/upload/drive/v3/files?uploadType=resumable&upload_id="+sid)
		w.WriteHeader(200)

	case path == "/upload/drive/v3/files" && r.Method == "PUT":
		fd.chunk(w, r, q.Get("upload_id"), body) // unlocks

	default:
		fd.mu.Unlock()
		apiErr(w, 400, "badRequest", "unknown "+r.Method+" "+path)
	}
}

// multipart handles single-request uploads. Called with the lock held.
func (fd *fakeDrive) multipart(w http.ResponseWriter, r *http.Request, body []byte) {
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	p1, err1 := mr.NextPart()
	metaRaw, _ := io.ReadAll(p1)
	p2, err2 := mr.NextPart()
	if err1 != nil || err2 != nil {
		fd.mu.Unlock()
		apiErr(w, 400, "badRequest", "bad multipart body")
		return
	}
	data, _ := io.ReadAll(p2)
	var meta map[string]any
	json.Unmarshal(metaRaw, &meta)
	f := fd.create(meta, data, p2.Header.Get("Content-Type"))
	if strings.HasSuffix(f.Name, " removed.json") {
		fd.counts["multipart_bookkeeping"]++
		m := fd.meta(f)
		fd.mu.Unlock()
		writeJSON(w, 200, m)
		return
	}
	fd.counts["multipart"]++
	lose := fd.chunkChaos && (fd.roll(0.1) || fd.counts["multipart"] == 2)
	if lose {
		fd.counts["multipart_lost_reply"]++ // stored, but the client never hears
	}
	m := fd.meta(f)
	fd.mu.Unlock()
	if lose {
		drop(w)
		return
	}
	writeJSON(w, 200, m)
}

// media serves file content with Range support. Called with the lock held; unlocks.
func (fd *fakeDrive) media(w http.ResponseWriter, r *http.Request, f *fakeFile) {
	data := f.Data
	if strings.HasSuffix(f.Name, " removed.json") { // dhaara's own bookkeeping, not music
		fd.counts["media_bookkeeping"]++
		fd.mu.Unlock()
		w.Write(data)
		return
	}
	fd.counts["media"]++
	n := fd.counts["media"]
	start := 0
	ranged := false
	if rg := r.Header.Get("Range"); rg != "" {
		v, _ := strings.CutPrefix(rg, "bytes=")
		v, _, _ = strings.Cut(v, "-")
		start, _ = strconv.Atoi(v)
		ranged = true
	}
	roll := fd.rng.Float64()
	if fd.dlChaos && (roll < 0.05 || n == 3) {
		ranged, start = false, 0 // pretend we never saw the Range header
		fd.counts["ignored_range"]++
	}
	if fd.dlChaos && fd.counts["dl_corrupt"] == 0 && len(data) > 10 && start == 0 {
		data = append([]byte(nil), data...)
		data[5] ^= 0xff
		fd.counts["dl_corrupt"]++
	}
	cut := fd.dlChaos && ((roll >= 0.05 && roll < 0.15) || n == 2)
	if cut {
		fd.counts["dl_cut"]++
	}
	hang := fd.hangMedia
	fd.hangMedia = 0
	if hang > 0 {
		fd.counts["dl_hang"]++
	}
	notify := fd.onMedia
	fd.mu.Unlock()

	if start > len(data) || (ranged && start == len(data)) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
		w.WriteHeader(416)
		return
	}
	body := data[start:]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.WriteHeader(206)
	} else {
		w.WriteHeader(200)
	}
	half := len(body) / 2
	w.Write(body[:half])
	w.(http.Flusher).Flush()
	if notify != nil {
		notify(n, len(data))
	}
	if hang > 0 {
		time.Sleep(hang)
	}
	if cut {
		drop(w)
		return
	}
	w.Write(body[half:])
}

var rangeRe = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

// chunk handles resumable PUTs. Called with the lock held; unlocks.
func (fd *fakeDrive) chunk(w http.ResponseWriter, r *http.Request, sid string, body []byte) {
	s := fd.sessions[sid]
	if s == nil || (fd.chunkChaos && fd.roll(0.01)) {
		delete(fd.sessions, sid)
		fd.counts["expired"]++
		fd.mu.Unlock()
		apiErr(w, 404, "notFound", "upload session expired")
		return
	}
	cr := r.Header.Get("Content-Range")
	var hang time.Duration
	var notify func(int, bool)
	complete := false
	n := 0
	if s.done == nil && !strings.HasPrefix(cr, "bytes */") {
		m := rangeRe.FindStringSubmatch(cr)
		if m == nil {
			fd.mu.Unlock()
			apiErr(w, 400, "badRequest", "bad Content-Range "+cr)
			return
		}
		a, _ := strconv.ParseInt(m[1], 10, 64)
		b, _ := strconv.ParseInt(m[2], 10, 64)
		total, _ := strconv.ParseInt(m[3], 10, 64)
		last := b+1 == total
		problem := ""
		switch {
		case total != s.size || b-a+1 != int64(len(body)):
			problem = fmt.Sprintf("bad chunk: range %s, body %d, size %d", cr, len(body), s.size)
		case !last && len(body)%(256<<10) != 0:
			problem = fmt.Sprintf("chunk not a multiple of 256 KiB: %d", len(body))
		case a > int64(len(s.buf)):
			problem = fmt.Sprintf("client skipped bytes: sent from %d but server has %d", a, len(s.buf))
		}
		if problem != "" {
			fd.t.Error(problem)
			fd.mu.Unlock()
			apiErr(w, 400, "badRequest", problem)
			return
		}
		fd.counts["chunks"]++
		n = fd.counts["chunks"]
		notify = fd.onChunk
		roll := fd.rng.Float64()
		if fd.chunkChaos && ((roll < 0.08 && n != 4) || n == 2) { // half the chunk arrives, then the line drops
			s.buf = append(s.buf[:a], body[:len(body)/2]...)
			fd.counts["half_drop"]++
			fd.mu.Unlock()
			drop(w)
			return
		}
		s.buf = append(s.buf[:a], body...)
		if int64(len(s.buf)) == s.size {
			f := fd.create(s.meta, s.buf, s.mime)
			if fd.chunkChaos && (fd.counts["corrupt"] == 0 || fd.roll(0.03)) {
				f.MD5 = strings.Repeat("0", 32) // Drive's copy "corrupted"
				fd.counts["corrupt"]++
			}
			s.done = f
			complete = true
			s.reply, _ = json.Marshal(fd.meta(f))
		}
		if fd.hangOnce > 0 {
			hang, fd.hangOnce = fd.hangOnce, 0
			fd.counts["hang"]++
		}
		if fd.chunkChaos && ((roll >= 0.08 && roll < 0.14) || n == 4) { // stored, but the reply is lost
			fd.counts["lost_reply"]++
			fd.mu.Unlock()
			if notify != nil {
				notify(n, complete)
			}
			drop(w)
			return
		}
	}
	done, reply, have := s.done, s.reply, len(s.buf)
	fd.mu.Unlock()
	if notify != nil {
		notify(n, complete)
	}
	if hang > 0 {
		time.Sleep(hang) // the client should give up on us and ask for status
	}
	if done != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(reply)
		return
	}
	if have > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", have-1))
	}
	w.WriteHeader(308)
}

func parseForm(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, "&") {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out, nil
}

// tree returns {relative path: content} for non-trashed files under
// My Drive/<dest>, failing the test on duplicates.
func (fd *fakeDrive) tree(t *testing.T, dest string) map[string][]byte {
	t.Helper()
	fd.mu.Lock()
	defer fd.mu.Unlock()
	var roots []*fakeFile
	for _, id := range fd.order {
		if f := fd.files[id]; f.Parent == "root" && f.Name == dest && !f.Trashed {
			roots = append(roots, f)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("expected one %q folder on Drive, found %d", dest, len(roots))
	}
	out := map[string][]byte{}
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		for _, fid := range fd.order {
			f := fd.files[fid]
			if f.Parent != id || f.Trashed {
				continue
			}
			p := f.Name
			if prefix != "" {
				p = prefix + "/" + f.Name
			}
			if f.Mime == gdrive.FolderMime {
				walk(f.ID, p)
				continue
			}
			if _, dup := out[p]; dup {
				t.Errorf("duplicate on Drive: %s", p)
			}
			out[p] = f.Data
		}
	}
	walk(roots[0].ID, "")
	return out
}

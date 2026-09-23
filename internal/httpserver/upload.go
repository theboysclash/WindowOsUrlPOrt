package httpserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// maxISOSize bounds a single uploaded image.
const maxISOSize = 32 << 30

// maxChunkSize stays under the 100 MB request body limit of Cloudflare
// tunnels so uploads work through every sharing method.
const maxChunkSize = 64 << 20

var uploadMu sync.Mutex

func (s *Server) isoDir() string {
	return filepath.Join(filepath.Dir(s.vm.Config().DiskPath), "isos")
}

// cleanISOName reduces a browser-supplied file name to a safe base name.
func cleanISOName(name string) (string, error) {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if !strings.HasSuffix(strings.ToLower(out), ".iso") || len(out) < 5 {
		return "", errors.New("only .iso files can be uploaded")
	}
	if len(out) > 128 {
		out = out[:124] + ".iso"
	}
	return out, nil
}

// apiUploadISO receives one chunk of an ISO. The browser sends chunks in
// order with ?name=&offset=&total=; offset 0 starts a new upload and the file
// is moved into place once offset+len(chunk) == total. Re-sending the chunk
// at the current size is accepted so a failed request can be retried.
func (s *Server) apiUploadISO(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, err := cleanISOName(q.Get("name"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	offset, err1 := strconv.ParseInt(q.Get("offset"), 10, 64)
	total, err2 := strconv.ParseInt(q.Get("total"), 10, 64)
	if err1 != nil || err2 != nil || offset < 0 || total <= 0 || offset > total {
		writeJSON(w, 400, map[string]string{"error": "bad offset or total"})
		return
	}
	if total > maxISOSize {
		writeJSON(w, 400, map[string]string{"error": "file is larger than 32 GB"})
		return
	}

	dir := s.isoDir()
	final := filepath.Join(dir, name)
	part := final + ".part"

	uploadMu.Lock()
	defer uploadMu.Unlock()

	cur := s.vm.Config()
	if st := s.vm.Status().State; (st == "running" || st == "starting") && (cur.ISOPath == final || cur.VirtioISOPath == final) {
		writeJSON(w, 409, map[string]string{"error": "that ISO is attached to the running VM; stop the VM or rename the file"})
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, err)
		return
	}

	flags := os.O_WRONLY | os.O_CREATE
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		writeErr(w, err)
		return
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		writeErr(w, err)
		return
	}
	if offset != size {
		// Retry of an already-written chunk: drop the tail so the retry
		// overwrites it. Any other gap means the client and server disagree.
		if offset < size && size-offset <= maxChunkSize {
			if err := f.Truncate(offset); err != nil {
				f.Close()
				writeErr(w, err)
				return
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				f.Close()
				writeErr(w, err)
				return
			}
		} else {
			f.Close()
			writeJSON(w, 409, map[string]any{"error": "upload out of sync", "have": size})
			return
		}
	}

	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxChunkSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("upload interrupted: %v", err)})
		return
	}
	if n > maxChunkSize {
		writeJSON(w, 400, map[string]string{"error": "chunk too large"})
		return
	}
	got := offset + n
	if got > total {
		os.Remove(part)
		writeJSON(w, 400, map[string]string{"error": "received more data than announced"})
		return
	}
	if got < total {
		writeJSON(w, 200, map[string]any{"received": got, "done": false})
		return
	}

	if err := os.Rename(part, final); err != nil {
		writeErr(w, err)
		return
	}
	sess := sessionFrom(r.Context())
	s.log.Info("iso uploaded", "user", sess.Username, "path", final, "bytes", total)
	writeJSON(w, 200, map[string]any{"received": got, "done": true, "path": final, "name": name})
}

type isoInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// apiListISOs lists images previously uploaded to the server.
func (s *Server) apiListISOs(w http.ResponseWriter, r *http.Request) {
	dir := s.isoDir()
	entries, err := os.ReadDir(dir)
	list := []isoInfo{}
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".iso") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			list = append(list, isoInfo{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Size: info.Size()})
		}
	}
	writeJSON(w, 200, list)
}

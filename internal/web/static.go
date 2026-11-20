// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// staticPrefix is the URL prefix of the embedded assets.
const staticPrefix = "/static/"

// contentTypes maps asset extensions to explicit Content-Types (no sniffing,
// no dependence on the host's mime database).
var contentTypes = map[string]string{
	".js":  "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".svg": "image/svg+xml",
	".txt": "text/plain; charset=utf-8",
	".md":  "text/plain; charset=utf-8",
}

type asset struct {
	data        []byte
	contentType string
	etag        string // quoted
	version     string // first 12 hex digits of the SHA-256
}

type assets struct {
	files map[string]asset // key: path below static/
}

func loadAssets() (*assets, error) {
	a := &assets{files: map[string]asset{}}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		ct, ok := contentTypes[path.Ext(p)]
		if !ok {
			return fmt.Errorf("static asset %s has no known content type", p)
		}
		sum := sha256.Sum256(data)
		hexSum := hex.EncodeToString(sum[:])
		a.files[strings.TrimPrefix(p, "static/")] = asset{
			data: data, contentType: ct, etag: `"` + hexSum + `"`, version: hexSum[:12],
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load assets: %w", err)
	}
	return a, nil
}

// url returns the URL of an asset with its content hash in the query
// string, so that the browser may cache it for a year and still get a new
// version after an upgrade. An unknown name is a programming error caught
// by the template tests; it yields the plain path.
func (a *assets) url(name string) string {
	f, ok := a.files[name]
	if !ok {
		return staticPrefix + name
	}
	return staticPrefix + name + "?v=" + f.version
}

// serveStatic serves the embedded assets: GET/HEAD only, explicit
// Content-Type, ETag, and a one-year immutable cache when the URL carries
// the matching ?v= hash (otherwise "no-cache": revalidate with the ETag).
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, staticPrefix)
	f, ok := s.assets.files[name]
	if !ok {
		s.notFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("ETag", f.etag)
	if r.URL.Query().Get("v") == f.version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if etagMatches(r.Header.Get("If-None-Match"), f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", fmt.Sprint(len(f.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(f.data)
	}
}

// etagMatches implements the weak comparison of If-None-Match.
func etagMatches(header, etag string) bool {
	for _, c := range strings.Split(header, ",") {
		c = strings.TrimPrefix(strings.TrimSpace(c), "W/")
		if c == "*" || c == etag {
			return true
		}
	}
	return false
}

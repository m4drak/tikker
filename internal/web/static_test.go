// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestVendoredAssetsPinned fails when a vendored file changes without
// updating static/VENDOR.md (which documents where the files come from).
func TestVendoredAssetsPinned(t *testing.T) {
	want := map[string]string{
		"static/htmx.min.js":  "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717",
		"static/pico.min.css": "fbc9a63fc9fc9f72d12fd7fc9806e11fa9f77ae4f9cad146b27003a1119ba3db",
	}
	vendor, err := staticFS.ReadFile("static/VENDOR.md")
	if err != nil {
		t.Fatal(err)
	}
	for file, sum := range want {
		data, err := staticFS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("%s: sha256 %x does not match the pinned %s", file, got, sum)
		}
		if !strings.Contains(string(vendor), sum) {
			t.Errorf("VENDOR.md does not list the sum of %s", file)
		}
	}
	for _, f := range []string{"static/LICENSES/htmx-0BSD.txt", "static/LICENSES/pico-MIT.txt"} {
		if data, err := staticFS.ReadFile(f); err != nil || len(data) < 200 {
			t.Errorf("licence file %s missing or empty (%v)", f, err)
		}
	}
}

func TestStaticContentTypes(t *testing.T) {
	e := newEnv(t, allRoles)
	tests := []struct{ path, ct string }{
		{"/static/htmx.min.js", "text/javascript; charset=utf-8"},
		{"/static/app.js", "text/javascript; charset=utf-8"},
		{"/static/pico.min.css", "text/css; charset=utf-8"},
		{"/static/app.css", "text/css; charset=utf-8"},
		{"/static/favicon.svg", "image/svg+xml"},
		{"/static/VENDOR.md", "text/plain; charset=utf-8"},
		{"/static/LICENSES/htmx-0BSD.txt", "text/plain; charset=utf-8"},
		{"/static/LICENSES/pico-MIT.txt", "text/plain; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := e.do(http.MethodGet, tt.path, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != tt.ct {
				t.Errorf("Content-Type = %q, want %q", got, tt.ct)
			}
			if w.Header().Get("ETag") == "" || w.Body.Len() == 0 {
				t.Error("no ETag or empty body")
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("no nosniff")
			}
		})
	}
}

func TestStaticCaching(t *testing.T) {
	e := newEnv(t, allRoles)
	// The layout links the assets with their hash.
	body := e.do(http.MethodGet, "/login", nil).Body.String()
	m := regexp.MustCompile(`href="(/static/pico\.min\.css\?v=[0-9a-f]{12})"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no versioned asset link in\n%s", body)
	}

	w := e.do(http.MethodGet, m[1], nil)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("versioned Cache-Control = %q", got)
	}
	etag := w.Header().Get("ETag")

	// Without the version (or with a stale one) the browser revalidates.
	for _, p := range []string{"/static/pico.min.css", "/static/pico.min.css?v=000000000000"} {
		if got := e.do(http.MethodGet, p, nil).Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q", p, got)
		}
	}

	for _, inm := range []string{etag, "W/" + etag, `"x", ` + etag, "*"} {
		w = e.do(http.MethodGet, "/static/pico.min.css", nil, withHeader("If-None-Match", inm))
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: status %d, %d body bytes", inm, w.Code, w.Body.Len())
		}
	}
	w = e.do(http.MethodGet, "/static/pico.min.css", nil, withHeader("If-None-Match", `"other"`))
	if w.Code != http.StatusOK {
		t.Errorf("stale ETag: %d", w.Code)
	}

	w = e.do(http.MethodHead, "/static/app.js", nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: status %d, %d body bytes, Content-Length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
}

func TestStaticNotFound(t *testing.T) {
	e := newEnv(t, allRoles)
	for _, p := range []string{"/static/", "/static/nope.js", "/static/../web.go", "/static/LICENSES/", "/static/%2e%2e/web.go"} {
		w := e.do(http.MethodGet, p, nil)
		if w.Code == http.StatusOK {
			t.Errorf("%s: status %d", p, w.Code)
		}
	}
	if w := e.do(http.MethodPost, "/static/app.js", nil); w.Code == http.StatusOK {
		t.Errorf("POST: status %d", w.Code)
	}
}

func TestAssetURL(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	if got := a.url("app.js"); !regexp.MustCompile(`^/static/app\.js\?v=[0-9a-f]{12}$`).MatchString(got) {
		t.Errorf("url = %q", got)
	}
	if got := a.url("missing.js"); got != "/static/missing.js" {
		t.Errorf("unknown asset url = %q", got)
	}
	// Every asset the layout references exists.
	e := newEnv(t, allRoles)
	body := e.do(http.MethodGet, "/login", nil).Body.String()
	for _, m := range regexp.MustCompile(`(?:href|src)="(/static/[^"?]+)\?v=`).FindAllStringSubmatch(body, -1) {
		if _, ok := a.files[strings.TrimPrefix(m[1], staticPrefix)]; !ok {
			t.Errorf("layout references missing asset %s", m[1])
		}
	}
}

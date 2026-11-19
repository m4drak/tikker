// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// CSRF token transport.
const (
	CSRFHeader    = "X-CSRF-Token" // htmx: hx-headers='{"X-CSRF-Token": "…"}'
	CSRFFormField = "csrf_token"   // hidden form field
	// maxCSRFFormBody bounds the form body CSRF parses to find the token.
	maxCSRFFormBody = 1 << 20
)

var errCrossOrigin = errors.New("auth: cross-origin request")

// crossOrigin rejects state-changing cross-origin browser requests. It
// delegates to net/http.CrossOriginProtection (Sec-Fetch-Site, then Origin
// against Host and the trusted origins) and adds a Referer check for old
// browsers that send neither header.
type crossOrigin struct {
	cop     *http.CrossOriginProtection
	trusted map[string]bool
}

func newCrossOrigin(origins []string) (*crossOrigin, error) {
	c := &crossOrigin{cop: http.NewCrossOriginProtection(), trusted: map[string]bool{}}
	for _, o := range origins {
		if err := c.cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("auth: trusted origin: %w", err)
		}
		c.trusted[o] = true
	}
	return c, nil
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (c *crossOrigin) check(r *http.Request) error {
	if safeMethod(r.Method) {
		return nil
	}
	if err := c.cop.Check(r); err != nil {
		return errCrossOrigin
	}
	if r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != "" {
		return nil // already judged by CrossOriginProtection
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return nil // not a browser, or a same-origin request with Referrer-Policy no-referrer
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return errCrossOrigin
	}
	if u.Host == r.Host || c.trusted[u.Scheme+"://"+u.Host] {
		return nil
	}
	return errCrossOrigin
}

// CSRF protects state-changing panel requests (all methods but GET, HEAD
// and OPTIONS):
//
//  1. cross-origin browser requests are rejected (Sec-Fetch-Site, Origin,
//     or Referer against the Host and the trusted origins, which include
//     the base URL);
//  2. when the request carries a session cookie, it must also carry the
//     session's CSRF token in the X-CSRF-Token header or the csrf_token
//     form field; the comparison is constant-time.
//
// Requests without a session (the login form, the first-run wizard) are
// protected by step 1 alone. Failures get 403. API routes authenticated
// with Bearer tokens do not need this middleware: browsers never attach
// the Authorization header on their own.
func (s *Service) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if err := s.cop.check(r); err != nil {
			s.log.Warn("csrf: cross-origin request rejected", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "Forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		if raw, ok := s.readCookie(r); ok {
			got := r.Header.Get(CSRFHeader)
			if got == "" {
				r.Body = http.MaxBytesReader(w, r.Body, maxCSRFFormBody)
				got = r.PostFormValue(CSRFFormField)
			}
			want := csrfFor(raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				s.log.Warn("csrf: missing or invalid token", "method", r.Method, "path", r.URL.Path)
				http.Error(w, "Forbidden: invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

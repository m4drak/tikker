// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import "net/http"

// registerPages registers the panel pages behind a session. The pages of
// T31 (dashboard, send, messages) and T32 (devices, tokens, users, audit)
// are placeholders until those tasks replace them: call s.Handle with the
// same pattern (see the package documentation).
func (s *Server) registerPages() {
	for _, it := range navItems {
		pattern := "GET " + it.Path
		if it.Path == "/" {
			pattern = "GET /{$}"
		}
		s.Handle(pattern, it.Role, s.soon(it.LabelKey))
	}
}

// soon renders the "coming soon" placeholder with the page's title.
func (s *Server) soon(titleKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.Render(w, r, http.StatusOK, "soon", titleKey)
	}
}

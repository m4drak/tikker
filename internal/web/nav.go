// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"strings"

	"github.com/m4drak/tikker/internal/domain"
)

// navItem is one entry of the main navigation.
type navItem struct {
	Path     string      // link target, also the route path
	LabelKey string      // i18n key
	Role     domain.Role // minimum role
}

// navItems is the navigation in display order. A user sees the entries
// their role allows: viewer - dashboard, messages; operator - plus send;
// admin - plus devices, tokens, users, audit.
var navItems = []navItem{
	{"/", "nav.dashboard", domain.RoleViewer},
	{"/messages", "nav.messages", domain.RoleViewer},
	{"/send", "nav.send", domain.RoleOperator},
	{"/devices", "nav.devices", domain.RoleAdmin},
	{"/tokens", "nav.tokens", domain.RoleAdmin},
	{"/users", "nav.users", domain.RoleAdmin},
	{"/audit", "nav.audit", domain.RoleAdmin},
}

// NavLink is a navigation entry as the templates see it.
type NavLink struct {
	Path   string
	Label  string
	Active bool
}

// navFor returns the entries visible to role, with the one matching path
// marked active (the dashboard only on an exact match, the others also on
// sub-paths such as /messages/123).
func (s *Server) navFor(role domain.Role, path, lang string) []NavLink {
	var out []NavLink
	for _, it := range navItems {
		if !role.Allows(it.Role) {
			continue
		}
		active := path == it.Path || (it.Path != "/" && strings.HasPrefix(path, it.Path+"/"))
		out = append(out, NavLink{Path: it.Path, Label: s.i18n.T(lang, it.LabelKey), Active: active})
	}
	return out
}

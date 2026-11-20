// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/web/i18n"
)

//go:embed templates
var templateFS embed.FS

// LangCookie is the cookie that stores the language chosen in the switcher.
const LangCookie = "tikker_lang"

// LangOption is an entry of the language switcher.
type LangOption struct {
	Code    string
	Name    string
	Current bool
}

// View is the data every template receives as ".". Handlers pass their own
// data in Data.
type View struct {
	Lang      string       // "pl" or "en", for <html lang> and the t function
	Path      string       // request path, for the navigation
	User      *domain.User // signed-in user, nil for anonymous pages
	RoleLabel string       // translated role name of User
	CSRF      string       // token for the hidden "csrf_token" field and the meta tag
	Nav       []NavLink
	Languages []LangOption
	Version   string
	SourceURL string
	Data      any
}

// loadTemplates parses, for every language, the layout files and each page
// on top of them. The "t" function is bound to the language, so a page is
// rendered by the set of the request's language.
func (s *Server) loadTemplates() (map[string]map[string]*template.Template, error) {
	pageFiles, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil || len(pageFiles) == 0 {
		return nil, fmt.Errorf("no page templates (%v)", err)
	}
	out := make(map[string]map[string]*template.Template, len(i18n.Languages))
	for _, lang := range i18n.Languages {
		funcs := template.FuncMap{
			"t":     func(key string, args ...any) string { return s.i18n.T(lang, key, args...) },
			"asset": s.assets.url,
			"ts":    formatTime,
		}
		base, err := template.New("layout").Funcs(funcs).ParseFS(templateFS, "templates/layout/*.html")
		if err != nil {
			return nil, fmt.Errorf("layout templates: %w", err)
		}
		set := make(map[string]*template.Template, len(pageFiles))
		for _, f := range pageFiles {
			c, err := base.Clone()
			if err != nil {
				return nil, fmt.Errorf("clone layout: %w", err)
			}
			if _, err := c.ParseFS(templateFS, f); err != nil {
				return nil, fmt.Errorf("page %s: %w", f, err)
			}
			set[strings.TrimSuffix(path.Base(f), ".html")] = c
		}
		out[lang] = set
	}
	return out, nil
}

// language returns the language of the request: the cookie set by the
// switcher, else the best match of Accept-Language, else the default.
func (s *Server) language(r *http.Request) string {
	if c, err := r.Cookie(LangCookie); err == nil && s.i18n.Supported(c.Value) {
		return c.Value
	}
	return s.i18n.Match(r.Header.Get("Accept-Language"))
}

// principal returns the signed-in user of the request: the one RequireSession
// stored, or, on pages outside it (login, 404), the session of the cookie.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		return p, true
	}
	p, err := s.auth.SessionPrincipal(w, r)
	if err != nil {
		if !errors.Is(err, auth.ErrUnauthenticated) {
			s.log.Error("web: session lookup failed", "err", err.Error())
		}
		return auth.Principal{}, false
	}
	return p, true
}

// newView fills the common part of a view.
func (s *Server) newView(w http.ResponseWriter, r *http.Request, data any) *View {
	lang := s.language(r)
	v := &View{
		Lang: lang, Path: r.URL.Path, Version: s.build, SourceURL: s.sourceURL, Data: data,
	}
	for _, code := range i18n.Languages {
		v.Languages = append(v.Languages, LangOption{Code: code, Name: s.i18n.T(code, "meta.name"), Current: code == lang})
	}
	if p, ok := s.principal(w, r); ok {
		u := p.User
		v.User = &u
		v.CSRF = p.CSRFToken
		v.RoleLabel = s.i18n.T(lang, "role."+string(u.Role))
		v.Nav = s.navFor(u.Role, r.URL.Path, lang)
	} else {
		v.CSRF = s.auth.CSRFToken(r)
	}
	return v
}

// Render renders the whole page (layout plus the page template of that name
// from templates/pages) with the given status. data is available as .Data.
func (s *Server) Render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	s.render(w, r, status, page, "base", data)
}

// RenderBlock renders only the named template defined in the page's file,
// for htmx fragments. The View (language, user, CSRF token) is the same as
// for a whole page.
func (s *Server) RenderBlock(w http.ResponseWriter, r *http.Request, status int, page, block string, data any) {
	s.render(w, r, status, page, block, data)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, block string, data any) {
	v := s.newView(w, r, data)
	tpl, ok := s.pages[v.Lang][page]
	if !ok {
		s.log.Error("web: unknown page template", "page", page)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// Render into a buffer so that a template error yields a clean 500
	// instead of half a page.
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, block, v); err != nil {
		s.log.Error("web: render failed", "page", page, "block", block, "err", err.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Language", v.Lang)
	h.Set("Cache-Control", "no-store")
	h.Add("Vary", "Cookie, Accept-Language")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// errorData is the data of the error page.
type errorData struct {
	Status   int
	TitleKey string
	TextKey  string
}

// errorPage renders a localised error page for 403, 404 and 500.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int) {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound:
	default:
		status = http.StatusInternalServerError
	}
	k := fmt.Sprintf("error.%d", status)
	s.Render(w, r, status, "error", errorData{Status: status, TitleKey: k + ".title", TextKey: k + ".text"})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound)
}

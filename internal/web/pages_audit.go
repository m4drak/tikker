// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/webhook"
)

const auditPerPage = 50

func (s *Server) registerAudit() {
	s.HandleFunc("GET /audit", domain.RoleAdmin, s.audit)
}

// auditActions are the actions offered by the filter.
var auditActions = []domain.AuditAction{
	domain.AuditLoginSuccess, domain.AuditLoginFailure, domain.AuditLogout,
	domain.AuditUserCreate, domain.AuditUserUpdate, domain.AuditTokenCreate, domain.AuditTokenDelete,
	domain.AuditDeviceCreate, domain.AuditDeviceUpdate, domain.AuditDeviceDelete,
	admin.AuditChannelCreate, domain.AuditChannelUpdate, admin.AuditChannelDelete,
	domain.AuditMessageSend, domain.AuditMessageCancel,
	webhook.AuditCreate, webhook.AuditUpdate, webhook.AuditDelete,
}

type auditFilter struct {
	Actor  string // "", "system" or "user:<id>"
	Action string
	Since  string // YYYY-MM-DD, UTC
	Until  string // YYYY-MM-DD, UTC, inclusive
}

func (f auditFilter) query() url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"actor": f.Actor, "action": f.Action, "since": f.Since, "until": f.Until} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

type auditRow struct {
	At     time.Time
	Actor  string
	Action string
	Target string
	Meta   []string // "key=value", sorted
}

type auditData struct {
	Filter    auditFilter
	Rows      []auditRow
	Actors    []optionItem
	Actions   []optionItem
	Filtered  bool
	Paged     bool
	NextLink  string
	FirstLink string
}

// metaHidden are the meta keys that never reach the page, whatever they
// hold; the audit entries contain no secrets by contract, this is a second
// line of defence.
var metaHidden = map[string]bool{"password": true, "secret": true, "token": true, "hash": true, "body": true}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var f auditFilter
	var sf store.AuditFilter

	users, err := s.st.Users().List(ctx)
	if err != nil {
		s.internalError(w, r, "list users failed", err)
		return
	}
	names := make(map[string]string, len(users))
	data := auditData{}
	data.Actors = []optionItem{{LabelKey: "messages.any"}, {Value: "system", LabelKey: "audit.actor.system"}}
	for _, u := range users {
		names["user:"+string(u.ID)] = u.Username
		data.Actors = append(data.Actors, optionItem{Value: "user:" + string(u.ID), Label: u.Username})
	}
	if a, err := domain.ParseActor(q.Get("actor")); err == nil && !a.IsZero() && a.Type != domain.ActorToken {
		if _, known := names[a.String()]; known || a.Type == domain.ActorSystem {
			f.Actor, sf.Actor = a.String(), a
		}
	}
	for _, a := range auditActions {
		if q.Get("action") == string(a) {
			f.Action, sf.Action = string(a), a
		}
	}
	if t, err := time.Parse(time.DateOnly, q.Get("since")); err == nil {
		f.Since, sf.Since = t.Format(time.DateOnly), t
	}
	if t, err := time.Parse(time.DateOnly, q.Get("until")); err == nil {
		f.Until, sf.Until = t.Format(time.DateOnly), t.AddDate(0, 0, 1)
	}
	var cursor string
	if id, err := domain.ParseID(q.Get("cursor")); err == nil {
		cursor = string(id)
	}

	page, err := s.st.Audit().List(ctx, sf, store.PageRequest{Cursor: cursor, Limit: auditPerPage})
	if err != nil {
		s.internalError(w, r, "list audit failed", err)
		return
	}
	for i := range data.Actors {
		data.Actors[i].Selected = data.Actors[i].Value == f.Actor
	}
	data.Actions = []optionItem{{LabelKey: "messages.any", Selected: f.Action == ""}}
	for _, a := range auditActions {
		data.Actions = append(data.Actions, optionItem{Value: string(a), Label: string(a), Selected: f.Action == string(a)})
	}
	for _, e := range page.Items {
		row := auditRow{At: e.At, Action: string(e.Action), Target: e.Target, Actor: e.Actor.String()}
		if n, ok := names[e.Actor.String()]; ok {
			row.Actor = n
		}
		keys := make([]string, 0, len(e.Meta))
		for k := range e.Meta {
			if !metaHidden[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			row.Meta = append(row.Meta, k+"="+e.Meta[k])
		}
		data.Rows = append(data.Rows, row)
	}
	data.Filter, data.Filtered, data.Paged = f, len(f.query()) > 0, cursor != ""
	if page.NextCursor != "" {
		nq := f.query()
		nq.Set("cursor", page.NextCursor)
		data.NextLink = "/audit?" + nq.Encode()
	}
	data.FirstLink = "/audit"
	if data.Filtered {
		data.FirstLink += "?" + f.query().Encode()
	}
	s.Render(w, r, http.StatusOK, "audit", data)
}

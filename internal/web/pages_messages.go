// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
)

// Size of a page of the message list and the length of the body preview.
const (
	messagesPerPage = 25
	bodyPreviewLen  = 70
	maxPeerFilter   = sending.MaxToLen
)

// Notices shown after a redirect (?notice=...); only these values are
// accepted, the rest is ignored.
const (
	noticeQueued    = "queued"
	noticeQueuedN   = "queued_n"
	noticeCancelled = "cancelled"
)

// messageFilter is the filter form of the list, as the user typed it.
type messageFilter struct {
	Direction string
	Status    string
	Peer      string
	Channel   string
	Since     string // YYYY-MM-DD, UTC
	Until     string // YYYY-MM-DD, UTC, inclusive
}

// query returns the filter as URL values (empty fields left out).
func (f messageFilter) query() url.Values {
	q := url.Values{}
	for k, v := range map[string]string{
		"direction": f.Direction, "status": f.Status, "peer": f.Peer,
		"channel": f.Channel, "since": f.Since, "until": f.Until,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// optionItem is an entry of a <select>.
type optionItem struct {
	Value    string
	Label    string // translated text, or "" when LabelKey is used
	LabelKey string
	Selected bool
}

// messageRow is one line of the list.
type messageRow struct {
	ID        domain.ID
	At        time.Time
	Direction string // translation key
	Peer      string
	Preview   string
	Status    domain.MessageStatus
	StatusKey string
	Duplicate bool
	Segments  int
	Channel   string
}

type messagesData struct {
	Filter    messageFilter
	Rows      []messageRow
	Total     int
	Channels  []optionItem
	Statuses  []optionItem
	Dirs      []optionItem
	Filtered  bool
	Paged     bool   // a cursor was given: offer a link back to the first page
	NextLink  string // link to the next page; "" on the last page
	FirstLink string // link to the first page with the same filter
	Notice    string
	NoticeN   int
}

// messageData is the data of the details page.
type messageData struct {
	M         domain.Message
	StatusKey string
	DirKey    string
	Encoding  string
	Channel   string
	Author    string // "" for inbound messages
	CanCancel bool
	Notice    string
	Conflict  bool // a cancel was refused: the message is no longer queued
}

func (s *Server) registerMessages() {
	s.HandleFunc("GET /messages", domain.RoleViewer, s.messages)
	s.HandleFunc("GET /messages/{id}", domain.RoleViewer, s.messageDetails)
	s.HandleFunc("POST /messages/{id}/cancel", domain.RoleOperator, s.messageCancel)
}

// parseFilter reads the filter from the query; invalid values are ignored.
func parseFilter(q url.Values) (messageFilter, store.MessageFilter) {
	var f messageFilter
	var sf store.MessageFilter
	if d, err := domain.ParseDirection(q.Get("direction")); err == nil {
		f.Direction, sf.Direction = string(d), d
	}
	if st, err := domain.ParseMessageStatus(q.Get("status")); err == nil {
		f.Status, sf.Statuses = string(st), []domain.MessageStatus{st}
	}
	if p := strings.TrimSpace(q.Get("peer")); p != "" && utf8.RuneCountInString(p) <= maxPeerFilter {
		f.Peer = p
	}
	if id, err := domain.ParseID(q.Get("channel")); err == nil {
		f.Channel, sf.ChannelID = string(id), id
	}
	if t, err := time.Parse(time.DateOnly, q.Get("since")); err == nil {
		f.Since, sf.Since = t.Format(time.DateOnly), t
	}
	if t, err := time.Parse(time.DateOnly, q.Get("until")); err == nil {
		f.Until, sf.Until = t.Format(time.DateOnly), t.AddDate(0, 0, 1) // the whole day
	}
	return f, sf
}

// messages renders the filtered, cursor-paginated list.
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	f, sf := parseFilter(q)
	if f.Peer != "" {
		sf.Peer = s.send.NormalizePeer(f.Peer)
	}
	var cursor string
	if id, err := domain.ParseID(q.Get("cursor")); err == nil {
		cursor = string(id)
	}

	page, err := s.st.Messages().List(ctx, sf, store.PageRequest{Cursor: cursor, Limit: messagesPerPage})
	if err != nil {
		s.log.Error("web: list messages failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	total, err := s.st.Messages().Count(ctx, sf)
	if err != nil {
		s.log.Error("web: count messages failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	chs, err := s.st.Channels().List(ctx, store.ChannelFilter{})
	if err != nil {
		s.log.Error("web: list channels failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	names := make(map[domain.ID]string, len(chs))
	data := messagesData{Filter: f, Total: total, Paged: cursor != ""}
	data.Channels = append(data.Channels, optionItem{Value: "", LabelKey: "messages.any"})
	for _, c := range chs {
		names[c.ID] = channelName(c)
		data.Channels = append(data.Channels, optionItem{Value: string(c.ID), Label: channelName(c), Selected: string(c.ID) == f.Channel})
	}
	data.Dirs = []optionItem{{LabelKey: "messages.any", Selected: f.Direction == ""}}
	for _, d := range []domain.Direction{domain.DirectionOut, domain.DirectionIn} {
		data.Dirs = append(data.Dirs, optionItem{Value: string(d), LabelKey: directionKey(d), Selected: f.Direction == string(d)})
	}
	data.Statuses = []optionItem{{LabelKey: "messages.any", Selected: f.Status == ""}}
	for _, st := range domain.AllMessageStatuses {
		data.Statuses = append(data.Statuses, optionItem{Value: string(st), LabelKey: statusKey(st), Selected: f.Status == string(st)})
	}
	data.Filtered = len(f.query()) > 0

	for _, m := range page.Items {
		at := m.CreatedAt
		if m.Direction == domain.DirectionIn && !m.ReceivedAt.IsZero() {
			at = m.ReceivedAt
		}
		data.Rows = append(data.Rows, messageRow{
			ID: m.ID, At: at, Direction: directionKey(m.Direction), Peer: m.Peer, Preview: preview(m.Body, bodyPreviewLen),
			Status: m.Status, StatusKey: statusKey(m.Status), Duplicate: m.PossibleDuplicate, Segments: m.Segments,
			Channel: names[m.ChannelID],
		})
	}
	if page.NextCursor != "" {
		nq := f.query()
		nq.Set("cursor", page.NextCursor)
		data.NextLink = "/messages?" + nq.Encode()
	}
	data.FirstLink = "/messages"
	if data.Filtered {
		data.FirstLink += "?" + f.query().Encode()
	}
	switch q.Get("notice") {
	case noticeQueuedN:
		if n, err := strconv.Atoi(q.Get("n")); err == nil && n > 1 && n <= 999 {
			data.Notice, data.NoticeN = noticeQueuedN, n
		}
	case noticeQueued, noticeCancelled:
		data.Notice = q.Get("notice")
	}
	s.Render(w, r, http.StatusOK, "messages", data)
}

// messageID reads the {id} path value.
func messageID(r *http.Request) (domain.ID, bool) {
	id, err := domain.ParseID(r.PathValue("id"))
	return id, err == nil
}

// messageDetails renders one message.
func (s *Server) messageDetails(w http.ResponseWriter, r *http.Request) {
	id, ok := messageID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	m, err := s.st.Messages().Get(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("web: get message failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	data, err := s.messageView(r.Context(), r, m)
	if err != nil {
		s.log.Error("web: message view failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	switch n := r.URL.Query().Get("notice"); n {
	case noticeQueued, noticeCancelled:
		data.Notice = n
	}
	s.Render(w, r, http.StatusOK, "message", data)
}

// messageView fills the details of m.
func (s *Server) messageView(ctx context.Context, r *http.Request, m domain.Message) (messageData, error) {
	d := messageData{
		M: m, StatusKey: statusKey(m.Status), DirKey: directionKey(m.Direction), Encoding: encodingLabel(m.Encoding),
	}
	if !m.CreatedBy.IsZero() {
		d.Author = m.CreatedBy.String()
	}
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		d.CanCancel = p.User.Role.Allows(domain.RoleOperator) && m.Direction == domain.DirectionOut && m.Status == domain.StatusQueued
	}
	if m.ChannelID != "" {
		chs, err := s.st.Channels().List(ctx, store.ChannelFilter{})
		if err != nil {
			return d, fmt.Errorf("list channels: %w", err)
		}
		d.Channel = string(m.ChannelID)
		for _, c := range chs {
			if c.ID == m.ChannelID {
				d.Channel = channelName(c)
			}
		}
	}
	if m.CreatedBy.Type == domain.ActorUser {
		u, err := s.st.Users().Get(ctx, m.CreatedBy.ID)
		switch {
		case err == nil:
			d.Author = u.Username
		case !errors.Is(err, domain.ErrNotFound):
			return d, fmt.Errorf("get author: %w", err)
		}
	}
	return d, nil
}

// messageCancel cancels a queued message (operator, CSRF by the middleware)
// and returns to its page. A message that is no longer queued is shown with a
// conflict notice.
func (s *Server) messageCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := messageID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		s.errorPage(w, r, http.StatusForbidden)
		return
	}
	m, err := s.send.Cancel(r.Context(), p.Actor(), id)
	switch {
	case err == nil:
		http.Redirect(w, r, "/messages/"+string(id)+"?notice="+noticeCancelled, http.StatusSeeOther)
	case errors.Is(err, domain.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, domain.ErrInvalidTransition):
		data, verr := s.messageView(r.Context(), r, m)
		if verr != nil {
			s.log.Error("web: message view failed", "err", verr.Error())
			s.errorPage(w, r, http.StatusInternalServerError)
			return
		}
		data.Conflict = true
		s.Render(w, r, http.StatusConflict, "message", data)
	default:
		s.log.Error("web: cancel failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
	}
}

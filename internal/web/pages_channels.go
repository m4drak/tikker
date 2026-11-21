// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/domain"
)

// Notices of the channel page.
const noticeChannelSaved = "saved"

func (s *Server) registerChannels() {
	s.HandleFunc("GET /channels/{id}", domain.RoleAdmin, s.channelDetails)
	s.HandleFunc("POST /channels/{id}", domain.RoleAdmin, s.channelUpdate)
	s.HandleFunc("POST /channels/{id}/toggle", domain.RoleAdmin, s.channelToggle)
	s.HandleFunc("POST /channels/{id}/check", domain.RoleAdmin, s.channelCheck)
	s.HandleFunc("GET /channels/{id}/delete", domain.RoleAdmin, s.channelDeleteConfirm)
	s.HandleFunc("POST /channels/{id}/delete", domain.RoleAdmin, s.channelDelete)
}

// channelForm is what the user typed.
type channelForm struct {
	Name          string
	Tags          string
	RatePerMin    string
	MaxSegments   string
	Transliterate bool
	Enabled       bool
}

type channelData struct {
	Row      adminChannelRow
	Device   domain.Device
	Form     channelForm
	Errors   map[string]string
	Notice   string
	Rate     [2]int // min, max
	Segments [2]int
	Check    *checkView
}

// checkView is the result of "check now".
type checkView struct {
	ChannelID domain.ID
	StateKey  string
	State     domain.ChannelState
	Reason    string
	Operator  string
	Signal    string
	At        string
}

func channelFormOf(c domain.Channel) channelForm {
	return channelForm{
		Name: c.Name, Tags: strings.Join(c.Tags, ", "), RatePerMin: strconv.Itoa(c.RatePerMin),
		MaxSegments: strconv.Itoa(c.MaxSegments), Transliterate: c.Transliterate, Enabled: c.Enabled,
	}
}

// loadChannel reads the channel and device of the path.
func (s *Server) loadChannel(w http.ResponseWriter, r *http.Request) (domain.Channel, domain.Device, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return domain.Channel{}, domain.Device{}, false
	}
	c, _, err := s.admin.GetChannel(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		s.notFound(w, r)
		return c, domain.Device{}, false
	}
	if err != nil {
		s.internalError(w, r, "get channel failed", err)
		return c, domain.Device{}, false
	}
	d, err := s.admin.GetDevice(r.Context(), c.DeviceID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.internalError(w, r, "get device of channel failed", err)
		return c, d, false
	}
	return c, d, true
}

func (s *Server) channelPage(w http.ResponseWriter, r *http.Request, status int, c domain.Channel, d domain.Device, f channelForm, errs map[string]string, noticeValue string) {
	v, err := s.admin.View(r.Context(), c, d.Enabled)
	if err != nil {
		s.internalError(w, r, "channel view failed", err)
		return
	}
	s.Render(w, r, status, "channel", channelData{
		Row: s.channelRowOf(v), Device: d, Form: f, Errors: errs, Notice: noticeValue,
		Rate: [2]int{admin.MinRatePerMin, admin.MaxRatePerMin}, Segments: [2]int{admin.MinMaxSegments, admin.MaxMaxSegments},
	})
}

func (s *Server) channelDetails(w http.ResponseWriter, r *http.Request) {
	c, d, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	s.channelPage(w, r, http.StatusOK, c, d, channelFormOf(c), nil, notice(r.URL.Query().Get("notice"), noticeChannelSaved, noticeEnabled, noticeDisabled))
}

// channelCreate adds a channel for a port (the buttons of the test result and
// the manual form of the device page).
func (s *Server) channelCreate(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	port, id := strings.TrimSpace(r.PostFormValue("port")), string(d.ID)
	_, err := s.admin.CreateChannel(r.Context(), p.Actor(), admin.ChannelCreate{DeviceID: &id, Port: &port})
	lang := s.language(r)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+id+"?notice="+noticeChannelAdded, http.StatusSeeOther)
		return
	case errors.Is(err, admin.ErrDuplicate):
		s.devicePage(w, r, http.StatusConflict, d, s.formData(d.ID, formOf(d), nil), "", s.i18n.T(lang, "ch.err.duplicate"))
		return
	}
	if ps, ok := problemsOf(err); ok {
		msg := s.fieldMessages(lang, ps)["port"]
		if msg == "" {
			msg = s.i18n.T(lang, "err.generic")
		}
		s.devicePage(w, r, http.StatusUnprocessableEntity, d, s.formData(d.ID, formOf(d), nil), "", msg)
		return
	}
	s.internalError(w, r, "create channel failed", err)
}

// channelUpdate saves the channel form.
func (s *Server) channelUpdate(w http.ResponseWriter, r *http.Request) {
	c, d, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	f := channelForm{
		Name: strings.TrimSpace(r.PostFormValue("name")), Tags: r.PostFormValue("tags"),
		RatePerMin: strings.TrimSpace(r.PostFormValue("rate_per_min")), MaxSegments: strings.TrimSpace(r.PostFormValue("max_segments")),
		Transliterate: r.PostFormValue("transliterate") != "", Enabled: r.PostFormValue("enabled") != "",
	}
	lang := s.language(r)
	errs := map[string]string{}
	rate, err := strconv.Atoi(f.RatePerMin)
	if err != nil {
		errs["rate_per_min"] = s.i18n.T(lang, "err.number")
	}
	segs, err := strconv.Atoi(f.MaxSegments)
	if err != nil {
		errs["max_segments"] = s.i18n.T(lang, "err.number")
	}
	if len(errs) > 0 {
		s.channelPage(w, r, http.StatusUnprocessableEntity, c, d, f, errs, "")
		return
	}
	tags := parseTags(f.Tags)
	_, err = s.admin.UpdateChannel(r.Context(), p.Actor(), c.ID, admin.ChannelChanges{
		Name: &f.Name, Tags: &tags, RatePerMin: &rate, MaxSegments: &segs,
		Transliterate: &f.Transliterate, Enabled: &f.Enabled,
	})
	if err == nil {
		http.Redirect(w, r, "/channels/"+string(c.ID)+"?notice="+noticeChannelSaved, http.StatusSeeOther)
		return
	}
	if errors.Is(err, domain.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if ps, ok := problemsOf(err); ok {
		s.channelPage(w, r, http.StatusUnprocessableEntity, c, d, f, s.fieldMessages(lang, ps), "")
		return
	}
	s.internalError(w, r, "update channel failed", err)
}

// channelToggle enables or disables the channel.
func (s *Server) channelToggle(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	enable := r.PostFormValue("enabled") == "true"
	if _, err := s.admin.UpdateChannel(r.Context(), p.Actor(), c.ID, admin.ChannelChanges{Enabled: &enable}); err != nil {
		s.internalError(w, r, "toggle channel failed", err)
		return
	}
	n := noticeDisabled
	if enable {
		n = noticeEnabled
	}
	http.Redirect(w, r, "/devices/"+string(c.DeviceID)+"?notice="+n, http.StatusSeeOther)
}

// channelCheck runs a health check now and answers with the "channel_check"
// fragment.
func (s *Server) channelCheck(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	st, err := s.admin.CheckChannel(r.Context(), c.ID)
	v := checkView{ChannelID: c.ID, State: st.State, StateKey: stateKey(st.State), Reason: st.Reason, Operator: st.Operator,
		Signal: signalText(st.Signal), At: formatTime(st.LastCheckAt)}
	if v.Reason == "" {
		v.Reason = st.LastError
	}
	switch {
	case err == nil:
	case errors.Is(err, admin.ErrNoChecker):
		s.internalError(w, r, "channel check unavailable", err)
		return
	default:
		// A failed check is a result, not a page error: show what is known.
		s.log.Warn("web: channel check failed", "channel_id", string(c.ID), "err", err.Error())
		if v.Reason == "" {
			v.Reason = s.i18n.T(s.language(r), "ch.check.failed")
		}
	}
	s.RenderBlock(w, r, http.StatusOK, "channel", "channel_check", v)
}

func (s *Server) channelDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	s.renderChannelConfirm(w, r, http.StatusOK, c, "")
}

func (s *Server) renderChannelConfirm(w http.ResponseWriter, r *http.Request, status int, c domain.Channel, errText string) {
	lang := s.language(r)
	s.Render(w, r, status, "confirm", confirmData{
		Heading: s.i18n.T(lang, "ch.delete.heading", channelName(c)), Text: s.i18n.T(lang, "ch.delete.text"),
		Action: "/channels/" + string(c.ID) + "/delete", Back: "/channels/" + string(c.ID),
		Error: errText, Button: s.i18n.T(lang, "ch.delete.button"),
	})
}

func (s *Server) channelDelete(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	err := s.admin.DeleteChannel(r.Context(), p.Actor(), c.ID)
	var am *admin.ActiveMessagesError
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+string(c.DeviceID)+"?notice="+noticeChannelDeleted, http.StatusSeeOther)
	case errors.Is(err, domain.ErrNotFound):
		s.notFound(w, r)
	case errors.As(err, &am):
		s.renderChannelConfirm(w, r, http.StatusConflict, c, s.i18n.T(s.language(r), "ch.delete.conflict"))
	default:
		s.internalError(w, r, "delete channel failed", err)
	}
}

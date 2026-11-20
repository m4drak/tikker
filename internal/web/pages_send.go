// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
)

// nonceRE matches the one-time form token (see newNonce).
var nonceRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// sendErrKeys maps "<form field>/<code>" of a validation problem to its
// translation key; sendErrByCode is the fallback by code alone.
var (
	sendErrKeys = map[string]string{
		"to/required":                 "send.err.to_required",
		"to/out_of_range":             "send.err.to_range",
		"to/invalid_phone_number":     "send.err.invalid_phone_number",
		"body/required":               "send.err.body_required",
		"body/out_of_range":           "send.err.body_range",
		"body/invalid_format":         "send.err.body_invalid",
		"body/too_many_segments":      "send.err.too_many_segments",
		"body/unsupported_characters": "send.err.unsupported_characters",
		"channel/invalid_format":      "send.err.channel_invalid",
		"channel/unknown_channel":     "send.err.unknown_channel",
		"channel/channel_disabled":    "send.err.channel_disabled",
		"channel/no_matching_channel": "send.err.no_matching_channel",
		"tags/no_matching_channel":    "send.err.no_matching_tags",
		"tags/invalid_format":         "send.err.tags_invalid",
		"tags/out_of_range":           "send.err.tags_range",
	}
	sendErrByCode = map[string]string{
		"too_many_segments":      "send.err.too_many_segments",
		"unsupported_characters": "send.err.unsupported_characters",
	}
)

// sendForm is what the user typed.
type sendForm struct {
	To      string
	Body    string
	Channel string
	Tags    string
	Nonce   string
}

// previewView is the live counter of the form, with translated problems.
type previewView struct {
	sending.Preview
	EncodingLabel   string
	ShownSegments   int
	Problems        []string
	SubstitutedText string // the characters replaced by '?', as text
	NonGSMText      string // the characters that force UCS-2, as text
	Limit           int    // maximum body length
}

// sendData is the data of the send page.
type sendData struct {
	Form     sendForm
	Errors   map[string][]string // form field -> translated problems
	Channels []optionItem        // usable channels, first entry: any
	NoChan   bool
	Preview  previewView
	MaxTo    int
	MaxBody  int
}

func (s *Server) registerSend() {
	s.HandleFunc("GET /send", domain.RoleOperator, s.sendForm)
	s.HandleFunc("POST /send", domain.RoleOperator, s.sendSubmit)
	s.HandleFunc("POST /send/preview", domain.RoleOperator, s.sendPreview)
}

// newNonce returns a random token that makes a form submission idempotent
// (a double click or a reload sends once).
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err //nolint:wrapcheck // the caller logs it
	}
	return hex.EncodeToString(b[:]), nil
}

// parseTags splits the tags field (commas and white space separate tags).
func parseTags(s string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		out = append(out, strings.ToLower(t))
	}
	return out
}

// normalizeBody makes line breaks single characters, as the planner counts
// them (browsers submit CR LF).
func normalizeBody(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// readSendForm reads the fields of the send form from a parsed request.
func readSendForm(r *http.Request) sendForm {
	return sendForm{
		To:      strings.TrimSpace(r.PostFormValue("to")),
		Body:    normalizeBody(r.PostFormValue("body")),
		Channel: strings.TrimSpace(r.PostFormValue("channel")),
		Tags:    strings.TrimSpace(r.PostFormValue("tags")),
		Nonce:   r.PostFormValue("nonce"),
	}
}

// sendForm shows the empty form.
func (s *Server) sendForm(w http.ResponseWriter, r *http.Request) {
	nonce, err := newNonce()
	if err != nil {
		s.log.Error("web: nonce failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.renderSend(w, r, http.StatusOK, sendForm{Nonce: nonce}, nil)
}

// renderSend renders the form with the problems found (nil: none) and a
// fresh preview.
func (s *Server) renderSend(w http.ResponseWriter, r *http.Request, status int, f sendForm, errs []sending.FieldError) {
	ctx := r.Context()
	lang := s.language(r)
	if !nonceRE.MatchString(f.Nonce) {
		nonce, err := newNonce()
		if err != nil {
			s.log.Error("web: nonce failed", "err", err.Error())
			s.errorPage(w, r, http.StatusInternalServerError)
			return
		}
		f.Nonce = nonce
	}
	rt, err := s.send.LoadRouting(ctx)
	if err != nil {
		s.log.Error("web: routing failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	pv, err := s.send.Preview(ctx, sending.PreviewRequest{Body: f.Body, ChannelID: f.Channel, Tags: parseTags(f.Tags)})
	if err != nil {
		s.log.Error("web: preview failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	data := sendData{
		Form: f, Errors: map[string][]string{}, MaxTo: sending.MaxToLen, MaxBody: sending.MaxBodyChars,
		Preview: s.previewView(lang, pv),
	}
	data.Channels = append(data.Channels, optionItem{Value: "", LabelKey: "send.channel_any", Selected: f.Channel == ""})
	for _, c := range rt.Usable() {
		data.Channels = append(data.Channels, optionItem{Value: string(c.ID), Label: channelName(c) + " (" + c.Port + ")", Selected: string(c.ID) == f.Channel})
	}
	data.NoChan = len(data.Channels) == 1
	for _, fe := range errs {
		field, msg := s.sendProblem(lang, fe)
		data.Errors[field] = append(data.Errors[field], msg)
	}
	// The preview repeats problems the submission reported: show each once.
	data.Preview.Problems = slices.DeleteFunc(data.Preview.Problems, func(m string) bool {
		for _, ms := range data.Errors {
			if slices.Contains(ms, m) {
				return true
			}
		}
		return false
	})
	s.Render(w, r, status, "send", data)
}

// sendSubmit validates and enqueues the message and redirects to it.
func (s *Server) sendSubmit(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest)
		return
	}
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		s.errorPage(w, r, http.StatusForbidden)
		return
	}
	f := readSendForm(r)
	req := sending.Request{To: &f.To, Body: &f.Body, Tags: parseTags(f.Tags)}
	if f.Channel != "" {
		req.ChannelID = &f.Channel
	}
	var opts sending.SendOptions
	if nonceRE.MatchString(f.Nonce) {
		opts.IdempotencyKey = "ui:" + f.Nonce
	}
	res, errs, err := s.send.Send(r.Context(), p.Actor(), req, opts)
	if errors.Is(err, domain.ErrIdempotencyMismatch) {
		// The form was resubmitted with other content (back button, edit):
		// that is a new message.
		f.Nonce, err = newNonce()
		if err == nil {
			opts.IdempotencyKey = "ui:" + f.Nonce
			res, errs, err = s.send.Send(r.Context(), p.Actor(), req, opts)
		}
	}
	if err != nil {
		s.log.Error("web: send failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	if len(errs) > 0 {
		s.renderSend(w, r, http.StatusUnprocessableEntity, f, errs)
		return
	}
	if n := len(res.Messages); n > 1 {
		http.Redirect(w, r, "/messages?direction=out&notice="+noticeQueuedN+"&n="+strconv.Itoa(n), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/messages/"+string(res.Messages[0].ID)+"?notice="+noticeQueued, http.StatusSeeOther)
}

// sendPreview renders the live counter fragment for the form's current
// content.
func (s *Server) sendPreview(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest)
		return
	}
	f := readSendForm(r)
	pv, err := s.send.Preview(r.Context(), sending.PreviewRequest{Body: f.Body, ChannelID: f.Channel, Tags: parseTags(f.Tags)})
	if err != nil {
		s.log.Error("web: preview failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.RenderBlock(w, r, http.StatusOK, "send", "send_preview", sendData{Preview: s.previewView(s.language(r), pv)})
}

// previewView prepares a preview for the template.
func (s *Server) previewView(lang string, pv sending.Preview) previewView {
	v := previewView{
		Preview: pv, EncodingLabel: encodingLabel(pv.Encoding), ShownSegments: pv.Segments,
		SubstitutedText: string(pv.Substituted), NonGSMText: string(pv.NonGSM), Limit: sending.MaxBodyChars,
	}
	if pv.Planned {
		v.ShownSegments = pv.TotalSegments
	}
	for _, fe := range pv.Errors {
		_, msg := s.sendProblem(lang, fe)
		v.Problems = append(v.Problems, msg)
	}
	return v
}

// sendProblem translates a validation problem and names the form field it
// belongs to ("to", "body", "channel" or "tags").
func (s *Server) sendProblem(lang string, fe sending.FieldError) (field, msg string) {
	field = strings.TrimPrefix(fe.Field, "/")
	field, _, _ = strings.Cut(field, "/")
	switch field {
	case "channel_id", "":
		field = "channel"
	}
	key, ok := sendErrKeys[field+"/"+fe.Code]
	if !ok {
		if key, ok = sendErrByCode[fe.Code]; !ok {
			key = "send.err.generic"
		}
	}
	if key == "send.err.body_range" {
		return field, s.i18n.T(lang, key, sending.MaxBodyChars)
	}
	return field, s.i18n.T(lang, key)
}

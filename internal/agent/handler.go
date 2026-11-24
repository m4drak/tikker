// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
)

// Prefix is the path prefix of the agent API; mount [Hub.Handler] under
// Prefix + "/".
const Prefix = "/agent/v1"

// Limits of agent requests.
const (
	MaxBodyBytes     = 1 << 20
	maxPortLen       = 32
	maxRemoteIDLen   = 64
	maxPhoneLen      = 64
	maxTimestampLen  = 64
	maxPDUHexLen     = 1024
	maxBodyRunes     = 2000
	maxTypeLen       = 32
	maxInfoLen       = 128
	maxReasonRunes   = 160
	maxPushMessages  = 100
	maxCheckinPorts  = 16
	maxAckRefs       = 1000
	maxRemoteRefLen  = 64
	maxPollPortsList = 16
)

// Problem types (RFC 9457 "type"), shared with the REST API.
const (
	problemMalformed       = "urn:tikker:problem:malformed-request"
	problemUnauthorized    = "urn:tikker:problem:unauthorized"
	problemNotFound        = "urn:tikker:problem:not-found"
	problemMethodNotAllow  = "urn:tikker:problem:method-not-allowed"
	problemPayloadTooLarge = "urn:tikker:problem:payload-too-large"
	problemValidation      = "urn:tikker:problem:validation"
	contentTypeProblem     = "application/problem+json"
)

var problemTitles = map[string]string{
	problemMalformed:       "Malformed request",
	problemUnauthorized:    "Unauthorized",
	problemNotFound:        "Not Found",
	problemMethodNotAllow:  "Method Not Allowed",
	problemPayloadTooLarge: "Payload too large",
	problemValidation:      "Request validation failed",
}

var portPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// registeredStates are registration values that mean "attached to a
// network" (as in the routeros connector). [WERYFIKACJA T00]
var registeredStates = map[string]bool{"home": true, "roaming": true, "registered": true}

// timestampLayouts parse the RouterOS inbox timestamp. [WERYFIKACJA T00]
var timestampLayouts = []string{"2006-01-02 15:04:05", "Jan/02/2006 15:04:05", time.RFC3339}

type route struct {
	method string
	h      func(w http.ResponseWriter, r *http.Request, s *session)
}

// Handler returns the agent API. Every request must carry the device's
// agent token ("Authorization: Bearer <token>"); every error is an
// application/problem+json document.
func (h *Hub) Handler() http.Handler {
	routes := map[string]route{
		Prefix + "/checkin":                      {http.MethodPost, h.handleCheckin},
		Prefix + "/commands":                     {http.MethodPost, h.handleCommands},
		Prefix + "/commands/{message_id}/result": {http.MethodPost, h.handleResult},
		Prefix + "/inbox":                        {http.MethodPost, h.handleInbox},
		Prefix + "/inbox/ack":                    {http.MethodPost, h.handleAck},
		Prefix + "/status":                       {http.MethodGet, h.handleStatus},
	}
	mux := http.NewServeMux()
	for pattern, rt := range routes {
		mux.Handle(pattern, h.endpoint(rt))
	}
	mux.HandleFunc(Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, problemNotFound, "Unknown agent endpoint.", nil)
	})
	return mux
}

// endpoint checks the method and the token before calling rt.h. The method
// is checked first so that probing paths reveals nothing about tokens.
func (h *Hub) endpoint(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != rt.method {
			w.Header().Set("Allow", rt.method)
			writeProblem(w, r, http.StatusMethodNotAllowed, problemMethodNotAllow, "Use "+rt.method+".", nil)
			return
		}
		s := h.authenticate(bearer(r))
		if s == nil {
			// Debug only: unauthenticated clients must not be able to
			// flood the log at the default level.
			h.log.Debug("agent authentication failed", slog.String("path", r.URL.Path), slog.String("remote_addr", r.RemoteAddr))
			w.Header().Set("WWW-Authenticate", `Bearer realm="tikker-agent"`)
			writeProblem(w, r, http.StatusUnauthorized, problemUnauthorized, "Missing or invalid agent token.", nil)
			return
		}
		s.touch()
		rt.h(w, r, s)
	})
}

// bearer extracts the token of an "Authorization: Bearer" header.
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// ---------------------------------------------------------------------------
// Problems

type fieldError struct {
	Field  string `json:"field"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type problemDoc struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	Errors   []fieldError `json:"errors,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, typ, detail string, errs []fieldError) {
	title, ok := problemTitles[typ]
	if !ok {
		title = http.StatusText(status)
	}
	w.Header().Set("Content-Type", contentTypeProblem)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDoc{
		Type: typ, Title: title, Status: status, Detail: detail, Instance: r.URL.Path, Errors: errs,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// validation collects field errors.
type validation []fieldError

func (v *validation) add(field, code, format string, args ...any) {
	*v = append(*v, fieldError{Field: field, Code: code, Detail: fmt.Sprintf(format, args...)})
}

func (v validation) write(w http.ResponseWriter, r *http.Request) bool {
	if len(v) == 0 {
		return false
	}
	writeProblem(w, r, http.StatusUnprocessableEntity, problemValidation, "The request has invalid fields.", v)
	return true
}

// decode reads a JSON object body into dst. An empty body is allowed when
// optional is set. It writes the problem and returns false on failure.
// Unknown fields are ignored, so newer scripts work with older servers.
func decode(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, problemPayloadTooLarge,
				fmt.Sprintf("The body exceeds %d bytes.", MaxBodyBytes), nil)
			return false
		}
		writeProblem(w, r, http.StatusBadRequest, problemMalformed, "The body cannot be read.", nil)
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		if optional {
			return true
		}
		writeProblem(w, r, http.StatusBadRequest, problemMalformed, "A JSON object is required.", nil)
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(dst); err != nil {
		writeProblem(w, r, http.StatusBadRequest, problemMalformed, "Invalid JSON: "+jsonProblem(err), nil)
		return false
	}
	if dec.More() {
		writeProblem(w, r, http.StatusBadRequest, problemMalformed, "Trailing data after the JSON object.", nil)
		return false
	}
	return true
}

// jsonProblem describes a decoding error without echoing request content.
func jsonProblem(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("syntax error at offset %d.", se.Offset)
	case errors.As(err, &te):
		return fmt.Sprintf("field %q has the wrong type.", te.Field)
	}
	return "cannot be decoded."
}

func checkLen(v *validation, field, s string, maxLen int, required bool) {
	switch {
	case s == "" && required:
		v.add(field, "required", "Required.")
	case len(s) > maxLen:
		v.add(field, "out_of_range", "At most %d bytes.", maxLen)
	case !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0:
		v.add(field, "invalid_format", "Must be printable UTF-8 text.")
	}
}

func checkPort(v *validation, field, port string) {
	if !portPattern.MatchString(port) {
		v.add(field, "invalid_format", "Must be an interface name: 1 to %d characters of A-Z a-z 0-9 . _ -.", maxPortLen)
	}
}

// ---------------------------------------------------------------------------
// Endpoints

func (h *Hub) handleCheckin(w http.ResponseWriter, r *http.Request, s *session) {
	var req checkinRequest
	if !decode(w, r, &req, false) {
		return
	}
	var v validation
	checkLen(&v, "/routeros_version", req.RouterOSVersion, maxInfoLen, false)
	checkLen(&v, "/board", req.Board, maxInfoLen, false)
	checkLen(&v, "/agent_version", req.AgentVersion, maxInfoLen, false)
	if len(req.Ports) > maxCheckinPorts {
		v.add("/ports", "out_of_range", "At most %d ports.", maxCheckinPorts)
	}
	now := h.clk.Now()
	rep := report{version: req.RouterOSVersion, board: req.Board, agentVersion: req.AgentVersion}
	seen := map[string]bool{}
	for i, p := range req.Ports {
		f := fmt.Sprintf("/ports/%d", i)
		checkPort(&v, f+"/port", p.Port)
		if seen[p.Port] {
			v.add(f+"/port", "invalid_format", "Duplicate port.")
		}
		seen[p.Port] = true
		checkLen(&v, f+"/registration", p.Registration, maxInfoLen, false)
		checkLen(&v, f+"/operator", p.Operator, maxInfoLen, false)
		checkLen(&v, f+"/technology", p.Technology, maxInfoLen, false)
		rep.ports = append(rep.ports, connector.PortInfo{Port: p.Port, Health: p.health(now)})
	}
	if v.write(w, r) {
		return
	}
	s.checkin(rep)
	h.log.Debug("agent checked in", slog.String("device_id", s.deviceID.String()), slog.Int("ports", len(rep.ports)))
	writeJSON(w, checkinResponse{
		DeviceID: s.deviceID.String(), ServerTime: now.UTC().Format(time.RFC3339),
		StaleAfterSeconds: int(h.opts.StaleAfter / time.Second),
	})
}

func (p portReport) health(now time.Time) connector.ChannelHealth {
	hl := connector.ChannelHealth{
		CheckedAt: now, Running: p.Running == nil || bool(*p.Running),
		Registration: p.Registration, Operator: p.Operator, Technology: p.Technology,
	}
	if p.Registered != nil {
		hl.Registered = bool(*p.Registered)
	} else {
		hl.Registered = registeredStates[strings.ToLower(strings.TrimSpace(p.Registration))]
	}
	sig := domain.Signal{RSSI: p.RSSI.ptr(), RSRP: p.RSRP.ptr(), RSRQ: p.RSRQ.ptr(), SINR: p.SINR.ptr()}
	if sig.RSSI != nil || sig.RSRP != nil || sig.RSRQ != nil || sig.SINR != nil {
		hl.Signal = &sig
	}
	return hl
}

func (h *Hub) handleCommands(w http.ResponseWriter, r *http.Request, s *session) {
	var req commandsRequest
	if !decode(w, r, &req, true) {
		return
	}
	var v validation
	if len(req.Ports) > maxPollPortsList {
		v.add("/ports", "out_of_range", "At most %d ports.", maxPollPortsList)
	}
	for i, p := range req.Ports {
		checkPort(&v, fmt.Sprintf("/ports/%d", i), p)
	}
	if req.Max < 0 || req.Wait < 0 {
		v.add("", "out_of_range", "max and wait must not be negative.")
	}
	if v.write(w, r) {
		return
	}
	limit := h.opts.MaxCommands
	if req.Max > 0 && int(req.Max) < limit {
		limit = int(req.Max)
	}
	wait := min(time.Duration(req.Wait)*time.Second, h.opts.MaxWait)

	cmds, wake := s.pending(req.Ports, limit)
	if len(cmds) == 0 && wait > 0 {
		select {
		case <-wake:
			cmds, _ = s.pending(req.Ports, limit)
		case <-h.clk.After(wait):
		case <-r.Context().Done():
			return
		case <-s.done:
		}
	}
	resp := commandsResponse{Commands: make([]commandDTO, 0, len(cmds)), InboxDelete: s.pendingDeletes()}
	for _, c := range cmds {
		resp.Commands = append(resp.Commands, commandDTO{
			MessageID: c.id.String(), Port: c.port, To: c.msg.To, Body: c.msg.Body,
			Encoding: string(c.msg.Encoding), Segments: c.msg.Segments, PDUs: c.pdus,
		})
	}
	if len(cmds) > 0 {
		h.log.Debug("commands delivered to the agent", slog.String("device_id", s.deviceID.String()), slog.Int("count", len(cmds)))
	}
	writeJSON(w, resp)
}

func (h *Hub) handleResult(w http.ResponseWriter, r *http.Request, s *session) {
	id, err := domain.ParseID(r.PathValue("message_id"))
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, problemNotFound, "Unknown message ID format.", nil)
		return
	}
	var req resultRequest
	if !decode(w, r, &req, false) {
		return
	}
	var v validation
	st := resultStatus(strings.ToLower(strings.TrimSpace(req.Status)))
	if !st.valid() {
		v.add("/status", "invalid_format", "Must be one of accepted, transient, ambiguous, permanent.")
	}
	checkLen(&v, "/remote_ref", req.RemoteRef, maxRemoteRefLen, false)
	if len(req.Reason) > 4*maxReasonRunes {
		v.add("/reason", "out_of_range", "At most %d bytes.", 4*maxReasonRunes)
	}
	if v.write(w, r) {
		return
	}
	known := s.resolve(id, outcome{status: st, reason: sanitizeReason(req.Reason), remoteRef: req.RemoteRef, at: h.clk.Now()})
	h.log.Info("agent reported a send result", slog.String("device_id", s.deviceID.String()),
		slog.String("message_id", id.String()), slog.String("status", string(st)), slog.Bool("known", known))
	writeJSON(w, resultResponse{MessageID: id.String(), Known: known})
}

// phoneLike matches digit runs that may be phone numbers echoed by the
// router; they are masked before the reason reaches logs and LastError.
var phoneLike = regexp.MustCompile(`\+?\d[\d ]{4,}\d`)

func sanitizeReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	s = phoneLike.ReplaceAllStringFunc(strings.TrimSpace(s), logging.MaskPhone)
	return truncate(s, maxReasonRunes)
}

func (h *Hub) handleInbox(w http.ResponseWriter, r *http.Request, s *session) {
	var req inboxRequest
	if !decode(w, r, &req, false) {
		return
	}
	var v validation
	if len(req.Messages) > maxPushMessages {
		v.add("/messages", "out_of_range", "At most %d messages per request.", maxPushMessages)
	}
	entries := make([]pushed, 0, len(req.Messages))
	for i, m := range req.Messages {
		f := fmt.Sprintf("/messages/%d", i)
		checkPort(&v, f+"/port", m.Port)
		checkLen(&v, f+"/remote_id", m.RemoteID, maxRemoteIDLen, true)
		checkLen(&v, f+"/phone", m.Phone, maxPhoneLen, false)
		checkLen(&v, f+"/timestamp", m.Timestamp, maxTimestampLen, false)
		checkLen(&v, f+"/type", m.Type, maxTypeLen, false)
		if len(m.PDU) > maxPDUHexLen {
			v.add(f+"/pdu", "out_of_range", "At most %d hex digits.", maxPDUHexLen)
		} else if _, err := hex.DecodeString(m.PDU); err != nil {
			v.add(f+"/pdu", "invalid_format", "Must be hex.")
		}
		if !utf8.ValidString(m.Body) || utf8.RuneCountInString(m.Body) > maxBodyRunes {
			v.add(f+"/body", "out_of_range", "At most %d characters of UTF-8.", maxBodyRunes)
		}
		if m.PDU == "" && m.Body == "" {
			v.add(f, "required", "pdu or body is required.")
		}
		entries = append(entries, pushed{port: m.Port, sms: connector.InboundSMS{
			RemoteID: m.RemoteID, Phone: m.Phone, RawTimestamp: m.Timestamp,
			Timestamp: h.parseTimestamp(m.Timestamp), Body: m.Body, Type: m.Type, PDU: strings.ToUpper(m.PDU),
		}})
	}
	if v.write(w, r) {
		return
	}
	accepted, refused := s.push(entries)
	if refused > 0 {
		h.log.Warn("agent inbox full; entries stay on the router", slog.String("device_id", s.deviceID.String()), slog.Int("refused", refused))
	}
	writeJSON(w, inboxResponse{Accepted: accepted, Refused: refused, InboxDelete: s.pendingDeletes()})
}

func (h *Hub) parseTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range timestampLayouts {
		if t, err := time.ParseInLocation(layout, s, h.opts.Location); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (h *Hub) handleAck(w http.ResponseWriter, r *http.Request, s *session) {
	var req ackRequest
	if !decode(w, r, &req, false) {
		return
	}
	var v validation
	if len(req.Deleted) > maxAckRefs {
		v.add("/deleted", "out_of_range", "At most %d entries.", maxAckRefs)
	}
	for i, d := range req.Deleted {
		f := fmt.Sprintf("/deleted/%d", i)
		checkPort(&v, f+"/port", d.Port)
		checkLen(&v, f+"/remote_id", d.RemoteID, maxRemoteIDLen, true)
		checkLen(&v, f+"/phone", d.Phone, maxPhoneLen, false)
		checkLen(&v, f+"/timestamp", d.Timestamp, maxTimestampLen, false)
	}
	if v.write(w, r) {
		return
	}
	n := s.acked(req.Deleted)
	writeJSON(w, ackResponse{Removed: n, InboxDelete: s.pendingDeletes()})
}

func (h *Hub) handleStatus(w http.ResponseWriter, _ *http.Request, s *session) {
	st := s.snapshot()
	ts := func(t time.Time) *string {
		if t.IsZero() {
			return nil
		}
		v := t.UTC().Format(time.RFC3339)
		return &v
	}
	writeJSON(w, statusResponse{
		DeviceID: s.deviceID.String(), LastCheckin: ts(st.lastCheckin), LastSeen: ts(st.lastSeen),
		PendingCommands: st.pendingCommands, InboxEntries: st.inboxEntries, PendingDeletes: st.pendingDeletes,
	})
}

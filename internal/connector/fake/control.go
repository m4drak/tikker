// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ControlPrefix is the URL prefix of the HTTP control API.
const ControlPrefix = "/_fake/"

// ControlHandler returns the JSON control API for end-to-end tests, meant to
// be mounted at ControlPrefix next to the RouterOS emulation. It has no
// authentication: expose it only on loopback or inside a test network.
//
//	POST   /_fake/inject    {"port","from","text","encoding":"auto|gsm7|ucs2","time":RFC3339,"class0"}
//	                        or {"raw":{"phone","timestamp_text","type","message","pdu","port"}}
//	POST   /_fake/fail      {"endpoint","kind":"http|drop|hang","status","message","detail","count","accepted","delay_ms"}
//	DELETE /_fake/fail      clear faults
//	POST   /_fake/config    {"send_delay_ms","session_timeout_ms","lenient","inbox_limit","auto_erase","rest_api","api"}
//	POST   /_fake/lte       {"port","registration","operator","access_technology","running","signal":{...},"no_signal"}
//	GET    /_fake/sent      accepted sends
//	GET    /_fake/inbox     stored inbox
//	GET    /_fake/requests  per-endpoint counters
//	POST   /_fake/reset     back to the initial state
func (s *Server) ControlHandler() http.Handler {
	return http.HandlerFunc(s.serveControl)
}

func ctlErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func ctlDecode(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, ControlPrefix), "/")
	switch {
	case name == "inject" && r.Method == http.MethodPost:
		s.ctlInject(w, r)
	case name == "fail" && r.Method == http.MethodPost:
		s.ctlFail(w, r)
	case name == "fail" && r.Method == http.MethodDelete:
		s.ClearFaults()
		w.WriteHeader(http.StatusNoContent)
	case name == "config" && r.Method == http.MethodPost:
		s.ctlConfig(w, r)
	case name == "lte" && r.Method == http.MethodPost:
		s.ctlLTE(w, r)
	case name == "sent" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.ctlSent())
	case name == "inbox" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.ctlInbox())
	case name == "requests" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"endpoints": s.AllRequests(), "auth_failures": s.AuthFailures(),
		})
	case name == "reset" && r.Method == http.MethodPost:
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	default:
		ctlErr(w, http.StatusNotFound, "unknown control endpoint")
	}
}

func parseEncoding(s string) (Encoding, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return EncodingAuto, nil
	case "gsm7", "gsm-7":
		return EncodingGSM7, nil
	case "ucs2", "ucs-2":
		return EncodingUCS2, nil
	}
	return 0, errors.New("encoding must be auto, gsm7 or ucs2")
}

func (s *Server) ctlInject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port     string    `json:"port"`
		From     string    `json:"from"`
		Text     string    `json:"text"`
		Encoding string    `json:"encoding"`
		Time     time.Time `json:"time"`
		Class0   bool      `json:"class0"`
		Raw      *struct {
			Port          string    `json:"port"`
			Phone         string    `json:"phone"`
			Timestamp     time.Time `json:"timestamp"`
			TimestampText string    `json:"timestamp_text"`
			Type          string    `json:"type"`
			Message       string    `json:"message"`
			PDU           string    `json:"pdu"`
		} `json:"raw"`
	}
	if err := ctlDecode(r, &req); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Raw != nil {
		id, err := s.InjectRaw(RawInbox{
			Port: req.Raw.Port, Phone: req.Raw.Phone, Timestamp: req.Raw.Timestamp,
			TimestampText: req.Raw.TimestampText, Type: req.Raw.Type,
			Message: req.Raw.Message, PDU: req.Raw.PDU,
		})
		if err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ids": []string{id}})
		return
	}
	enc, err := parseEncoding(req.Encoding)
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ids, err := s.InjectSMS(IncomingSMS{
		Port: req.Port, From: req.From, Text: req.Text, Time: req.Time,
		Encoding: enc, Class0: req.Class0,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrInboxFull) {
			status = http.StatusInsufficientStorage
		}
		ctlErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
}

func (s *Server) ctlFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Kind     string `json:"kind"`
		Status   int    `json:"status"`
		Message  string `json:"message"`
		Detail   string `json:"detail"`
		Count    int    `json:"count"`
		Accepted bool   `json:"accepted"`
		DelayMS  int    `json:"delay_ms"`
	}
	if err := ctlDecode(r, &req); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f := Fault{
		Status: req.Status, Message: req.Message, Detail: req.Detail,
		Accepted: req.Accepted, Delay: time.Duration(req.DelayMS) * time.Millisecond,
	}
	switch req.Kind {
	case "", "http":
		f.Kind = FaultHTTP
	case "drop":
		f.Kind = FaultDrop
	case "hang":
		f.Kind = FaultHang
	default:
		ctlErr(w, http.StatusBadRequest, "kind must be http, drop or hang")
		return
	}
	ep := Endpoint(req.Endpoint)
	if ep == "" {
		ep = EndpointAny
	}
	if req.Count == 0 {
		req.Count = 1
	}
	s.FailNext(ep, f, req.Count)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ctlConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SendDelayMS      *int  `json:"send_delay_ms"`
		SessionTimeoutMS *int  `json:"session_timeout_ms"`
		Lenient          *bool `json:"lenient"`
		InboxLimit       *int  `json:"inbox_limit"`
		AutoErase        *bool `json:"auto_erase"`
		RESTAPI          *bool `json:"rest_api"`
		API              *bool `json:"api"`
	}
	if err := ctlDecode(r, &req); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.SendDelayMS != nil {
		s.SetSendDelay(time.Duration(*req.SendDelayMS) * time.Millisecond)
	}
	if req.SessionTimeoutMS != nil {
		s.SetSessionTimeout(time.Duration(*req.SessionTimeoutMS) * time.Millisecond)
	}
	if req.Lenient != nil {
		s.SetLenient(*req.Lenient)
	}
	if req.InboxLimit != nil {
		s.SetInboxLimit(*req.InboxLimit)
	}
	if req.AutoErase != nil {
		s.SetSettings(func(st *Settings) { st.AutoErase = *req.AutoErase })
	}
	if req.RESTAPI != nil || req.API != nil {
		s.mu.Lock()
		if req.RESTAPI != nil {
			s.policies.RESTAPI = *req.RESTAPI
		}
		if req.API != nil {
			s.policies.API = *req.API
		}
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ctlLTE(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port             string  `json:"port"`
		Registration     *string `json:"registration"`
		Operator         *string `json:"operator"`
		AccessTechnology *string `json:"access_technology"`
		Running          *bool   `json:"running"`
		Signal           *Signal `json:"signal"`
		NoSignal         bool    `json:"no_signal"`
	}
	if err := ctlDecode(r, &req); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Port == "" {
		req.Port = s.opts.Ports[0]
	}
	ok := s.SetLTE(req.Port, func(l *LTEState) {
		if req.Registration != nil {
			l.Registration = *req.Registration
		}
		if req.Operator != nil {
			l.Operator = *req.Operator
		}
		if req.AccessTechnology != nil {
			l.AccessTechnology = *req.AccessTechnology
		}
		if req.Running != nil {
			l.Running = *req.Running
		}
		if req.Signal != nil {
			sig := *req.Signal
			l.Signal = &sig
		}
		if req.NoSignal {
			l.Signal = nil
		}
	})
	if !ok {
		ctlErr(w, http.StatusBadRequest, "unknown port")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sentJSON struct {
	Seq         int       `json:"seq"`
	Port        string    `json:"port"`
	PhoneNumber string    `json:"phone_number"`
	Message     string    `json:"message"`
	Type        string    `json:"type,omitempty"`
	Septets     int       `json:"septets"`
	ReceivedAt  time.Time `json:"received_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Ambiguous   bool      `json:"ambiguous"`
}

func (s *Server) ctlSent() []sentJSON {
	sent := s.Sent()
	out := make([]sentJSON, 0, len(sent))
	for _, m := range sent {
		out = append(out, sentJSON(m))
	}
	return out
}

type inboxCtlJSON struct {
	ID        string    `json:"id"`
	Port      string    `json:"port"`
	Phone     string    `json:"phone"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	PDU       string    `json:"pdu"`
}

func (s *Server) ctlInbox() []inboxCtlJSON {
	in := s.Inbox()
	out := make([]inboxCtlJSON, 0, len(in))
	for _, e := range in {
		out = append(out, inboxCtlJSON{
			ID: e.ID, Port: e.Port, Phone: e.Phone, Timestamp: e.Timestamp,
			Type: e.Type, Message: e.Message, PDU: e.PDU,
		})
	}
	return out
}

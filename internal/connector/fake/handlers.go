// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errBody is the RouterOS REST error document (research W6).
type errBody struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, errBody{Error: status, Message: http.StatusText(status), Detail: detail})
}

// classify maps a request to an endpoint and, for inbox deletes, the item id.
func classify(path string) (ep Endpoint, id string) {
	p, ok := strings.CutPrefix(path, "/rest/")
	if !ok {
		return EndpointUnknown, ""
	}
	p = strings.TrimSuffix(p, "/")
	switch p {
	case "tool/sms/send":
		return EndpointSend, ""
	case "tool/sms/inbox":
		return EndpointInbox, ""
	case "tool/sms/inbox/remove":
		return EndpointInboxRemove, ""
	case "tool/sms":
		return EndpointSettings, ""
	case "interface/lte":
		return EndpointLTE, ""
	case "interface/lte/monitor":
		return EndpointLTEMonitor, ""
	case "system/resource":
		return EndpointResource, ""
	case "system/routerboard":
		return EndpointRouterboard, ""
	}
	if rest, ok := strings.CutPrefix(p, "tool/sms/inbox/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return EndpointInboxDelete, rest
	}
	return EndpointUnknown, ""
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	ep, id := classify(r.URL.Path)
	s.mu.Lock()
	s.requests[ep]++
	s.mu.Unlock()

	f := s.popFault(ep)
	if f != nil && (!f.Accepted || ep != EndpointSend) {
		s.act(w, r, f)
		return
	}
	switch ep {
	case EndpointSend:
		s.handleSend(w, r, f)
	case EndpointInbox:
		s.handleInbox(w, r)
	case EndpointInboxDelete:
		s.handleInboxDelete(w, r, id)
	case EndpointInboxRemove:
		s.handleInboxRemove(w, r)
	case EndpointSettings:
		s.handleSettings(w, r)
	case EndpointLTE:
		s.handleLTEList(w, r)
	case EndpointLTEMonitor:
		s.handleLTEMonitor(w, r)
	case EndpointResource:
		s.handleResource(w, r)
	case EndpointRouterboard:
		s.handleRouterboard(w, r)
	default:
		// ASSUMPTION(T00): RouterOS reports unknown paths like the console does.
		writeErr(w, http.StatusBadRequest, "no such command or directory ("+strings.TrimPrefix(r.URL.Path, "/rest/")+")")
	}
}

// authorize checks basic auth and the user policies (research W2). It writes
// the error response itself.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	s.mu.Lock()
	good := ok &&
		subtle.ConstantTimeCompare([]byte(u), []byte(s.user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(p), []byte(s.pass)) == 1
	pol := s.policies
	if !good || !pol.RESTAPI {
		s.authFailures++
	}
	s.mu.Unlock()
	if !good || !pol.RESTAPI {
		// Missing rest-api policy and bad credentials both give 401.
		w.Header().Set("WWW-Authenticate", `Basic realm="fake-routeros"`)
		writeErr(w, http.StatusUnauthorized, "")
		return false
	}
	if !pol.API {
		s.mu.Lock()
		s.authFailures++
		s.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "not allowed (9)")
		return false
	}
	return true
}

// methodOK enforces the HTTP method; on mismatch it writes the answer.
// ASSUMPTION(T00): only GET on /tool/sms/send is documented (500, research
// W3); other mismatches answer 405.
func methodOK(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/rest/tool/sms/send" {
		writeErr(w, http.StatusInternalServerError, "")
		return false
	}
	writeErr(w, http.StatusMethodNotAllowed, "")
	return false
}

// readJSON enforces Content-Type: application/json (415 otherwise, research
// W3) and decodes the body into a map. An empty body is {}.
func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "")
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Failed to parse json")
		return nil, false
	}
	out := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return out, true
	}
	if err := json.Unmarshal(data, &out); err != nil {
		writeErr(w, http.StatusBadRequest, "Failed to parse json")
		return nil, false
	}
	return out, true
}

func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// validPhone: digits, '*', '#', letters a-c, optional leading '+' (research W3).
func validPhone(p string) bool {
	p = strings.TrimPrefix(p, "+")
	if p == "" {
		return false
	}
	for _, c := range p {
		if (c < '0' || c > '9') && c != '*' && c != '#' && (c < 'a' || c > 'c') && (c < 'A' || c > 'C') {
			return false
		}
	}
	return true
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, f *Fault) {
	received := time.Now()
	if !methodOK(w, r, http.MethodPost) {
		return
	}
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	port, okPort := strField(body, "port")
	phone, okPhone := strField(body, "phone-number")
	msg, okMsg := strField(body, "message")
	// ASSUMPTION(T00): wording of the missing-argument errors.
	for _, m := range []struct {
		name string
		ok   bool
	}{{"port", okPort}, {"phone-number", okPhone}, {"message", okMsg}} {
		if !m.ok {
			writeErr(w, http.StatusBadRequest, "missing value(s) of argument(s) "+m.name)
			return
		}
	}
	sem, known := s.portSem[port]
	if !known {
		writeErr(w, http.StatusBadRequest, "input does not match any value of port")
		return
	}
	if !validPhone(phone) {
		writeErr(w, http.StatusBadRequest, "invalid phone-number")
		return
	}
	septets, isGSM := gsm7Encode(msg)
	s.mu.Lock()
	lenient := s.lenient
	s.mu.Unlock()
	if !lenient {
		// Research W4/W5: GSM-7 only, 160 characters. The real router may
		// silently mangle instead of rejecting (forum F2); rejecting is the
		// strictest behaviour a client must cope with.
		// ASSUMPTION(T00): status 400 and wording "Body too long" (F3).
		if !isGSM {
			writeErr(w, http.StatusBadRequest, "failure: message contains characters outside the GSM 7-bit alphabet")
			return
		}
		if len(septets) > 160 {
			writeErr(w, http.StatusBadRequest, "failure: Body too long")
			return
		}
	}

	// One command per modem port at a time; others queue (research W7).
	select {
	case sem <- struct{}{}:
	case <-r.Context().Done():
		return
	case <-s.done:
		return
	}
	defer func() { <-sem }()

	s.mu.Lock()
	st := s.lte[port]
	registered := st.Registered()
	delay, timeout := s.sendDelay, s.sessionTimeout
	s.mu.Unlock()
	if !registered {
		// ASSUMPTION(T00): no registration is a 500 (transient), text unknown.
		writeErr(w, http.StatusInternalServerError, "failure: not registered to network")
		return
	}

	rec := SentMessage{
		Port: port, PhoneNumber: phone, Message: msg, Septets: len(septets),
		ReceivedAt: received, StartedAt: time.Now(),
	}
	rec.Type, _ = strField(body, "type")
	sessionClosed := delay >= timeout
	if sessionClosed {
		// The REST session ends while the modem is still busy.
		// ASSUMPTION(T00): the SMS still goes out after "Session closed".
		delay = timeout
	}
	if !s.sleep(delay) {
		return
	}
	rec.CompletedAt = time.Now()
	rec.Ambiguous = sessionClosed || f != nil
	s.mu.Lock()
	rec.Seq = len(s.sent) + 1
	s.sent = append(s.sent, rec)
	s.mu.Unlock()

	switch {
	case f != nil:
		s.act(w, r, f)
	case sessionClosed:
		writeErr(w, http.StatusBadRequest, "Session closed")
	default:
		// ASSUMPTION(T00): a command without return value answers [].
		writeJSON(w, http.StatusOK, []any{})
	}
}

type inboxJSON struct {
	ID        string `json:".id"`
	Phone     string `json:"phone"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	PDU       string `json:"pdu"`
	Port      string `json:"port,omitempty"`
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodGet) {
		return
	}
	s.mu.Lock()
	out := make([]inboxJSON, 0, len(s.inbox))
	for _, e := range s.inbox {
		ts := e.rawTimestamp
		if ts == "" {
			ts = s.opts.FormatTimestamp(e.Timestamp)
		}
		j := inboxJSON{ID: e.ID, Phone: e.Phone, Timestamp: ts, Type: e.Type, Message: e.Message, PDU: e.PDU}
		if s.opts.InboxPortField {
			j.Port = e.Port
		}
		out = append(out, j)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// removeInbox deletes the given ids; all must exist. Caller holds no lock.
func (s *Server) removeInbox(ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := map[string]int{}
	for i, e := range s.inbox {
		idx[e.ID] = i
	}
	drop := map[int]bool{}
	for _, id := range ids {
		i, ok := idx[id]
		if !ok {
			return false
		}
		drop[i] = true
	}
	kept := s.inbox[:0:0]
	for i, e := range s.inbox {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	s.inbox = kept
	return true
}

func (s *Server) handleInboxDelete(w http.ResponseWriter, r *http.Request, id string) {
	if !methodOK(w, r, http.MethodDelete) {
		return
	}
	// ASSUMPTION(T00): 204 on success, 404 "no such item (4)" otherwise (W10).
	if !s.removeInbox([]string{id}) {
		writeErr(w, http.StatusNotFound, "no such item (4)")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInboxRemove(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodPost) {
		return
	}
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	raw, ok := strField(body, ".id")
	if !ok || raw == "" {
		writeErr(w, http.StatusBadRequest, "missing value(s) of argument(s) numbers")
		return
	}
	// ASSUMPTION(T00): comma-separated id lists work as in the console, the
	// whole call fails with "no such item (4)" if any id is unknown, and the
	// success answer is [].
	if !s.removeInbox(strings.Split(raw, ",")) {
		writeErr(w, http.StatusBadRequest, "no such item (4)")
		return
	}
	writeJSON(w, http.StatusOK, []any{})
}

func yesNo(b bool) string { return strconv.FormatBool(b) }

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodGet) {
		return
	}
	s.mu.Lock()
	st := s.settings
	s.mu.Unlock()
	// RouterOS REST renders booleans as "true"/"false" strings.
	writeJSON(w, http.StatusOK, map[string]string{
		"port":                       st.Port,
		"receive-enabled":            yesNo(st.ReceiveEnabled),
		"auto-erase":                 yesNo(st.AutoErase),
		"remove-sent-sms-after-send": yesNo(st.RemoveSentSMSAfterSend),
		"sms-storage":                st.SMSStorage,
		"polling":                    yesNo(st.Polling),
		"channel":                    "0",
	})
}

func (s *Server) handleLTEList(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodGet) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]string, 0, len(s.ports()))
	for i, p := range s.ports() {
		st := s.lte[p]
		out = append(out, map[string]string{
			".id":          fmt.Sprintf("*%d", i+1),
			"name":         p,
			"default-name": p,
			"running":      yesNo(st.Running),
			"disabled":     "false",
			"mtu":          "1500",
			"mac-address":  fmt.Sprintf("02:00:00:00:00:%02X", i+1),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLTEMonitor(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodPost) {
		return
	}
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	num, ok := strField(body, "numbers")
	if !ok || num == "" {
		writeErr(w, http.StatusBadRequest, "missing value(s) of argument(s) numbers")
		return
	}
	port := ""
	for i, p := range s.ports() {
		if num == p || num == fmt.Sprintf("*%d", i+1) {
			port = p
		}
	}
	if port == "" {
		writeErr(w, http.StatusBadRequest, "no such item (4)")
		return
	}
	once, _ := strField(body, "once")
	if once != "true" && once != "yes" {
		// ASSUMPTION(T00): without "once" monitor streams until the REST
		// session limit hits. Catches clients that forget "once".
		s.act(w, r, &Fault{Kind: FaultHang})
		return
	}
	s.mu.Lock()
	st := *s.lte[port]
	var sig *Signal
	if st.Signal != nil {
		c := *st.Signal
		sig = &c
	}
	s.mu.Unlock()

	// ASSUMPTION(T00): field names and value strings of the monitor output.
	status := "connected"
	if !st.Registered() {
		status = "searching"
	}
	m := map[string]string{
		"status":              status,
		"registration-status": st.Registration,
		"current-operator":    st.Operator,
		"access-technology":   st.AccessTechnology,
		"imei":                st.IMEI,
		"imsi":                st.IMSI,
		"current-cellid":      st.CellID,
		"primary-band":        st.Band,
		"pin-status":          "ok",
		"functionality":       "full",
	}
	if sig != nil {
		m["rssi"] = strconv.Itoa(sig.RSSI)
		m["rsrp"] = strconv.Itoa(sig.RSRP)
		m["rsrq"] = strconv.Itoa(sig.RSRQ)
		m["sinr"] = strconv.Itoa(sig.SINR)
	}
	writeJSON(w, http.StatusOK, []map[string]string{m})
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"version":            s.opts.Version,
		"architecture-name":  "arm64",
		"board-name":         s.opts.Model,
		"platform":           "MikroTik",
		"cpu":                "ARM64",
		"cpu-count":          "4",
		"total-memory":       "1073741824",
		"free-memory":        "805306368",
		"uptime":             "1d2h3m4s",
		"build-time":         "2026-01-01 00:00:00",
		"factory-software":   s.opts.Version,
		"total-hdd-space":    "134217728",
		"free-hdd-space":     "100663296",
		"cpu-load":           "1",
		"write-sect-total":   "0",
		"bad-blocks":         "0",
		"architecture-flags": "",
	})
}

func (s *Server) handleRouterboard(w http.ResponseWriter, r *http.Request) {
	if !methodOK(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"routerboard":      "true",
		"model":            s.opts.Model,
		"serial-number":    "FAKE0000001",
		"firmware-type":    "ipq6000",
		"factory-firmware": "7.10",
		"current-firmware": "7.16.2",
		"upgrade-firmware": "7.16.2",
	})
}

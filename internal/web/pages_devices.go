// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/agent"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
)

// Notices of the device pages (?notice=...).
const (
	noticeCreated        = "created"
	noticeSaved          = "saved"
	noticeEnabled        = "enabled"
	noticeDisabled       = "disabled"
	noticeChannelAdded   = "channel_added"
	noticeChannelDeleted = "channel_deleted"
	noticeDeviceDeleted  = "device_deleted"
)

func (s *Server) registerDevices() {
	s.HandleFunc("GET /devices", domain.RoleAdmin, s.devices)
	s.HandleFunc("GET /devices/new", domain.RoleAdmin, s.deviceNew)
	s.HandleFunc("POST /devices", domain.RoleAdmin, s.deviceCreate)
	s.HandleFunc("GET /devices/{id}", domain.RoleAdmin, s.deviceDetails)
	s.HandleFunc("GET /devices/{id}/form", domain.RoleAdmin, s.deviceFormFragment)
	s.HandleFunc("POST /devices/{id}", domain.RoleAdmin, s.deviceUpdate)
	s.HandleFunc("POST /devices/{id}/toggle", domain.RoleAdmin, s.deviceToggle)
	s.HandleFunc("POST /devices/{id}/test", domain.RoleAdmin, s.deviceTest)
	s.HandleFunc("GET /devices/{id}/delete", domain.RoleAdmin, s.deviceDeleteConfirm)
	s.HandleFunc("POST /devices/{id}/delete", domain.RoleAdmin, s.deviceDelete)
	s.HandleFunc("POST /devices/{id}/channels", domain.RoleAdmin, s.channelCreate)
}

// tlsModes are the modes of the form, in display order, with their keys.
var tlsModes = []struct {
	Mode domain.TLSMode
	Key  string
}{
	{domain.TLSVerify, "dev.tls.verify"},
	{domain.TLSCustomCA, "dev.tls.ca"},
	{domain.TLSFingerprint, "dev.tls.fingerprint"},
	{domain.TLSInsecure, "dev.tls.insecure"},
}

// tlsModeKey is the translation key of a TLS mode.
func tlsModeKey(m domain.TLSMode) string {
	for _, t := range tlsModes {
		if t.Mode == m {
			return t.Key
		}
	}
	return "dev.tls.verify"
}

// deviceForm is what the user typed. The password is never part of it: the
// field is always rendered empty.
type deviceForm struct {
	Connector   string // domain.ConnectorKind; "" means routeros-rest
	Name        string
	BaseURL     string
	Username    string
	TLSMode     string
	CAPEM       string
	Fingerprint string
	Enabled     bool
}

// deviceFormData is the data of the "device_form" block.
type deviceFormData struct {
	ID       domain.ID // empty: a new device
	Form     deviceForm
	Errors   map[string]string // form field -> translated message
	Modes    []optionItem
	Insecure bool // the typed settings are unencrypted or unauthenticated
	Agent    bool // the device uses the agent connector (no connection settings)
	// AgentURL is where the router script connects (base URL + /agent/v1/).
	AgentURL string
}

type deviceRow struct {
	D        domain.Device
	TLSKey   string
	Insecure bool
	Channels []adminChannelRow
}

type adminChannelRow struct {
	ID          domain.ID
	Name        string
	Port        string
	Tags        []string
	State       domain.ChannelState
	StateKey    string
	Reason      string
	Operator    string
	Signal      string
	Enabled     bool
	RatePerMin  int
	MaxSegments int
	Translit    bool
	Queue       int
}

type devicesData struct {
	Rows   []deviceRow
	Notice string
}

type deviceData struct {
	Device   domain.Device
	Form     deviceFormData
	Channels []adminChannelRow
	Insecure bool
	Notice   string
	Alert    string // translated, e.g. a failed channel creation
}

func formOf(d domain.Device) deviceForm {
	return deviceForm{Connector: string(d.Connector), Name: d.Name, BaseURL: d.BaseURL, Username: d.Username, TLSMode: string(d.TLSMode),
		CAPEM: d.CAPEM, Fingerprint: d.Fingerprint, Enabled: d.Enabled}
}

func (s *Server) formData(id domain.ID, f deviceForm, errs map[string]string) deviceFormData {
	d := deviceFormData{ID: id, Form: f, Errors: errs}
	for _, m := range tlsModes {
		d.Modes = append(d.Modes, optionItem{Value: string(m.Mode), LabelKey: m.Key, Selected: f.TLSMode == string(m.Mode)})
	}
	d.Agent = f.Connector == string(domain.ConnectorAgent)
	d.Insecure = !d.Agent && domain.Device{BaseURL: f.BaseURL, TLSMode: domain.TLSMode(f.TLSMode)}.InsecureTransport() && f.BaseURL != ""
	if d.Agent {
		d.AgentURL = strings.TrimRight(s.baseURL, "/") + agent.Prefix + "/"
	}
	return d
}

func readDeviceForm(r *http.Request) deviceForm {
	return deviceForm{
		Connector: r.PostFormValue("connector"),
		Name:      strings.TrimSpace(r.PostFormValue("name")), BaseURL: strings.TrimSpace(r.PostFormValue("base_url")),
		Username: r.PostFormValue("username"), TLSMode: r.PostFormValue("tls_mode"),
		CAPEM: strings.TrimSpace(r.PostFormValue("ca_pem")), Fingerprint: strings.TrimSpace(r.PostFormValue("fingerprint")),
		Enabled: r.PostFormValue("enabled") != "",
	}
}

// channelRows builds the rows of the channels of one device.
func (s *Server) channelRows(r *http.Request, dev domain.Device) ([]adminChannelRow, error) {
	chs, err := s.st.Channels().List(r.Context(), store.ChannelFilter{DeviceID: dev.ID})
	if err != nil {
		return nil, err //nolint:wrapcheck // logged by the caller
	}
	var rows []adminChannelRow
	for _, c := range chs {
		v, err := s.admin.View(r.Context(), c, dev.Enabled)
		if err != nil {
			return nil, err //nolint:wrapcheck // logged by the caller
		}
		rows = append(rows, s.channelRowOf(v))
	}
	return rows, nil
}

func (s *Server) channelRowOf(v admin.ChannelView) adminChannelRow {
	c, st := v.Channel, v.State()
	return adminChannelRow{
		ID: c.ID, Name: channelName(c), Port: c.Port, Tags: c.Tags, State: st, StateKey: stateKey(st),
		Reason: c.StateReason, Operator: c.Operator, Signal: signalText(c.Signal), Enabled: c.Enabled,
		RatePerMin: c.RatePerMin, MaxSegments: c.MaxSegments, Translit: c.Transliterate, Queue: v.QueueDepth,
	}
}

// devices lists the devices with the state of their channels.
func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.st.Devices().List(r.Context())
	if err != nil {
		s.internalError(w, r, "list devices failed", err)
		return
	}
	data := devicesData{Notice: notice(r.URL.Query().Get("notice"), noticeDeviceDeleted, noticeEnabled, noticeDisabled)}
	for _, d := range devs {
		rows, err := s.channelRows(r, d)
		if err != nil {
			s.internalError(w, r, "list channels failed", err)
			return
		}
		data.Rows = append(data.Rows, deviceRow{D: d, TLSKey: tlsModeKey(d.TLSMode), Insecure: d.InsecureTransport(), Channels: rows})
	}
	s.Render(w, r, http.StatusOK, "devices", data)
}

func (s *Server) deviceNew(w http.ResponseWriter, r *http.Request) {
	f := deviceForm{TLSMode: string(domain.TLSVerify), Enabled: true}
	if r.URL.Query().Get("connector") == string(domain.ConnectorAgent) {
		f.Connector = string(domain.ConnectorAgent)
	}
	s.Render(w, r, http.StatusOK, "device", deviceData{Form: s.formData("", f, nil)})
}

// deviceCreate stores a new device and goes to its page.
func (s *Server) deviceCreate(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	f := readDeviceForm(r)
	pw := secrets.Password(r.PostFormValue("password"))
	req := admin.DeviceCreate{
		Name: &f.Name, BaseURL: &f.BaseURL, Username: &f.Username, Password: &pw, TLSMode: &f.TLSMode,
		CAPEM: &f.CAPEM, Fingerprint: &f.Fingerprint, Enabled: &f.Enabled,
	}
	if f.Connector != "" {
		req.Connector = &f.Connector
	}
	if f.Connector == string(domain.ConnectorAgent) {
		tok := secrets.Password(r.PostFormValue("agent_token"))
		req.AgentToken = &tok
		req.Password = nil
	}
	d, err := s.admin.CreateDevice(r.Context(), p.Actor(), req)
	lang := s.language(r)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+string(d.ID)+"?notice="+noticeCreated, http.StatusSeeOther)
		return
	case errors.Is(err, admin.ErrDuplicate):
		s.Render(w, r, http.StatusConflict, "device", deviceData{Form: s.formData("", f, map[string]string{"name": s.i18n.T(lang, "dev.err.duplicate")})})
		return
	}
	if ps, ok := problemsOf(err); ok {
		s.Render(w, r, http.StatusUnprocessableEntity, "device", deviceData{Form: s.formData("", f, s.fieldMessages(lang, ps))})
		return
	}
	s.internalError(w, r, "create device failed", err)
}

// loadDevice reads the device of the path; false means the response was
// written.
func (s *Server) loadDevice(w http.ResponseWriter, r *http.Request) (domain.Device, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return domain.Device{}, false
	}
	d, err := s.admin.GetDevice(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		s.notFound(w, r)
		return d, false
	}
	if err != nil {
		s.internalError(w, r, "get device failed", err)
		return d, false
	}
	return d, true
}

// devicePage renders the page of d with the given form state.
func (s *Server) devicePage(w http.ResponseWriter, r *http.Request, status int, d domain.Device, fd deviceFormData, noticeValue, alert string) {
	rows, err := s.channelRows(r, d)
	if err != nil {
		s.internalError(w, r, "list channels failed", err)
		return
	}
	s.Render(w, r, status, "device", deviceData{
		Device: d, Form: fd, Channels: rows, Insecure: d.InsecureTransport(), Notice: noticeValue, Alert: alert,
	})
}

func (s *Server) deviceDetails(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	n := notice(r.URL.Query().Get("notice"), noticeCreated, noticeSaved, noticeEnabled, noticeDisabled, noticeChannelAdded, noticeChannelDeleted)
	s.devicePage(w, r, http.StatusOK, d, s.formData(d.ID, formOf(d), nil), n, "")
}

// deviceFormFragment renders the edit form (htmx), optionally with the TLS
// mode and the fingerprint of the query filled in: the "pin this fingerprint"
// button of the test result uses it. Nothing is saved.
func (s *Server) deviceFormFragment(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	f := formOf(d)
	q := r.URL.Query()
	if m, err := domain.ParseTLSMode(q.Get("tls_mode")); err == nil {
		f.TLSMode = string(m)
	}
	if fp, ok := admin.NormalizeFingerprint(q.Get("fingerprint")); ok {
		f.Fingerprint = fp
	}
	s.RenderBlock(w, r, http.StatusOK, "device", "device_form", deviceData{Device: d, Form: s.formData(d.ID, f, nil)})
}

// deviceUpdate saves the form. An empty password field keeps the stored
// password.
func (s *Server) deviceUpdate(w http.ResponseWriter, r *http.Request) {
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
	f := readDeviceForm(r)
	ch := admin.DeviceChanges{
		Name: &f.Name, BaseURL: &f.BaseURL, Username: &f.Username, TLSMode: &f.TLSMode,
		CAPEM: &f.CAPEM, Fingerprint: &f.Fingerprint, Enabled: &f.Enabled,
	}
	if pw := r.PostFormValue("password"); pw != "" {
		sp := secrets.Password(pw)
		ch.Password = &sp
	}
	f.Connector = string(d.Connector) // the kind cannot change
	if d.Connector == domain.ConnectorAgent {
		// The agent form has no connection settings.
		ch = admin.DeviceChanges{Name: &f.Name, Enabled: &f.Enabled}
		if tok := r.PostFormValue("agent_token"); tok != "" {
			st := secrets.Password(tok)
			ch.AgentToken = &st
		}
	}
	_, err := s.admin.UpdateDevice(r.Context(), p.Actor(), d.ID, ch)
	lang := s.language(r)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+string(d.ID)+"?notice="+noticeSaved, http.StatusSeeOther)
		return
	case errors.Is(err, domain.ErrNotFound):
		s.notFound(w, r)
		return
	case errors.Is(err, admin.ErrDuplicate):
		s.devicePage(w, r, http.StatusConflict, d, s.formData(d.ID, f, map[string]string{"name": s.i18n.T(lang, "dev.err.duplicate")}), "", "")
		return
	}
	if ps, ok := problemsOf(err); ok {
		s.devicePage(w, r, http.StatusUnprocessableEntity, d, s.formData(d.ID, f, s.fieldMessages(lang, ps)), "", "")
		return
	}
	s.internalError(w, r, "update device failed", err)
}

// deviceToggle enables or disables the device (form field "enabled").
func (s *Server) deviceToggle(w http.ResponseWriter, r *http.Request) {
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
	enable := r.PostFormValue("enabled") == "true"
	if _, err := s.admin.UpdateDevice(r.Context(), p.Actor(), d.ID, admin.DeviceChanges{Enabled: &enable}); err != nil {
		s.internalError(w, r, "toggle device failed", err)
		return
	}
	n := noticeDisabled
	if enable {
		n = noticeEnabled
	}
	back := auth.SafeRedirect(r.PostFormValue("next"))
	if back == "/" || !strings.HasPrefix(back, "/devices") {
		back = "/devices/" + string(d.ID)
	}
	http.Redirect(w, r, strings.SplitN(back, "?", 2)[0]+"?notice="+n, http.StatusSeeOther)
}

// testView is the result of "test connection" as the template shows it.
type testView struct {
	DeviceID    domain.ID
	OK          bool
	CheckedAt   string
	LatencyMS   int64
	HasLatency  bool
	Version     string
	Board       string
	TLS         *testTLSView
	Ports       []testPortView
	ErrorKey    string // translation key of the error code
	ErrorDetail string
}

type testTLSView struct {
	ModeKey     string
	Verified    bool
	Fingerprint string
	PinURL      string // hx-get target that fills the form; "" when already pinned
}

type testPortView struct {
	Port         string
	Running      bool
	Operator     string
	Registration string
	Signal       string
	Channel      domain.ID // "" when there is no channel yet
}

// testErrKeys maps the error code of a failed test to its translation key.
var testErrKeys = map[domain.ErrorCode]string{
	domain.CodeUnreachable:        "dev.test.err.unreachable",
	domain.CodeTimeout:            "dev.test.err.timeout",
	domain.CodeTLSError:           "dev.test.err.tls_error",
	domain.CodeAuthFailed:         "dev.test.err.auth_failed",
	domain.CodeInsufficientPolicy: "dev.test.err.insufficient_policy",
	domain.CodeUnsupportedVersion: "dev.test.err.unsupported_version",
	domain.CodeUnexpectedResponse: "dev.test.err.unexpected_response",
}

// deviceTest probes the device and answers with the "device_test" fragment.
func (s *Server) deviceTest(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	res, err := s.admin.TestDevice(r.Context(), d.ID)
	if errors.Is(err, domain.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, r, "test device failed", err)
		return
	}
	v := testView{DeviceID: d.ID, OK: res.OK, CheckedAt: formatTime(res.CheckedAt), Version: res.RouterOSVersion, Board: res.BoardName}
	if res.Latency != nil {
		v.HasLatency, v.LatencyMS = true, res.Latency.Milliseconds()
	}
	if res.TLS != nil {
		t := &testTLSView{ModeKey: tlsModeKey(domain.TLSMode(res.TLS.Mode)), Verified: res.TLS.Verified, Fingerprint: res.TLS.PeerFingerprint}
		if t.Fingerprint != "" && (d.TLSMode != domain.TLSFingerprint || d.Fingerprint != t.Fingerprint) {
			q := url.Values{"tls_mode": {string(domain.TLSFingerprint)}, "fingerprint": {t.Fingerprint}}
			t.PinURL = "/devices/" + string(d.ID) + "/form?" + q.Encode()
		}
		v.TLS = t
	}
	for _, pi := range res.Ports {
		v.Ports = append(v.Ports, testPortView{Port: pi.Port, Running: pi.Running, Operator: pi.Operator,
			Registration: pi.Registration, Signal: signalText(pi.Signal), Channel: pi.ChannelID})
	}
	if res.Error != nil {
		v.ErrorKey = testErrKeys[res.Error.Code]
		if v.ErrorKey == "" {
			v.ErrorKey = "dev.test.err.unexpected_response"
		}
		v.ErrorDetail = res.Error.Detail
	}
	s.RenderBlock(w, r, http.StatusOK, "device", "device_test", v)
}

func (s *Server) deviceDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	s.renderDeviceConfirm(w, r, http.StatusOK, d, "")
}

func (s *Server) renderDeviceConfirm(w http.ResponseWriter, r *http.Request, status int, d domain.Device, errText string) {
	lang := s.language(r)
	s.Render(w, r, status, "confirm", confirmData{
		Heading: s.i18n.T(lang, "dev.delete.heading", d.Name), Text: s.i18n.T(lang, "dev.delete.text"),
		Action: "/devices/" + string(d.ID) + "/delete", Back: "/devices/" + string(d.ID),
		Error: errText, Button: s.i18n.T(lang, "dev.delete.button"),
	})
}

func (s *Server) deviceDelete(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	err := s.admin.DeleteDevice(r.Context(), p.Actor(), d.ID)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices?notice="+noticeDeviceDeleted, http.StatusSeeOther)
	case errors.Is(err, domain.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, domain.ErrConflict):
		s.renderDeviceConfirm(w, r, http.StatusConflict, d, s.i18n.T(s.language(r), "dev.delete.conflict"))
	default:
		s.internalError(w, r, "delete device failed", err)
	}
}

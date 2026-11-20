// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/sms"
	"github.com/m4drak/tikker/internal/store"
)

// modem can send everything (UCS-2, several segments).
var modem = connector.Capabilities{
	Encodings:   []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2},
	MaxSegments: 10, Concatenation: true,
}

type chOpt func(*domain.Channel)

func withTags(tags ...string) chOpt { return func(c *domain.Channel) { c.Tags = tags } }
func withTranslit() chOpt           { return func(c *domain.Channel) { c.Transliterate = true } }
func withMaxSeg(n int) chOpt        { return func(c *domain.Channel) { c.MaxSegments = n } }
func offline() chOpt                { return func(c *domain.Channel) { c.Enabled = false } }
func named(n string) chOpt          { return func(c *domain.Channel) { c.Name = n } }

// channel creates an enabled device with one channel (the default capabilities
// are those of RouterOS: GSM-7, one send).
func (e *env) channel(port string, opts ...chOpt) domain.Channel {
	e.t.Helper()
	ctx := e.t.Context()
	e.clk.Advance(time.Second)
	d := domain.Device{
		ID: e.ids().NewID(), Name: "router-" + port, Connector: domain.ConnectorRouterOSREST, BaseURL: "https://10.0.0.1",
		Username: "api", PasswordEnc: []byte("x"), TLSMode: domain.TLSVerify, Enabled: true, CreatedAt: t0,
	}
	if err := e.st.Devices().Create(ctx, d); err != nil {
		e.t.Fatal(err)
	}
	c := domain.Channel{
		ID: e.ids().NewID(), DeviceID: d.ID, Port: port, Name: "ch-" + port, RatePerMin: 6, MaxSegments: 3,
		Enabled: true, State: domain.ChannelOK,
	}
	for _, o := range opts {
		o(&c)
	}
	if err := e.st.Channels().Create(ctx, c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) ids() domain.IDGenerator { return domain.NewULIDGenerator(e.clk) }

// user returns the stored user.
func (e *env) user(name string) domain.User {
	e.t.Helper()
	u, err := e.st.Users().GetByUsername(e.t.Context(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

type msgOpt func(*domain.Message)

func inChannel(c domain.Channel) msgOpt { return func(m *domain.Message) { m.Route.ChannelID = c.ID } }
func withBody(b string) msgOpt          { return func(m *domain.Message) { m.Body = b } }
func byUser(u domain.User) msgOpt {
	return func(m *domain.Message) { m.CreatedBy = domain.UserActor(u.ID) }
}

// message enqueues an outbound message for peer, one second after the last.
func (e *env) message(peer string, opts ...msgOpt) domain.Message {
	e.t.Helper()
	e.clk.Advance(time.Second)
	now := e.clk.Now()
	m := domain.Message{
		ID: e.ids().NewID(), Direction: domain.DirectionOut, Peer: peer, Body: "hello " + peer,
		Encoding: domain.EncodingGSM7, Segments: 1, Status: domain.StatusQueued,
		CreatedBy: domain.TokenActor("01JZZZZZZZZZZZZZZZZZZZZZZZ"), CreatedAt: now, NextAttemptAt: now,
	}
	for _, o := range opts {
		o(&m)
	}
	res, err := e.st.Messages().Enqueue(e.t.Context(), []domain.Message{m}, store.Idempotency{})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Messages[0]
}

// leased takes the message of a channel into sending.
func (e *env) lease(c domain.Channel) domain.Message {
	e.t.Helper()
	m, ok, err := e.st.Messages().AcquireNext(e.t.Context(), store.AcquireRequest{
		ChannelID: c.ID, ChannelTags: c.Tags, Now: e.clk.Now(), LeaseUntil: e.clk.Now().Add(time.Minute),
	})
	if err != nil || !ok {
		e.t.Fatalf("acquire: %v %v", ok, err)
	}
	return m
}

func (e *env) fail(c domain.Channel, reason string) {
	e.t.Helper()
	m := e.lease(c)
	if err := e.st.Messages().MarkFailed(e.t.Context(), m.Lease(), reason, false); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) inbound(c domain.Channel, peer, body string) domain.Message {
	e.t.Helper()
	e.clk.Advance(time.Second)
	m := domain.Message{
		ID: e.ids().NewID(), Direction: domain.DirectionIn, ChannelID: c.ID, Peer: peer, Body: body,
		Encoding: domain.EncodingGSM7, Segments: 1, Status: domain.StatusReceived,
		CreatedAt: e.clk.Now(), ReceivedAt: e.clk.Now(), DedupHash: "h-" + string(c.ID) + body,
	}
	if ok, err := e.st.Messages().InsertInbound(e.t.Context(), m); err != nil || !ok {
		e.t.Fatalf("insert inbound: %v %v", ok, err)
	}
	return m
}

// get fetches a page as the signed-in user in the given language.
func (e *env) get(target string, c *http.Cookie, lang string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodGet, target, nil, withCookies(c), withHeader("Accept-Language", lang))
}

// post sends a form with the session's CSRF token.
func (e *env) post(target string, form url.Values, c *http.Cookie, lang string) *httptest.ResponseRecorder {
	e.t.Helper()
	f := url.Values{"csrf_token": {e.csrfToken(c)}}
	for k, v := range form {
		f[k] = v
	}
	return e.do(http.MethodPost, target, f, withCookies(c), withHeader("Accept-Language", lang))
}

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	body = strings.ReplaceAll(body, "&#43;", "+") // html/template escapes "+"
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s lacks %q", what, w)
		}
	}
}

func mustNotContain(t *testing.T, what, body string, nots ...string) {
	t.Helper()
	for _, w := range nots {
		if strings.Contains(body, w) {
			t.Errorf("%s contains %q", what, w)
		}
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	e := newEnv(t, allRoles)
	base := Options{Auth: e.svc, Store: e.st, Sending: e.srv.send, Queue: e.queue, Health: e.hs}
	for name, mod := range map[string]func(*Options){
		"Auth":    func(o *Options) { o.Auth = nil },
		"Store":   func(o *Options) { o.Store = nil },
		"Sending": func(o *Options) { o.Sending = nil },
		"Queue":   func(o *Options) { o.Queue = nil },
		"Health":  func(o *Options) { o.Health = nil },
	} {
		o := base
		mod(&o)
		if _, err := New(o); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
	if _, err := New(base); err != nil {
		t.Errorf("all given: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Dashboard

func TestDashboard(t *testing.T) {
	e := newEnv(t, allRoles)
	ok := e.channel("lte1", withTags("main", "eu"), named("Main SIM"))
	e.channel("lte2", offline(), named("Spare"))
	bad := e.channel("lte3")
	e.message("+48600100200", inChannel(ok))
	e.message("+48600100201", inChannel(ok))
	e.message("+48600100202", inChannel(bad))
	e.lease(bad) // sending
	e.fail(ok, "modem says no")

	rsrp, sinr := -95, 12
	e.hs.snap = []health.ChannelStatus{{
		ChannelID: ok.ID, State: domain.ChannelOK, Operator: "Orange", Technology: "LTE",
		Signal: &domain.Signal{RSRP: &rsrp, SINR: &sinr}, LastCheckAt: t0,
	}, {
		ChannelID: bad.ID, State: domain.ChannelDown, Reason: "router unreachable",
	}}
	e.queue.stats = queue.Stats{
		Totals:   queue.Counters{Sent: 41, Failed: 3},
		Channels: []queue.ChannelStats{{ChannelID: ok.ID, Counters: queue.Counters{Sent: 40, Failed: 1}}, {ChannelID: bad.ID, Sending: true}},
	}

	viewer := e.login("viewer1")
	en := e.get("/", viewer, "en")
	if en.Code != http.StatusOK {
		t.Fatalf("status %d", en.Code)
	}
	body := en.Body.String()
	mustContain(t, "dashboard", body,
		"Main SIM", "router-lte1 / lte1", `<span class="tag">main</span>`, "Orange", "LTE", "RSRP -95 dBm, SINR 12 dB",
		"Spare", "router unreachable", "modem says no", "Recent errors", "+48600100200",
		`hx-get="/dashboard/status"`, `hx-trigger="every 10s"`,
		`id="stat-queued">1<`, `id="stat-sending">1<`, `id="stat-sent">41<`, `id="stat-failed">3<`,
		"state-ok", "state-down", "state-disabled", "status-sending", "Sent: 40, Failed: 1")
	mustNotContain(t, "dashboard", body, "is coming soon")

	pl := e.get("/", viewer, "pl").Body.String()
	mustContain(t, "pl dashboard", pl, "Ostatnie błędy", "W kolejce", "działa", "niedostępny", "wyłączony", "Kanały")

	// The fragment is what htmx polls: no layout, keeps the polling attributes.
	frag := e.do(http.MethodGet, "/dashboard/status", nil, withCookies(viewer), withHeader("HX-Request", "true"))
	if frag.Code != http.StatusOK {
		t.Fatalf("fragment: %d", frag.Code)
	}
	fb := frag.Body.String()
	mustContain(t, "fragment", fb, `id="dashboard"`, `hx-trigger="every 10s"`, `hx-swap="outerHTML"`, "Main SIM")
	mustNotContain(t, "fragment", fb, "<html", "<head", "<footer")
	if cc := frag.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	// Anonymous users are sent to the login page (htmx: HX-Redirect).
	if w := e.do(http.MethodGet, "/dashboard/status", nil); w.Code != http.StatusSeeOther && w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous fragment: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/dashboard/status", nil, withHeader("HX-Request", "true")); w.Header().Get("HX-Redirect") == "" {
		t.Errorf("anonymous htmx fragment: %d, no HX-Redirect", w.Code)
	}
}

func TestDashboardEmpty(t *testing.T) {
	e := newEnv(t, allRoles)
	body := e.get("/", e.login("viewer1"), "en").Body.String()
	mustContain(t, "empty dashboard", body, "No channels.", "No send errors.", `id="stat-queued">0<`)
}

func TestDashboardStoredStateWithoutLiveHealth(t *testing.T) {
	e := newEnv(t, allRoles)
	e.channel("lte1", func(c *domain.Channel) {
		c.State, c.StateReason, c.Operator = domain.ChannelDegraded, "SIM locked", "Plus"
	})
	body := e.get("/", e.login("viewer1"), "en").Body.String()
	mustContain(t, "stored state", body, "state-degraded", "SIM locked", "Plus")
}

// ---------------------------------------------------------------------------
// Send

var nonceInput = regexp.MustCompile(`name="nonce" value="([0-9a-f]{32})"`)

func formNonce(t *testing.T, page string) string {
	t.Helper()
	m := nonceInput.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no nonce in the form:\n%s", page)
	}
	return m[1]
}

func TestSendRoles(t *testing.T) {
	e := newEnv(t, allRoles)
	e.channel("lte1")
	viewer := e.login("viewer1")
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/send"}, {http.MethodPost, "/send"}, {http.MethodPost, "/send/preview"},
	} {
		var w *httptest.ResponseRecorder
		if tt.method == http.MethodGet {
			w = e.get(tt.path, viewer, "en")
		} else {
			w = e.post(tt.path, url.Values{"to": {"600100200"}, "body": {"x"}}, viewer, "en")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d", tt.method, tt.path, w.Code)
		}
	}
	if n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{}); n != 0 {
		t.Errorf("a viewer sent %d messages", n)
	}
	if e.get("/send", e.login("operator1"), "en").Code != http.StatusOK || e.get("/send", e.login("admin1"), "en").Code != http.StatusOK {
		t.Error("operator and admin may open the form")
	}
	// Nav: viewers do not see the link.
	if strings.Contains(e.get("/", viewer, "en").Body.String(), `href="/send"`) {
		t.Error("viewer sees the send link")
	}
}

func TestSendCSRF(t *testing.T) {
	e := newEnv(t, allRoles)
	e.channel("lte1")
	op := e.login("operator1")
	form := url.Values{"to": {"600100200"}, "body": {"x"}}
	if w := e.do(http.MethodPost, "/send", form, withCookies(op)); w.Code != http.StatusForbidden {
		t.Errorf("without token: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/send", form, withCookies(op), withHeader("X-CSRF-Token", "wrong")); w.Code != http.StatusForbidden {
		t.Errorf("wrong token: %d", w.Code)
	}
	if n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{}); n != 0 {
		t.Errorf("%d messages stored without CSRF token", n)
	}
	// The header works too (htmx).
	if w := e.do(http.MethodPost, "/send/preview", form, withCookies(op), withHeader("X-CSRF-Token", e.csrfToken(op))); w.Code != http.StatusOK {
		t.Errorf("preview with header: %d", w.Code)
	}
}

func TestSendForm(t *testing.T) {
	e := newEnv(t, allRoles)
	e.channel("lte1", named("Main"))
	e.channel("lte2", offline(), named("Spare"))
	op := e.login("operator1")

	body := e.get("/send", op, "en").Body.String()
	mustContain(t, "send form", body,
		`<form method="post" action="/send"`, `hx-post="/send/preview"`, `hx-trigger="input changed delay:300ms"`, `hx-target="#send-preview"`,
		`name="to"`, `name="body"`, `name="channel"`, `name="tags"`, `name="csrf_token"`, `maxlength="1600"`,
		"Any enabled channel", "Main (lte1)", `id="send-preview"`, `id="pv-chars">0<`)
	mustNotContain(t, "send form", body, "Spare", "no-channels")
	if formNonce(t, body) == formNonce(t, e.get("/send", op, "en").Body.String()) {
		t.Error("the nonce does not change between requests")
	}
	mustContain(t, "pl form", e.get("/send", op, "pl").Body.String(), "Numer odbiorcy", "Dowolny aktywny kanał", "Kodowanie")

	// No usable channel: a warning, and the form still shows.
	e2 := newEnv(t, allRoles)
	mustContain(t, "no channel", e2.get("/send", e2.login("operator1"), "en").Body.String(), `id="no-channels"`, "There is no enabled channel")
}

func TestSendSubmit(t *testing.T) {
	e := newEnv(t, allRoles)
	ch := e.channel("lte1", withTags("main"))
	op := e.login("operator1")
	nonce := formNonce(t, e.get("/send", op, "en").Body.String())

	form := url.Values{"to": {"600 100 200"}, "body": {"Hello\r\nworld"}, "nonce": {nonce}, "channel": {string(ch.ID)}, "tags": {"Main, "}}
	w := e.post("/send", form, op, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d\n%s", w.Code, w.Body)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/messages/") || !strings.HasSuffix(loc, "?notice=queued") {
		t.Fatalf("Location = %q", loc)
	}
	page, err := e.st.Messages().List(e.t.Context(), store.MessageFilter{}, store.PageRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("%v, %d messages", err, len(page.Items))
	}
	m := page.Items[0]
	if m.Peer != "+48600100200" || m.Body != "Hello\nworld" || m.Route.ChannelID != ch.ID || len(m.Route.Tags) != 1 || m.Route.Tags[0] != "main" ||
		m.CreatedBy != domain.UserActor(e.user("operator1").ID) || m.Status != domain.StatusQueued {
		t.Errorf("message = %+v", m)
	}
	if loc != "/messages/"+string(m.ID)+"?notice=queued" {
		t.Errorf("Location = %q", loc)
	}
	if e.wake.n != 1 {
		t.Errorf("queue woken %d times", e.wake.n)
	}

	// The notice on the details page.
	mustContain(t, "details", e.get(loc, op, "en").Body.String(), "The message is queued for sending.", "Hello", "operator1")

	// Audit (SPEC §7): who sent, no number and no text.
	log, err := e.st.Audit().List(e.t.Context(), store.AuditFilter{Action: domain.AuditMessageSend}, store.PageRequest{})
	if err != nil || len(log.Items) != 1 {
		t.Fatalf("audit: %v %+v", err, log.Items)
	}
	a := log.Items[0]
	if a.Actor != m.CreatedBy || a.Target != "message:"+string(m.ID) || a.Meta["source"] != "ui" {
		t.Errorf("audit = %+v", a)
	}

	// Submitting the same form again (double click, reload): the same message.
	w = e.post("/send", form, op, "en")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != loc {
		t.Errorf("repeat: %d %q", w.Code, w.Header().Get("Location"))
	}
	if n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{}); n != 1 || e.wake.n != 1 {
		t.Errorf("%d messages, woken %d times", n, e.wake.n)
	}
	// The nonce reused with other text (back button): a new message.
	form.Set("body", "another text")
	w = e.post("/send", form, op, "en")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") == loc {
		t.Errorf("edited resubmit: %d %q", w.Code, w.Header().Get("Location"))
	}
	if n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{}); n != 2 {
		t.Errorf("%d messages", n)
	}
	// A form without a (valid) nonce still sends, every time.
	form.Set("nonce", "garbage")
	e.post("/send", form, op, "en")
	e.post("/send", form, op, "en")
	if n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{}); n != 4 {
		t.Errorf("%d messages", n)
	}
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t, allRoles)
	e.channel("lte1", withTags("main"), withMaxSeg(1))
	e.channel("lte2", offline(), withTags("off"))
	op := e.login("operator1")

	tests := []struct {
		name string
		form url.Values
		code int
		en   []string
		pl   []string
	}{
		{"invalid number", url.Values{"to": {"12"}, "body": {"x"}}, 422,
			[]string{"This is not a valid phone number.", `id="to-error"`, `aria-invalid="true"`},
			[]string{"To nie jest prawidłowy numer telefonu."}},
		{"missing number", url.Values{"body": {"x"}}, 422, []string{"Enter the recipient number."}, []string{"Podaj numer odbiorcy."}},
		{"missing text", url.Values{"to": {"600100200"}}, 422, []string{"Enter the message text.", `id="body-error"`}, []string{"Wpisz treść wiadomości."}},
		{"text too long", url.Values{"to": {"600100200"}, "body": {strings.Repeat("a", 1601)}}, 422, []string{"at most 1600 characters"}, []string{"najwyżej 1600 znaków"}},
		{"Polish text, GSM-7 channel", url.Values{"to": {"600100200"}, "body": {"Zażółć gęślą jaźń"}}, 422,
			[]string{"outside the GSM-7 alphabet", `id="body-error"`}, []string{"spoza alfabetu GSM-7"}},
		{"over the segment limit", url.Values{"to": {"600100200"}, "body": {strings.Repeat("a", 200)}}, 422,
			[]string{"segment limit. Shorten it"}, []string{"przekracza limit segmentów"}},
		{"unknown channel", url.Values{"to": {"600100200"}, "body": {"x"}, "channel": {"nope"}}, 422,
			[]string{"This channel does not exist.", `id="channel-error"`}, []string{"Taki kanał nie istnieje."}},
		{"disabled channel", url.Values{"to": {"600100200"}, "body": {"x"}, "channel": {"01JZZZZZZZZZZZZZZZZZZZZZZZ"}}, 422,
			[]string{"This channel does not exist."}, nil},
		{"no channel with the tags", url.Values{"to": {"600100200"}, "body": {"x"}, "tags": {"vip"}}, 422,
			[]string{"No enabled channel carries all the given tags.", `id="tags-error"`}, []string{"Żaden aktywny kanał nie ma wszystkich podanych tagów."}},
		{"bad tag", url.Values{"to": {"600100200"}, "body": {"x"}, "tags": {"ok bad!"}}, 422,
			[]string{"A tag consists of lower-case letters"}, []string{"Tag może zawierać małe litery"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.post("/send", tt.form, op, "en")
			if w.Code != tt.code {
				t.Fatalf("status %d\n%s", w.Code, w.Body)
			}
			mustContain(t, "en", w.Body.String(), tt.en...)
			// The form keeps what was typed and carries a nonce.
			if v := tt.form.Get("to"); v != "" {
				mustContain(t, "form", w.Body.String(), `value="`+v+`"`)
			}
			formNonce(t, w.Body.String())
			if tt.pl != nil {
				mustContain(t, "pl", e.post("/send", tt.form, op, "pl").Body.String(), tt.pl...)
			}
		})
	}
	if n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{}); n != 0 {
		t.Errorf("invalid requests stored %d messages", n)
	}
	if e.wake.n != 0 {
		t.Errorf("queue woken %d times", e.wake.n)
	}
}

func TestSendValidationDedupesPreviewProblems(t *testing.T) {
	e := newEnv(t, allRoles)
	e.channel("lte1")
	body := e.post("/send", url.Values{"to": {"600100200"}, "body": {"Zażółć"}}, e.login("operator1"), "en").Body.String()
	if n := strings.Count(body, "outside the GSM-7 alphabet"); n != 1 {
		t.Errorf("the same problem is shown %d times", n)
	}
}

func TestSendSplit(t *testing.T) {
	e := newEnvSending(t, sending.Options{DefaultRegion: "PL", LongMessageStrategy: sms.StrategySplit}, allRoles)
	e.channel("lte1", withMaxSeg(5))
	op := e.login("operator1")
	text := strings.Repeat("word ", 50)

	pv := e.post("/send/preview", url.Values{"body": {text}}, op, "en").Body.String()
	mustContain(t, "split preview", pv, `id="pv-split"`, "separate messages")

	w := e.post("/send", url.Values{"to": {"600100200"}, "body": {text}}, op, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d\n%s", w.Code, w.Body)
	}
	n, _ := e.st.Messages().Count(e.t.Context(), store.MessageFilter{})
	if n < 2 {
		t.Fatalf("%d messages", n)
	}
	want := "/messages?direction=out&notice=queued_n&n=" + strconv.Itoa(n)
	if w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	list := e.get(w.Header().Get("Location"), op, "en").Body.String()
	mustContain(t, "list", list, "The text was split into "+strconv.Itoa(n)+" messages", "(1/"+strconv.Itoa(n)+")")
	mustContain(t, "pl list", e.get(w.Header().Get("Location"), op, "pl").Body.String(), "Tekst został podzielony na "+strconv.Itoa(n)+" wiadomości")
}

func TestSendPreview(t *testing.T) {
	e := newEnv(t, allRoles)
	plain := e.channel("lte1", withTags("plain"))
	e.channel("lte2", withTags("translit"), withTranslit())
	ucs := e.channel("lte3", withTags("ucs"), withMaxSeg(5))
	e.caps.per = map[domain.ID]connector.Capabilities{ucs.ID: modem}
	op := e.login("operator1")

	preview := func(form url.Values, lang string) string {
		t.Helper()
		f := url.Values{"csrf_token": {e.csrfToken(op)}}
		for k, v := range form {
			f[k] = v
		}
		w := e.do(http.MethodPost, "/send/preview", f, withCookies(op), withHeader("HX-Request", "true"), withHeader("Accept-Language", lang))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d\n%s", w.Code, w.Body)
		}
		b := w.Body.String()
		mustNotContain(t, "fragment", b, "<html", "<head", "<form")
		return b
	}

	t.Run("plain text", func(t *testing.T) {
		b := preview(url.Values{"body": {"Hello world"}, "channel": {string(plain.ID)}}, "en")
		mustContain(t, "preview", b, `id="pv-chars">11<`, `id="pv-encoding">GSM-7<`, `id="pv-segments">1<`, `id="pv-remaining">149<`)
		mustNotContain(t, "preview", b, "field-error", "pv-translit", "pv-split")
	})
	t.Run("Polish text without transliteration", func(t *testing.T) {
		b := preview(url.Values{"body": {"Zażółć gęślą jaźń"}, "tags": {"plain"}}, "en")
		mustContain(t, "preview", b, "outside the GSM-7 alphabet", `id="pv-nongsm"`, "ż", `role="alert"`)
		mustContain(t, "pl preview", preview(url.Values{"body": {"Zażółć gęślą jaźń"}, "tags": {"plain"}}, "pl"), "spoza alfabetu GSM-7")
	})
	t.Run("Polish text with transliteration", func(t *testing.T) {
		b := preview(url.Values{"body": {"Zażółć gęślą jaźń"}, "tags": {"translit"}}, "en")
		mustContain(t, "preview", b, `id="pv-chars">17<`, `id="pv-encoding">GSM-7<`, `id="pv-segments">1<`, `id="pv-translit"`, "The text will be changed before sending.")
		mustNotContain(t, "preview", b, "field-error", "pv-nongsm")
		mustContain(t, "pl preview", preview(url.Values{"body": {"Zażółć"}, "tags": {"translit"}}, "pl"), "transliteracja", "Tekst zostanie zmieniony")
	})
	t.Run("characters that cannot be converted", func(t *testing.T) {
		b := preview(url.Values{"body": {"ok 中"}, "tags": {"translit"}}, "en")
		mustContain(t, "preview", b, `id="pv-substituted"`, "中")
	})
	t.Run("UCS-2 channel counts 70 characters per segment", func(t *testing.T) {
		b := preview(url.Values{"body": {"Zażółć gęślą jaźń " + strings.Repeat("a", 60)}, "channel": {string(ucs.ID)}}, "en")
		mustContain(t, "preview", b, `id="pv-encoding">UCS-2<`, `id="pv-segments">2<`, "channel limit: 5")
		mustNotContain(t, "preview", b, "field-error")
	})
	t.Run("over the segment limit", func(t *testing.T) {
		b := preview(url.Values{"body": {strings.Repeat("a", 161)}, "channel": {string(plain.ID)}}, "en")
		mustContain(t, "preview", b, "segment limit. Shorten it", "field-error")
	})
	t.Run("channel problems", func(t *testing.T) {
		mustContain(t, "unknown channel", preview(url.Values{"body": {"x"}, "channel": {"nope"}}, "en"), "This channel does not exist.")
		mustContain(t, "no tags", preview(url.Values{"body": {"x"}, "tags": {"zzz"}}, "en"), "No enabled channel carries all the given tags.")
		mustContain(t, "bad tags", preview(url.Values{"body": {"x"}, "tags": {"Bad!"}}, "en"), "A tag consists of")
	})
	t.Run("line breaks count once", func(t *testing.T) {
		b := preview(url.Values{"body": {"a\r\nb"}, "channel": {string(plain.ID)}}, "en")
		mustContain(t, "preview", b, `id="pv-chars">3<`)
	})
	t.Run("empty", func(t *testing.T) {
		mustContain(t, "preview", preview(url.Values{}, "en"), `id="pv-chars">0<`, `id="pv-segments">0<`)
	})
}

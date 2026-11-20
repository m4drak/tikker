// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

var rowLink = regexp.MustCompile(`<td class="nowrap"><a href="/messages/([0-9A-Z]{26})">`)

// listed returns the IDs of the rows of a list page, newest first.
func listed(page string) []domain.ID {
	var ids []domain.ID
	for _, m := range rowLink.FindAllStringSubmatch(page, -1) {
		ids = append(ids, domain.ID(m[1]))
	}
	return ids
}

func TestMessagesFilters(t *testing.T) {
	e := newEnv(t, allRoles)
	a := e.channel("lte1", named("Alpha"))
	b := e.channel("lte2", named("Beta"))
	m1 := e.message("+48600100200", inChannel(a), withBody("first message"))
	m2 := e.message("+48600100201", inChannel(b), withBody("second\nmessage"))
	m3 := e.message("+48600100202", inChannel(a))
	if _, err := e.st.Messages().Cancel(e.t.Context(), m2.ID); err != nil {
		t.Fatal(err)
	}
	in := e.inbound(a, "+48511222333", "incoming text")
	// A message that is retried after an ambiguous failure.
	m5 := e.message("+48600100203", inChannel(b))
	if err := e.st.Messages().Requeue(e.t.Context(), e.lease2(b, m5), store.RequeueParams{NextAttemptAt: e.clk.Now(), LastError: "timeout", PossibleDuplicate: true}); err != nil {
		t.Fatal(err)
	}

	viewer := e.login("viewer1")
	get := func(target string) string {
		t.Helper()
		w := e.get(target, viewer, "en")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", target, w.Code)
		}
		return w.Body.String()
	}
	ids := func(target string) []domain.ID { return listed(get(target)) }
	same := func(got []domain.ID, want ...domain.ID) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	all := get("/messages")
	if got := listed(all); !same(got, m5.ID, in.ID, m3.ID, m2.ID, m1.ID) {
		t.Errorf("all = %v", got)
	}
	// The full number is shown to signed-in users; bodies are shortened to one line.
	mustContain(t, "list", all, "+48600100200", "+48511222333", "first message", "second message", "Matching messages: 5",
		"status-cancelled", "status-queued", "status-received", "possible duplicate", "Alpha", "Beta", "outbound", "inbound")
	mustNotContain(t, "list", all, "Clear filters")

	for _, tt := range []struct {
		name  string
		query string
		want  []domain.ID
	}{
		{"direction in", "direction=in", []domain.ID{in.ID}},
		{"direction out", "direction=out", []domain.ID{m5.ID, m3.ID, m2.ID, m1.ID}},
		{"status", "status=cancelled", []domain.ID{m2.ID}},
		{"channel", "channel=" + string(a.ID), []domain.ID{in.ID, m3.ID, m1.ID}},
		{"peer as typed", "peer=" + url.QueryEscape("600 100 201"), []domain.ID{m2.ID}},
		{"peer international", "peer=" + url.QueryEscape("+48511222333"), []domain.ID{in.ID}},
		{"combined", "direction=out&status=queued&channel=" + string(a.ID), []domain.ID{m3.ID, m1.ID}},
		{"nothing matches", "peer=999", nil},
		{"invalid values are ignored", "direction=sideways&status=lost&channel=zz&since=yesterday&until=&cursor=junk", []domain.ID{m5.ID, in.ID, m3.ID, m2.ID, m1.ID}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ids("/messages?" + tt.query); !same(got, tt.want...) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	// Date range: the fake clock is at 2026-03-04 10:00 UTC plus seconds.
	day := t0.Format(time.DateOnly)
	next := t0.AddDate(0, 0, 1).Format(time.DateOnly)
	if got := ids("/messages?since=" + day + "&until=" + day); len(got) != 5 {
		t.Errorf("the whole last day is included: %v", got)
	}
	if got := ids("/messages?since=" + next); len(got) != 0 {
		t.Errorf("since tomorrow: %v", got)
	}
	if got := ids("/messages?until=" + t0.AddDate(0, 0, -1).Format(time.DateOnly)); len(got) != 0 {
		t.Errorf("until yesterday: %v", got)
	}

	// The form keeps the filter and offers to clear it.
	f := get("/messages?direction=out&status=queued&peer=600&since=" + day)
	mustContain(t, "filter form", f, `<option value="out" selected>`, `<option value="queued" selected>`, `value="600"`, `value="`+day+`"`, "Clear filters")
	mustContain(t, "pl", e.get("/messages", viewer, "pl").Body.String(), "Pasujących wiadomości: 5", "Filtruj", "wychodząca", "możliwy duplikat")
	empty := get("/messages?peer=999")
	mustContain(t, "empty", empty, "No messages match these filters.")
	mustNotContain(t, "empty", empty, `id="messages"`)
}

// lease2 acquires m for the channel and returns its lease.
func (e *env) lease2(c domain.Channel, m domain.Message) domain.Lease {
	e.t.Helper()
	got := e.lease(c)
	if got.ID != m.ID {
		e.t.Fatalf("acquired %s, want %s", got.ID, m.ID)
	}
	return got.Lease()
}

func TestMessagesPagination(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.channel("lte1")
	const total = 60
	var created []domain.ID
	for i := 0; i < total; i++ {
		created = append(created, e.message("+48600100200", inChannel(c)).ID)
	}
	viewer := e.login("viewer1")

	first := e.get("/messages", viewer, "en").Body.String()
	got := listed(first)
	if len(got) != messagesPerPage || got[0] != created[total-1] {
		t.Fatalf("first page: %d rows, first %v", len(got), got[0])
	}
	mustContain(t, "first page", first, "Matching messages: 60", `rel="next"`, "Older")
	mustNotContain(t, "first page", first, "First page")

	seen := map[domain.ID]bool{}
	target := "/messages"
	pages := 0
	for {
		page := e.get(target, viewer, "en").Body.String()
		t.Logf("%s -> %d rows", target, len(listed(page)))
		for _, id := range listed(page) {
			if seen[id] {
				t.Fatalf("%s listed twice", id)
			}
			seen[id] = true
		}
		pages++
		m := regexp.MustCompile(`href="(/messages\?[^"]+)" rel="next"`).FindStringSubmatch(page)
		if m == nil {
			break
		}
		target = strings.ReplaceAll(m[1], "&amp;", "&")
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
	}
	if pages != 3 || len(seen) != total {
		t.Errorf("%d pages, %d messages seen", pages, len(seen))
	}

	// The filter survives paging, and later pages link back to the first.
	f := e.channel("lte2")
	for i := 0; i < 30; i++ {
		e.message("+48600100201", inChannel(f))
	}
	p1 := e.get("/messages?channel="+string(f.ID), viewer, "en").Body.String()
	m := regexp.MustCompile(`href="(/messages\?[^"]+)" rel="next"`).FindStringSubmatch(p1)
	if m == nil || !strings.Contains(m[1], "channel="+string(f.ID)) || !strings.Contains(m[1], "cursor=") {
		t.Fatalf("next link = %v", m)
	}
	p2 := e.get(strings.ReplaceAll(m[1], "&amp;", "&"), viewer, "en").Body.String()
	if n := len(listed(p2)); n != 5 {
		t.Errorf("second page has %d rows", n)
	}
	mustContain(t, "second page", p2, "First page", "channel="+string(f.ID))
	mustNotContain(t, "second page", p2, `rel="next"`)
}

func TestMessagesNotices(t *testing.T) {
	e := newEnv(t, allRoles)
	viewer := e.login("viewer1")
	for _, tt := range []struct{ query, want string }{
		{"notice=queued", "The message is queued for sending."},
		{"notice=queued_n&n=3", "The text was split into 3 messages"},
		{"notice=cancelled", "Message cancelled."},
	} {
		mustContain(t, tt.query, e.get("/messages?"+tt.query, viewer, "en").Body.String(), tt.want)
	}
	for _, q := range []string{"notice=queued_n&n=1", "notice=queued_n&n=x", "notice=queued_n&n=100000", "notice=<script>", "notice=evil"} {
		b := e.get("/messages?"+q, viewer, "en").Body.String()
		mustNotContain(t, q, b, `role="status"`, "<script>")
	}
}

func TestMessageDetails(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.channel("lte1", named("Alpha"), withTags("main"))
	author := e.user("operator1")
	exp := e.clk.Now().Add(time.Hour)
	m := e.message("+48600100200", inChannel(c), byUser(author), withBody("Dear <b>customer</b>,\nyour code is 1234"), func(m *domain.Message) {
		m.ClientRef = "order-17"
		m.ExpiresAt = exp
		m.Route.Tags = []string{"main"}
	})
	viewer, op := e.login("viewer1"), e.login("operator1")

	b := e.get("/messages/"+string(m.ID), viewer, "en").Body.String()
	mustContain(t, "details", b, string(m.ID), "+48600100200", "Alpha", "operator1", "order-17", "main", "GSM-7", "Send attempts",
		"Dear &lt;b&gt;customer&lt;/b&gt;,\nyour code is 1234", "status-queued", ts(m.CreatedAt), ts(exp), "Next attempt")
	mustNotContain(t, "details", b, "<b>customer</b>", "possible duplicate")
	// Viewers cannot cancel: no button.
	mustNotContain(t, "viewer details", b, "/cancel")
	mustContain(t, "operator details", e.get("/messages/"+string(m.ID), op, "en").Body.String(), `action="/messages/`+string(m.ID)+`/cancel"`, "Cancel message")
	mustContain(t, "pl details", e.get("/messages/"+string(m.ID), op, "pl").Body.String(), "Anuluj wiadomość", "Utworzył", "w kolejce")

	// Inbound, with a token author, a missing channel and a purged body.
	in := e.inbound(c, "+48511222333", "hi")
	ib := e.get("/messages/"+string(in.ID), op, "en").Body.String()
	mustContain(t, "inbound", ib, "inbound", "Received", "status-received")
	mustNotContain(t, "inbound", ib, "/cancel", "Created by")

	tok := e.message("+48600100210")
	mustContain(t, "token author", e.get("/messages/"+string(tok.ID), op, "en").Body.String(), "token:01JZZZZZZZZZZZZZZZZZZZZZZZ")
	unknownUser := e.message("+48600100211", func(m *domain.Message) { m.CreatedBy = domain.UserActor("01JYYYYYYYYYYYYYYYYYYYYYYY") })
	mustContain(t, "deleted author", e.get("/messages/"+string(unknownUser.ID), op, "en").Body.String(), "user:01JYYYYYYYYYYYYYYYYYYYYYYY")

	purged := e.message("+48600100212", inChannel(c))
	if _, err := e.st.Messages().Cancel(e.t.Context(), purged.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.st.Messages().PurgeBodies(e.t.Context(), e.clk.Now().Add(time.Hour)); err != nil || n == 0 {
		t.Fatalf("purge: %d %v", n, err)
	}
	mustContain(t, "purged", e.get("/messages/"+string(purged.ID), op, "en").Body.String(), "removed by the retention policy")

	// Unknown and malformed IDs.
	for _, id := range []string{"01JXXXXXXXXXXXXXXXXXXXXXXX", "not-an-id", "01jzzzzzzzzzzzzzzzzzzzzzzz"} {
		if w := e.get("/messages/"+id, viewer, "en"); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", id, w.Code)
		}
	}
}

func ts(t time.Time) string { return formatTime(t) }

func TestMessageDetailsPossibleDuplicate(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.channel("lte1")
	d := e.message("+48600100213", inChannel(c))
	if err := e.st.Messages().Requeue(e.t.Context(), e.lease2(c, d), store.RequeueParams{NextAttemptAt: e.clk.Now(), LastError: "timeout while sending", PossibleDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	db := e.get("/messages/"+string(d.ID), e.login("operator1"), "en").Body.String()
	mustContain(t, "duplicate", db, "possible duplicate", "may get this message twice", "timeout while sending", "status-duplicate")
}

func TestMessageCancel(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.channel("lte1")
	viewer, op := e.login("viewer1"), e.login("operator1")

	m := e.message("+48600100200", inChannel(c))
	target := "/messages/" + string(m.ID) + "/cancel"

	// Viewer: 403; no CSRF token: 403; nothing changes.
	if w := e.post(target, nil, viewer, "en"); w.Code != http.StatusForbidden {
		t.Errorf("viewer: %d", w.Code)
	}
	if w := e.do(http.MethodPost, target, url.Values{}, withCookies(op)); w.Code != http.StatusForbidden {
		t.Errorf("no token: %d", w.Code)
	}
	if got, _ := e.st.Messages().Get(e.t.Context(), m.ID); got.Status != domain.StatusQueued {
		t.Fatalf("status = %s", got.Status)
	}
	// GET is not a cancel.
	if w := e.get(target, op, "en"); w.Code == http.StatusOK || w.Code == http.StatusSeeOther {
		t.Errorf("GET cancel: %d", w.Code)
	}

	w := e.post(target, nil, op, "en")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/messages/"+string(m.ID)+"?notice=cancelled" {
		t.Fatalf("cancel: %d %q", w.Code, w.Header().Get("Location"))
	}
	got, _ := e.st.Messages().Get(e.t.Context(), m.ID)
	if got.Status != domain.StatusCancelled {
		t.Errorf("status = %s", got.Status)
	}
	page := e.get(w.Header().Get("Location"), op, "en").Body.String()
	mustContain(t, "after cancel", page, "Message cancelled.", "status-cancelled")
	mustNotContain(t, "after cancel", page, "Cancel message")

	log, err := e.st.Audit().List(e.t.Context(), store.AuditFilter{Action: domain.AuditMessageCancel}, store.PageRequest{})
	if err != nil || len(log.Items) != 1 || log.Items[0].Actor != domain.UserActor(e.user("operator1").ID) || log.Items[0].Target != "message:"+string(m.ID) {
		t.Errorf("audit: %v %+v", err, log.Items)
	}

	// Cancelling again: the message is no longer queued.
	w = e.post(target, nil, op, "en")
	if w.Code != http.StatusConflict {
		t.Fatalf("second cancel: %d", w.Code)
	}
	mustContain(t, "conflict", w.Body.String(), "Cannot cancel: the message is no longer queued.", "status-cancelled")
	mustContain(t, "pl conflict", e.post(target, nil, op, "pl").Body.String(), "Nie można anulować")

	// A message that is being sent cannot be cancelled either.
	s := e.message("+48600100201", inChannel(c))
	e.lease(c)
	if w := e.post("/messages/"+string(s.ID)+"/cancel", nil, op, "en"); w.Code != http.StatusConflict {
		t.Errorf("sending: %d", w.Code)
	}
	if got, _ := e.st.Messages().Get(e.t.Context(), s.ID); got.Status != domain.StatusSending {
		t.Errorf("status = %s", got.Status)
	}

	// Unknown and malformed IDs.
	for _, id := range []string{"01JXXXXXXXXXXXXXXXXXXXXXXX", "garbage"} {
		if w := e.post("/messages/"+id+"/cancel", nil, op, "en"); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", id, w.Code)
		}
	}
}

func TestPagesHaveNoInlineCode(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.channel("lte1", named(`<script>alert(1)</script>`))
	m := e.message("+48600100200", inChannel(c), withBody(`<img src=x onerror=alert(1)>`))
	e.fail(c, `"><script>x</script>`)
	op := e.login("operator1")
	for _, lang := range []string{"pl", "en"} {
		for _, target := range []string{
			"/", "/dashboard/status", "/send", "/messages", "/messages/" + string(m.ID),
			"/messages?notice=queued_n&n=2", "/messages?peer=%3Cscript%3E",
		} {
			b := e.get(target, op, lang).Body.String()
			if found := inlineCode(b); len(found) != 0 {
				t.Errorf("%s %s: inline code %v", lang, target, found)
			}
			mustNotContain(t, target, b, "<script>alert", "<img src=x", `"><script>`)
		}
		// POST results too (send form with errors, preview).
		for _, target := range []string{"/send", "/send/preview"} {
			b := e.post(target, url.Values{"to": {"<script>"}, "body": {"<script>x</script>"}, "tags": {"<b>"}}, op, lang).Body.String()
			if found := inlineCode(b); len(found) != 0 {
				t.Errorf("%s POST %s: inline code %v", lang, target, found)
			}
			mustNotContain(t, target, b, "<script>x", "<b>")
		}
	}
}

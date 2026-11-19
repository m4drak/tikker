// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package storetest is a contract test suite for [store.Store]
// implementations. Each implementation (SQLite today, PostgreSQL later) calls
// [Run] from its own tests with a factory that returns a fresh, empty store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Factory returns a fresh, empty store and registers its cleanup with t.
type Factory func(t *testing.T) store.Store

// T0 is the base time used by the suite (whole milliseconds, UTC).
var T0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// Run executes the whole contract suite against stores made by f.
func Run(t *testing.T, f Factory) {
	t.Helper()
	groups := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"Devices", testDevices},
		{"Channels", testChannels},
		{"Users", testUsers},
		{"Tokens", testTokens},
		{"Audit", testAudit},
		{"Enqueue", testEnqueue},
		{"Idempotency", testIdempotency},
		{"Acquire", testAcquire},
		{"Routing", testRouting},
		{"Lease", testLease},
		{"Cancel", testCancel},
		{"Recover", testRecover},
		{"Expire", testExpire},
		{"ListCount", testListCount},
		{"Inbound", testInbound},
		{"InboundParts", testInboundParts},
		{"Retention", testRetention},
		{"WithTx", testWithTx},
		{"Concurrency", testConcurrency},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) { g.fn(t, f) })
	}
}

// env bundles a store with an ID generator whose time advances per ID, so
// that IDs sort in creation order.
type env struct {
	t   *testing.T
	s   store.Store
	ctx context.Context
	clk *clock.Fake
	ids *domain.ULIDGenerator
}

func newEnv(t *testing.T, f Factory) *env {
	t.Helper()
	clk := clock.NewFake(T0)
	return &env{t: t, s: f(t), ctx: context.Background(), clk: clk, ids: domain.NewULIDGenerator(clk)}
}

// id returns the next ID; each call is one millisecond later than the last.
func (e *env) id() domain.ID {
	e.clk.Advance(time.Millisecond)
	return e.ids.NewID()
}

func (e *env) ok(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
}

func (e *env) is(err, want error) {
	e.t.Helper()
	if !errors.Is(err, want) {
		e.t.Fatalf("error = %v, want %v", err, want)
	}
}

func (e *env) device(name string) domain.Device {
	e.t.Helper()
	d := domain.Device{
		ID: e.id(), Name: name, Connector: domain.ConnectorRouterOSREST, BaseURL: "https://10.0.0.1",
		Username: "api", PasswordEnc: []byte("secret-" + name), TLSMode: domain.TLSVerify,
		Enabled: true, CreatedAt: T0,
	}
	e.ok(e.s.Devices().Create(e.ctx, d))
	return d
}

func (e *env) channel(dev domain.Device, port string, tags ...string) domain.Channel {
	e.t.Helper()
	c := domain.Channel{
		ID: e.id(), DeviceID: dev.ID, Port: port, Name: dev.Name + "/" + port, Tags: tags,
		RatePerMin: 6, MaxSegments: 3, Enabled: true, State: domain.ChannelOK,
	}
	e.ok(e.s.Channels().Create(e.ctx, c))
	return c
}

func (e *env) msg(peer string) domain.Message {
	return domain.Message{
		ID: e.id(), Peer: peer, Body: "hello " + peer, Encoding: domain.EncodingGSM7, Segments: 1,
		CreatedBy: domain.TokenActor(tokenOwner), CreatedAt: T0, NextAttemptAt: T0,
	}
}

// tokenOwner is a fixed, valid ULID used as the creating token in messages.
const tokenOwner domain.ID = "01JZZZZZZZZZZZZZZZZZZZZZZZ"

func (e *env) enqueue(ms ...domain.Message) []domain.Message {
	e.t.Helper()
	res, err := e.s.Messages().Enqueue(e.ctx, ms, store.Idempotency{})
	e.ok(err)
	return res.Messages
}

func (e *env) acquire(ch domain.ID, tags []string, now time.Time) (domain.Message, bool) {
	e.t.Helper()
	m, ok, err := e.s.Messages().AcquireNext(e.ctx, store.AcquireRequest{
		ChannelID: ch, ChannelTags: tags, Now: now, LeaseUntil: now.Add(time.Minute),
	})
	e.ok(err)
	return m, ok
}

// ---------------------------------------------------------------------------

func testDevices(t *testing.T, f Factory) {
	e := newEnv(t, f)
	d := e.device("alpha")

	got, err := e.s.Devices().Get(e.ctx, d.ID)
	e.ok(err)
	if got.Name != "alpha" || string(got.PasswordEnc) != "secret-alpha" || !got.Enabled || !got.CreatedAt.Equal(T0) ||
		got.TLSMode != domain.TLSVerify || got.Connector != domain.ConnectorRouterOSREST {
		t.Fatalf("Get = %+v", got)
	}
	_, err = e.s.Devices().Get(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)

	dup := d
	dup.ID = e.id()
	e.is(e.s.Devices().Create(e.ctx, dup), domain.ErrConflict)
	same := d
	same.Name = "other"
	e.is(e.s.Devices().Create(e.ctx, same), domain.ErrConflict) // duplicate ID

	b := e.device("bravo")
	list, err := e.s.Devices().List(e.ctx)
	e.ok(err)
	if len(list) != 2 || list[0].ID != d.ID || list[1].ID != b.ID {
		t.Fatalf("List = %+v", list)
	}

	// Update keeps the password when PasswordEnc is nil, replaces it otherwise.
	upd := d
	upd.PasswordEnc = nil
	upd.Name = "alpha2"
	upd.Enabled = false
	upd.TLSMode = domain.TLSInsecure
	e.ok(e.s.Devices().Update(e.ctx, upd))
	got, _ = e.s.Devices().Get(e.ctx, d.ID)
	if got.Name != "alpha2" || got.Enabled || string(got.PasswordEnc) != "secret-alpha" || got.TLSMode != domain.TLSInsecure {
		t.Fatalf("after Update = %+v", got)
	}
	upd.PasswordEnc = []byte("new")
	e.ok(e.s.Devices().Update(e.ctx, upd))
	got, _ = e.s.Devices().Get(e.ctx, d.ID)
	if string(got.PasswordEnc) != "new" {
		t.Fatalf("password = %q", got.PasswordEnc)
	}
	upd.Name = "bravo"
	e.is(e.s.Devices().Update(e.ctx, upd), domain.ErrConflict)
	missing := upd
	missing.ID = e.id()
	missing.Name = "zzz"
	e.is(e.s.Devices().Update(e.ctx, missing), domain.ErrNotFound)

	// Delete removes channels; conflicts while messages are active.
	c := e.channel(b, "lte1")
	m := e.msg("+48600000001")
	m.Route.ChannelID = c.ID
	e.enqueue(m)
	e.is(e.s.Devices().Delete(e.ctx, b.ID), domain.ErrConflict)
	if _, err := e.s.Channels().Get(e.ctx, c.ID); err != nil {
		t.Fatalf("channel must survive failed delete: %v", err)
	}
	if _, err := e.s.Messages().Cancel(e.ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	e.ok(e.s.Devices().Delete(e.ctx, b.ID))
	_, err = e.s.Channels().Get(e.ctx, c.ID)
	e.is(err, domain.ErrNotFound)
	kept, err := e.s.Messages().Get(e.ctx, m.ID)
	e.ok(err)
	if kept.Route.ChannelID != c.ID {
		t.Fatalf("message lost its channel reference: %+v", kept)
	}
	e.is(e.s.Devices().Delete(e.ctx, b.ID), domain.ErrNotFound)
}

func testChannels(t *testing.T, f Factory) {
	e := newEnv(t, f)
	d1 := e.device("d1")
	d2 := e.device("d2")
	d2off := d2
	d2off.Enabled = false
	d2off.PasswordEnc = nil
	e.ok(e.s.Devices().Update(e.ctx, d2off))

	c1 := e.channel(d1, "lte1", "sms", "pl")
	c2 := e.channel(d1, "lte2", "sms")
	c3 := e.channel(d2, "lte1")

	e.is(e.s.Channels().Create(e.ctx, domain.Channel{ID: e.id(), DeviceID: d1.ID, Port: "lte1", Name: "x"}), domain.ErrConflict)
	e.is(e.s.Channels().Create(e.ctx, domain.Channel{ID: e.id(), DeviceID: e.id(), Port: "lte1", Name: "x"}), domain.ErrNotFound)

	got, err := e.s.Channels().Get(e.ctx, c1.ID)
	e.ok(err)
	if got.Port != "lte1" || len(got.Tags) != 2 || got.Tags[0] != "sms" || !got.Enabled || got.State != domain.ChannelOK || got.Signal != nil {
		t.Fatalf("Get = %+v", got)
	}
	_, err = e.s.Channels().Get(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)
	if got, _ := e.s.Channels().Get(e.ctx, c3.ID); len(got.Tags) != 0 {
		t.Fatalf("tags = %v", got.Tags)
	}

	ids := func(cs []domain.Channel) []domain.ID {
		var out []domain.ID
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	check := func(name string, f store.ChannelFilter, want ...domain.ID) {
		t.Helper()
		cs, err := e.s.Channels().List(e.ctx, f)
		e.ok(err)
		got := ids(cs)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}
	check("all", store.ChannelFilter{}, c1.ID, c2.ID, c3.ID)
	check("device", store.ChannelFilter{DeviceID: d1.ID}, c1.ID, c2.ID)
	check("enabled", store.ChannelFilter{EnabledOnly: true}, c1.ID, c2.ID)
	check("tags all", store.ChannelFilter{Tags: []string{"sms", "pl"}}, c1.ID)
	check("tags one", store.ChannelFilter{Tags: []string{"sms"}}, c1.ID, c2.ID)
	check("tags none", store.ChannelFilter{Tags: []string{"nope"}})
	check("state", store.ChannelFilter{States: []domain.ChannelState{domain.ChannelDown}})
	check("state ok", store.ChannelFilter{States: []domain.ChannelState{domain.ChannelOK, domain.ChannelDown}}, c1.ID, c2.ID, c3.ID)

	// Update changes configuration only.
	upd := c1
	upd.Name = "renamed"
	upd.Tags = []string{"x"}
	upd.RatePerMin = 9
	upd.MaxSegments = 1
	upd.Transliterate = true
	upd.Enabled = false
	upd.Port = "ignored"
	e.ok(e.s.Channels().Update(e.ctx, upd))
	got, _ = e.s.Channels().Get(e.ctx, c1.ID)
	if got.Name != "renamed" || got.Port != "lte1" || got.RatePerMin != 9 || got.MaxSegments != 1 ||
		!got.Transliterate || got.Enabled || len(got.Tags) != 1 || got.State != domain.ChannelOK {
		t.Fatalf("after Update = %+v", got)
	}
	nf := c1
	nf.ID = e.id()
	e.is(e.s.Channels().Update(e.ctx, nf), domain.ErrNotFound)

	// Health.
	rssi, sinr := -70, 12
	h := store.ChannelHealthUpdate{State: domain.ChannelDegraded, StateReason: "401", At: T0,
		Signal: &domain.Signal{RSSI: &rssi, SINR: &sinr}, Operator: "Orange"}
	e.ok(e.s.Channels().UpdateHealth(e.ctx, c2.ID, h))
	got, _ = e.s.Channels().Get(e.ctx, c2.ID)
	if got.State != domain.ChannelDegraded || got.StateReason != "401" || got.Operator != "Orange" ||
		!got.LastHealthAt.Equal(T0) || got.Signal == nil || *got.Signal.RSSI != -70 || *got.Signal.SINR != 12 ||
		got.Signal.RSRP != nil || got.Signal.RSRQ != nil {
		t.Fatalf("health = %+v", got)
	}
	// Only the timestamp moved, shortly after: no change expected.
	h.At = T0.Add(time.Second)
	e.ok(e.s.Channels().UpdateHealth(e.ctx, c2.ID, h))
	got, _ = e.s.Channels().Get(e.ctx, c2.ID)
	if !got.LastHealthAt.Equal(T0) {
		t.Fatalf("timestamp-only update should be skipped, LastHealthAt = %v", got.LastHealthAt)
	}
	// A real change, or a stale timestamp, is stored.
	h.State = domain.ChannelOK
	h.StateReason = ""
	e.ok(e.s.Channels().UpdateHealth(e.ctx, c2.ID, h))
	got, _ = e.s.Channels().Get(e.ctx, c2.ID)
	if got.State != domain.ChannelOK || !got.LastHealthAt.Equal(T0.Add(time.Second)) {
		t.Fatalf("health = %+v", got)
	}
	h.At = T0.Add(time.Hour)
	e.ok(e.s.Channels().UpdateHealth(e.ctx, c2.ID, h))
	got, _ = e.s.Channels().Get(e.ctx, c2.ID)
	if !got.LastHealthAt.Equal(T0.Add(time.Hour)) {
		t.Fatalf("stale timestamp must be refreshed: %v", got.LastHealthAt)
	}
	h.Signal = nil
	h.At = T0.Add(time.Hour + time.Second)
	e.ok(e.s.Channels().UpdateHealth(e.ctx, c2.ID, h))
	got, _ = e.s.Channels().Get(e.ctx, c2.ID)
	if got.Signal != nil {
		t.Fatalf("signal not cleared: %+v", got.Signal)
	}
	e.is(e.s.Channels().UpdateHealth(e.ctx, e.id(), h), domain.ErrNotFound)

	// Delete: refused while a message is active, also when pinned.
	m := e.msg("+48600000002")
	m.Route.ChannelID = c3.ID
	e.enqueue(m)
	e.is(e.s.Channels().Delete(e.ctx, c3.ID), domain.ErrConflict)
	m2 := e.msg("+48600000003")
	e.enqueue(m2)
	acq, ok := e.acquire(c2.ID, nil, T0)
	if !ok || acq.ID != m.ID && acq.ID != m2.ID {
		t.Fatalf("acquire = %+v %v", acq, ok)
	}
	e.is(e.s.Channels().Delete(e.ctx, c2.ID), domain.ErrConflict)
	e.ok(e.s.Channels().Delete(e.ctx, c1.ID))
	e.is(e.s.Channels().Delete(e.ctx, c1.ID), domain.ErrNotFound)
}

func testUsers(t *testing.T, f Factory) {
	e := newEnv(t, f)
	mk := func(name string, role domain.Role) domain.User {
		u := domain.User{ID: e.id(), Username: name, PasswordHash: "$argon2id$x", Role: role, CreatedAt: T0}
		e.ok(e.s.Users().Create(e.ctx, u))
		return u
	}
	n, err := e.s.Users().Count(e.ctx)
	e.ok(err)
	if n != 0 {
		t.Fatalf("Count = %d", n)
	}
	admin := mk("Admin", domain.RoleAdmin)
	op := mk("op", domain.RoleOperator)

	e.is(e.s.Users().Create(e.ctx, domain.User{ID: e.id(), Username: "ADMIN", PasswordHash: "x", Role: domain.RoleViewer}), domain.ErrConflict)

	got, err := e.s.Users().GetByUsername(e.ctx, "aDMIN")
	e.ok(err)
	if got.ID != admin.ID || got.Username != "Admin" || got.Role != domain.RoleAdmin || got.Disabled || !got.CreatedAt.Equal(T0) {
		t.Fatalf("GetByUsername = %+v", got)
	}
	_, err = e.s.Users().GetByUsername(e.ctx, "nobody")
	e.is(err, domain.ErrNotFound)
	_, err = e.s.Users().Get(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)
	if got, err := e.s.Users().Get(e.ctx, op.ID); err != nil || got.Username != "op" {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	list, err := e.s.Users().List(e.ctx)
	e.ok(err)
	if len(list) != 2 || list[0].Username != "Admin" || list[1].Username != "op" {
		t.Fatalf("List = %+v", list)
	}
	if n, _ := e.s.Users().Count(e.ctx); n != 2 {
		t.Fatalf("Count = %d", n)
	}
	if n, _ := e.s.Users().CountActiveAdmins(e.ctx); n != 1 {
		t.Fatalf("CountActiveAdmins = %d", n)
	}

	// Last admin cannot be demoted or disabled.
	demote := admin
	demote.Role = domain.RoleOperator
	e.is(e.s.Users().Update(e.ctx, demote), domain.ErrConflict)
	disable := admin
	disable.Disabled = true
	e.is(e.s.Users().Update(e.ctx, disable), domain.ErrConflict)
	// ... but a second admin makes it legal.
	op2 := op
	op2.Role = domain.RoleAdmin
	op2.PasswordHash = "new"
	e.ok(e.s.Users().Update(e.ctx, op2))
	if n, _ := e.s.Users().CountActiveAdmins(e.ctx); n != 2 {
		t.Fatalf("CountActiveAdmins = %d", n)
	}
	e.ok(e.s.Users().Update(e.ctx, disable))
	if n, _ := e.s.Users().CountActiveAdmins(e.ctx); n != 1 {
		t.Fatalf("CountActiveAdmins = %d", n)
	}
	// Profile edits of the last admin that keep it an enabled admin are fine.
	op2.Username = "boss"
	e.ok(e.s.Users().Update(e.ctx, op2))
	got, _ = e.s.Users().Get(e.ctx, op.ID)
	if got.Username != "boss" || got.PasswordHash != "new" || got.Role != domain.RoleAdmin {
		t.Fatalf("after update = %+v", got)
	}
	dupName := op2
	dupName.Username = "admin"
	e.is(e.s.Users().Update(e.ctx, dupName), domain.ErrConflict)
	missing := op2
	missing.ID = e.id()
	e.is(e.s.Users().Update(e.ctx, missing), domain.ErrNotFound)
}

func testTokens(t *testing.T, f Factory) {
	e := newEnv(t, f)
	u1 := domain.User{ID: e.id(), Username: "u1", PasswordHash: "x", Role: domain.RoleAdmin, CreatedAt: T0}
	u2 := domain.User{ID: e.id(), Username: "u2", PasswordHash: "x", Role: domain.RoleViewer, CreatedAt: T0}
	e.ok(e.s.Users().Create(e.ctx, u1))
	e.ok(e.s.Users().Create(e.ctx, u2))

	t1 := domain.APIToken{ID: e.id(), UserID: u1.ID, Name: "ci", Prefix: "abc123", Hash: []byte{1, 2, 3},
		Scopes: domain.Scopes{domain.ScopeSMSSend, domain.ScopeSMSRead}, ExpiresAt: T0.Add(time.Hour), CreatedAt: T0}
	e.ok(e.s.Tokens().Create(e.ctx, t1))
	t2 := domain.APIToken{ID: e.id(), UserID: u2.ID, Name: "ro", Prefix: "def456", Hash: []byte{4}, CreatedAt: T0}
	e.ok(e.s.Tokens().Create(e.ctx, t2))

	dup := t2
	dup.ID = e.id()
	e.is(e.s.Tokens().Create(e.ctx, dup), domain.ErrConflict)
	orphan := domain.APIToken{ID: e.id(), UserID: e.id(), Name: "x", Prefix: "zzz", Hash: []byte{1}}
	e.is(e.s.Tokens().Create(e.ctx, orphan), domain.ErrNotFound)

	got, err := e.s.Tokens().GetByPrefix(e.ctx, "abc123")
	e.ok(err)
	if got.ID != t1.ID || got.UserID != u1.ID || string(got.Hash) != "\x01\x02\x03" || len(got.Scopes) != 2 ||
		got.Scopes[0] != domain.ScopeSMSSend || !got.ExpiresAt.Equal(T0.Add(time.Hour)) || !got.LastUsedAt.IsZero() {
		t.Fatalf("GetByPrefix = %+v", got)
	}
	_, err = e.s.Tokens().GetByPrefix(e.ctx, "nope")
	e.is(err, domain.ErrNotFound)
	if got, _ := e.s.Tokens().GetByPrefix(e.ctx, "def456"); len(got.Scopes) != 0 {
		t.Fatalf("scopes = %v", got.Scopes)
	}

	all, err := e.s.Tokens().List(e.ctx, "")
	e.ok(err)
	if len(all) != 2 || all[0].ID != t2.ID || all[1].ID != t1.ID {
		t.Fatalf("List(all) = %+v", all)
	}
	mine, _ := e.s.Tokens().List(e.ctx, u1.ID)
	if len(mine) != 1 || mine[0].ID != t1.ID {
		t.Fatalf("List(u1) = %+v", mine)
	}

	// TouchLastUsed coalesces writes inside the interval.
	e.ok(e.s.Tokens().TouchLastUsed(e.ctx, t1.ID, T0.Add(time.Hour)))
	e.ok(e.s.Tokens().TouchLastUsed(e.ctx, t1.ID, T0.Add(time.Hour+time.Second)))
	got, _ = e.s.Tokens().GetByPrefix(e.ctx, "abc123")
	if !got.LastUsedAt.Equal(T0.Add(time.Hour)) {
		t.Fatalf("LastUsedAt = %v", got.LastUsedAt)
	}
	e.ok(e.s.Tokens().TouchLastUsed(e.ctx, t1.ID, T0.Add(2*time.Hour)))
	got, _ = e.s.Tokens().GetByPrefix(e.ctx, "abc123")
	if !got.LastUsedAt.Equal(T0.Add(2 * time.Hour)) {
		t.Fatalf("LastUsedAt = %v", got.LastUsedAt)
	}
	e.is(e.s.Tokens().TouchLastUsed(e.ctx, e.id(), T0), domain.ErrNotFound)

	e.ok(e.s.Tokens().Delete(e.ctx, t1.ID))
	e.is(e.s.Tokens().Delete(e.ctx, t1.ID), domain.ErrNotFound)
	_, err = e.s.Tokens().GetByPrefix(e.ctx, "abc123")
	e.is(err, domain.ErrNotFound)
}

func testAudit(t *testing.T, f Factory) {
	e := newEnv(t, f)
	actor := domain.UserActor(tokenOwner)
	var all []domain.AuditLog
	for i := 0; i < 5; i++ {
		a := domain.AuditLog{ID: e.id(), Actor: actor, Action: domain.AuditLoginSuccess, Target: "user:x",
			At: T0.Add(time.Duration(i) * time.Minute)}
		if i%2 == 1 {
			a.Action = domain.AuditDeviceCreate
			a.Actor = domain.SystemActor
			a.Meta = map[string]string{"name": "d", "k": "v"}
		}
		e.ok(e.s.Audit().Append(e.ctx, a))
		all = append(all, a)
	}
	e.is(e.s.Audit().Append(e.ctx, all[0]), domain.ErrConflict)

	page, err := e.s.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Limit: 2})
	e.ok(err)
	if len(page.Items) != 2 || page.Items[0].ID != all[4].ID || page.Items[1].ID != all[3].ID || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	if page.Items[1].Meta["name"] != "d" || page.Items[1].Actor != domain.SystemActor || page.Items[0].Meta != nil {
		t.Fatalf("meta/actor = %+v", page.Items)
	}
	page, err = e.s.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Limit: 2, Cursor: page.NextCursor})
	e.ok(err)
	if len(page.Items) != 2 || page.Items[0].ID != all[2].ID || page.NextCursor == "" {
		t.Fatalf("page 2 = %+v", page)
	}
	page, err = e.s.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Limit: 2, Cursor: page.NextCursor})
	e.ok(err)
	if len(page.Items) != 1 || page.Items[0].ID != all[0].ID || page.NextCursor != "" {
		t.Fatalf("page 3 = %+v", page)
	}
	if _, err := e.s.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Cursor: "garbage"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}

	count := func(f store.AuditFilter) int {
		p, err := e.s.Audit().List(e.ctx, f, store.PageRequest{})
		e.ok(err)
		return len(p.Items)
	}
	if n := count(store.AuditFilter{Action: domain.AuditDeviceCreate}); n != 2 {
		t.Fatalf("by action = %d", n)
	}
	if n := count(store.AuditFilter{Actor: actor}); n != 3 {
		t.Fatalf("by actor = %d", n)
	}
	if n := count(store.AuditFilter{Since: T0.Add(time.Minute), Until: T0.Add(3 * time.Minute)}); n != 2 {
		t.Fatalf("by time = %d", n)
	}
}

// ---------------------------------------------------------------------------
// Messages

func testEnqueue(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	m1 := e.msg("+48600000001")
	m1.Route = domain.Route{ChannelID: ch, Tags: []string{"a", "b"}}
	m1.ClientRef = "ref-1"
	m1.ExpiresAt = T0.Add(time.Hour)
	m1.NextAttemptAt = time.Time{}
	m1.Attempts = 7 // ignored
	m1.Status = domain.StatusSent
	res, err := e.s.Messages().Enqueue(e.ctx, []domain.Message{m1}, store.Idempotency{})
	e.ok(err)
	if res.Replayed || len(res.Messages) != 1 {
		t.Fatalf("res = %+v", res)
	}
	got, err := e.s.Messages().Get(e.ctx, m1.ID)
	e.ok(err)
	if got.Status != domain.StatusQueued || got.Direction != domain.DirectionOut || got.Attempts != 0 ||
		got.ChannelID != ch || got.Route.ChannelID != ch || len(got.Route.Tags) != 2 ||
		!got.NextAttemptAt.Equal(T0) || !got.ExpiresAt.Equal(T0.Add(time.Hour)) || got.ClientRef != "ref-1" ||
		got.CreatedBy != m1.CreatedBy || !got.CreatedAt.Equal(T0) || got.Peer != m1.Peer || got.Body != m1.Body ||
		got.Encoding != domain.EncodingGSM7 || got.Segments != 1 || got.PossibleDuplicate || !got.LeaseUntil.IsZero() {
		t.Fatalf("stored = %+v", got)
	}
	if res.Messages[0].Status != domain.StatusQueued || !res.Messages[0].NextAttemptAt.Equal(T0) {
		t.Fatalf("returned = %+v", res.Messages[0])
	}
	_, err = e.s.Messages().Get(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)

	// Unpinned messages have no channel; CreatedAt defaults to the store clock.
	m2 := e.msg("+48600000002")
	m2.CreatedAt = time.Time{}
	m2.NextAttemptAt = time.Time{}
	res, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{m2}, store.Idempotency{})
	e.ok(err)
	if res.Messages[0].CreatedAt.IsZero() || !res.Messages[0].NextAttemptAt.Equal(res.Messages[0].CreatedAt) {
		t.Fatalf("defaults = %+v", res.Messages[0])
	}
	got, _ = e.s.Messages().Get(e.ctx, m2.ID)
	if got.ChannelID != "" || got.Route.Tags != nil {
		t.Fatalf("stored = %+v", got)
	}

	// Invalid input.
	_, err = e.s.Messages().Enqueue(e.ctx, nil, store.Idempotency{})
	if err == nil {
		t.Fatal("empty batch accepted")
	}
	noID := e.msg("+48600000003")
	noID.ID = ""
	if _, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{noID}, store.Idempotency{}); err == nil {
		t.Fatal("message without ID accepted")
	}
	other := e.msg("+48600000004")
	other.CreatedBy = domain.UserActor(tokenOwner)
	if _, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{e.msg("+48600000005"), other}, store.Idempotency{}); err == nil {
		t.Fatal("mixed CreatedBy accepted")
	}

	// A batch is atomic: a duplicate ID rolls back the whole request.
	a, b := e.msg("+48600000006"), e.msg("+48600000007")
	dup := b
	_, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{a, b, dup}, store.Idempotency{})
	e.is(err, domain.ErrConflict)
	for _, m := range []domain.Message{a, b} {
		if _, err := e.s.Messages().Get(e.ctx, m.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("message %s survived a failed batch: %v", m.ID, err)
		}
	}
}

func testIdempotency(t *testing.T, f Factory) {
	e := newEnv(t, f)
	idem := store.Idempotency{Key: "k1", RequestHash: "h1", Since: T0.Add(-24 * time.Hour)}
	a, b := e.msg("+48600000001"), e.msg("+48600000002")
	res, err := e.s.Messages().Enqueue(e.ctx, []domain.Message{a, b}, idem)
	e.ok(err)
	if res.Replayed || len(res.Messages) != 2 || res.Messages[0].IdempotencyKey != "k1" {
		t.Fatalf("first = %+v", res)
	}

	// Same key and hash: nothing new, original messages in current state.
	if _, err := e.s.Messages().Cancel(e.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	c := e.msg("+48600000003")
	res, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{c}, idem)
	e.ok(err)
	if !res.Replayed || len(res.Messages) != 2 || res.Messages[0].ID != a.ID || res.Messages[1].ID != b.ID ||
		res.Messages[0].Status != domain.StatusCancelled {
		t.Fatalf("replay = %+v", res)
	}
	if _, err := e.s.Messages().Get(e.ctx, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("replay stored a message: %v", err)
	}

	// Different hash: mismatch, and nothing stored.
	bad := idem
	bad.RequestHash = "h2"
	_, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{c}, bad)
	e.is(err, domain.ErrIdempotencyMismatch)
	if _, err := e.s.Messages().Get(e.ctx, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("mismatch stored a message: %v", err)
	}

	// Keys are scoped per creator.
	d := e.msg("+48600000004")
	d.CreatedBy = domain.UserActor(tokenOwner)
	res, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{d}, bad)
	e.ok(err)
	if res.Replayed {
		t.Fatal("key leaked across creators")
	}

	// A record older than the window is ignored and replaced.
	stale := store.Idempotency{Key: "k1", RequestHash: "h3", Since: T0.Add(time.Hour)}
	res, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{c}, stale)
	e.ok(err)
	if res.Replayed {
		t.Fatalf("stale record replayed: %+v", res)
	}
	stale.Since = T0.Add(-time.Hour)
	res, err = e.s.Messages().Enqueue(e.ctx, []domain.Message{e.msg("+48600000009")}, stale)
	e.ok(err)
	if !res.Replayed || res.Messages[0].ID != c.ID {
		t.Fatalf("new record not used: %+v", res)
	}

	// Concurrent requests with one key create the messages once.
	conc := store.Idempotency{Key: "race", RequestHash: "h", Since: T0.Add(-time.Hour)}
	const n = 8
	var wg sync.WaitGroup
	replayed := make([]bool, n)
	errs := make([]error, n)
	var mu sync.Mutex
	msgs := make([]domain.Message, n)
	for i := range msgs {
		mu.Lock()
		msgs[i] = e.msg(fmt.Sprintf("+4860001%04d", i))
		mu.Unlock()
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.s.Messages().Enqueue(e.ctx, []domain.Message{msgs[i]}, conc)
			replayed[i], errs[i] = r.Replayed, err
		}()
	}
	wg.Wait()
	fresh := 0
	for i := range errs {
		e.ok(errs[i])
		if !replayed[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d requests created messages, want 1", fresh)
	}
}

func testAcquire(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	rq := store.AcquireRequest{ChannelID: ch, Now: T0, LeaseUntil: T0.Add(time.Minute)}

	// Empty queue.
	_, ok, err := e.s.Messages().AcquireNext(e.ctx, rq)
	e.ok(err)
	if ok {
		t.Fatal("acquired from empty queue")
	}
	_, ok, err = e.s.Messages().NextDue(e.ctx, ch, nil)
	e.ok(err)
	if ok {
		t.Fatal("NextDue on empty queue")
	}

	late := e.msg("+48600000001")
	late.NextAttemptAt = T0.Add(10 * time.Minute)
	first := e.msg("+48600000002")
	first.NextAttemptAt = T0.Add(-2 * time.Minute)
	second := e.msg("+48600000003") // same NextAttemptAt as third, lower ID
	third := e.msg("+48600000004")
	expired := e.msg("+48600000005")
	expired.ExpiresAt = T0.Add(-time.Second)
	expired.NextAttemptAt = T0.Add(-time.Hour)
	e.enqueue(late, first, second, third, expired)

	at, ok, err := e.s.Messages().NextDue(e.ctx, ch, nil)
	e.ok(err)
	if !ok || !at.Equal(T0.Add(-time.Hour)) {
		t.Fatalf("NextDue = %v %v", at, ok)
	}

	var order []domain.ID
	for {
		m, ok := e.acquire(ch, nil, T0)
		if !ok {
			break
		}
		if m.Status != domain.StatusSending || m.Attempts != 1 || m.ChannelID != ch ||
			!m.LeaseUntil.Equal(T0.Add(time.Minute)) || m.Direction != domain.DirectionOut {
			t.Fatalf("acquired = %+v", m)
		}
		order = append(order, m.ID)
	}
	want := []domain.ID{first.ID, second.ID, third.ID}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	// The expired message stays queued and is never handed out; the late one waits.
	if m, _ := e.s.Messages().Get(e.ctx, expired.ID); m.Status != domain.StatusQueued {
		t.Fatalf("expired = %+v", m)
	}
	if m, ok := e.acquire(ch, nil, T0.Add(10*time.Minute)); !ok || m.ID != late.ID {
		t.Fatalf("late = %+v %v", m, ok)
	}
}

func testRouting(t *testing.T, f Factory) {
	e := newEnv(t, f)
	chA, chB := e.id(), e.id()

	free := e.msg("+48600000001")
	pinnedA := e.msg("+48600000002")
	pinnedA.Route.ChannelID = chA
	tagged := e.msg("+48600000003")
	tagged.Route.Tags = []string{"pl", "vip"}
	taggedPl := e.msg("+48600000004")
	taggedPl.Route.Tags = []string{"pl"}
	e.enqueue(free, pinnedA, tagged, taggedPl)

	// Channel B without tags: only the unconstrained message.
	m, ok := e.acquire(chB, nil, T0)
	if !ok || m.ID != free.ID {
		t.Fatalf("B = %+v %v", m, ok)
	}
	if _, ok := e.acquire(chB, nil, T0); ok {
		t.Fatal("B must not take pinned or tagged messages")
	}
	if _, ok := e.acquire(chB, []string{"vip"}, T0); ok {
		t.Fatal("tags must be a superset of the route tags")
	}
	at, ok, err := e.s.Messages().NextDue(e.ctx, chB, nil)
	e.ok(err)
	if ok {
		t.Fatalf("NextDue for B = %v", at)
	}
	if _, ok, _ := e.s.Messages().NextDue(e.ctx, chB, []string{"pl"}); !ok {
		t.Fatal("NextDue must see tagged work for a matching channel")
	}

	// A channel with pl+vip+extra takes both tagged messages, oldest first.
	m, ok = e.acquire(chB, []string{"vip", "pl", "extra"}, T0)
	if !ok || m.ID != tagged.ID {
		t.Fatalf("tagged = %+v %v", m, ok)
	}
	m, ok = e.acquire(chB, []string{"pl"}, T0)
	if !ok || m.ID != taggedPl.ID {
		t.Fatalf("taggedPl = %+v %v", m, ok)
	}
	// The pinned one goes to A only, even though A has no tags.
	m, ok = e.acquire(chA, nil, T0)
	if !ok || m.ID != pinnedA.ID || m.ChannelID != chA {
		t.Fatalf("pinned = %+v %v", m, ok)
	}
}

func testLease(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch, ch2 := e.id(), e.id()
	mk := func(peer string) domain.Message {
		m := e.msg(peer)
		e.enqueue(m)
		got, ok := e.acquire(ch, nil, T0)
		if !ok || got.ID != m.ID {
			t.Fatalf("acquire = %+v %v", got, ok)
		}
		return got
	}

	// MarkSent.
	m := mk("+48600000001")
	e.ok(e.s.Messages().MarkSent(e.ctx, m.Lease(), T0.Add(time.Second)))
	got, _ := e.s.Messages().Get(e.ctx, m.ID)
	if got.Status != domain.StatusSent || !got.SentAt.Equal(T0.Add(time.Second)) || !got.LeaseUntil.IsZero() {
		t.Fatalf("sent = %+v", got)
	}
	// The lease is spent: a second completion is fenced off.
	e.is(e.s.Messages().MarkSent(e.ctx, m.Lease(), T0), domain.ErrLeaseLost)
	e.is(e.s.Messages().MarkFailed(e.ctx, m.Lease(), "x", false), domain.ErrLeaseLost)
	e.is(e.s.Messages().Requeue(e.ctx, m.Lease(), store.RequeueParams{}), domain.ErrLeaseLost)

	// MarkFailed ORs the possible-duplicate flag.
	m = mk("+48600000002")
	e.ok(e.s.Messages().MarkFailed(e.ctx, m.Lease(), "permanent: bad number", true))
	got, _ = e.s.Messages().Get(e.ctx, m.ID)
	if got.Status != domain.StatusFailed || got.LastError != "permanent: bad number" || !got.PossibleDuplicate || !got.LeaseUntil.IsZero() {
		t.Fatalf("failed = %+v", got)
	}

	// Requeue: unpinned messages lose their channel, retry later, keep the flag.
	m = mk("+48600000003")
	retryAt := T0.Add(30 * time.Second)
	e.ok(e.s.Messages().Requeue(e.ctx, m.Lease(), store.RequeueParams{NextAttemptAt: retryAt, LastError: "timeout", PossibleDuplicate: true}))
	got, _ = e.s.Messages().Get(e.ctx, m.ID)
	if got.Status != domain.StatusQueued || got.ChannelID != "" || !got.NextAttemptAt.Equal(retryAt) ||
		got.LastError != "timeout" || !got.PossibleDuplicate || got.Attempts != 1 || !got.LeaseUntil.IsZero() {
		t.Fatalf("requeued = %+v", got)
	}
	if _, ok := e.acquire(ch2, nil, T0); ok {
		t.Fatal("message acquired before NextAttemptAt")
	}
	m2, ok := e.acquire(ch2, nil, retryAt)
	if !ok || m2.ID != m.ID || m2.Attempts != 2 || m2.ChannelID != ch2 {
		t.Fatalf("second attempt = %+v %v", m2, ok)
	}
	// The first lease (attempt 1) is stale now.
	e.is(e.s.Messages().MarkSent(e.ctx, m.Lease(), T0), domain.ErrLeaseLost)
	e.ok(e.s.Messages().MarkSent(e.ctx, m2.Lease(), retryAt))
	got, _ = e.s.Messages().Get(e.ctx, m.ID)
	if got.Status != domain.StatusSent || !got.PossibleDuplicate {
		t.Fatalf("final = %+v", got)
	}

	// A pinned message keeps its channel when requeued.
	p := e.msg("+48600000004")
	p.Route.ChannelID = ch
	e.enqueue(p)
	pm, ok := e.acquire(ch, nil, T0)
	if !ok {
		t.Fatal("pinned not acquired")
	}
	e.ok(e.s.Messages().Requeue(e.ctx, pm.Lease(), store.RequeueParams{NextAttemptAt: T0}))
	got, _ = e.s.Messages().Get(e.ctx, p.ID)
	if got.ChannelID != ch || got.Status != domain.StatusQueued || got.PossibleDuplicate {
		t.Fatalf("pinned requeued = %+v", got)
	}

	// Unknown message.
	e.is(e.s.Messages().MarkSent(e.ctx, domain.Lease{MessageID: e.id(), Attempt: 1}, T0), domain.ErrLeaseLost)
}

func testCancel(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	q, s := e.msg("+48600000001"), e.msg("+48600000002")
	e.enqueue(q, s)

	got, err := e.s.Messages().Cancel(e.ctx, q.ID)
	e.ok(err)
	if got.Status != domain.StatusCancelled || got.ID != q.ID {
		t.Fatalf("cancelled = %+v", got)
	}
	got, err = e.s.Messages().Cancel(e.ctx, q.ID)
	e.is(err, domain.ErrInvalidTransition)
	if got.Status != domain.StatusCancelled {
		t.Fatalf("returned = %+v", got)
	}
	if _, ok := e.acquire(ch, nil, T0); !ok {
		t.Fatal("remaining message not acquired")
	}
	got, err = e.s.Messages().Cancel(e.ctx, s.ID)
	e.is(err, domain.ErrInvalidTransition)
	if got.Status != domain.StatusSending {
		t.Fatalf("returned = %+v", got)
	}
	_, err = e.s.Messages().Cancel(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)

	in := e.msg("+48600000003")
	in.DedupHash = "h-cancel"
	_, err = e.s.Messages().InsertInbound(e.ctx, in)
	e.ok(err)
	_, err = e.s.Messages().Cancel(e.ctx, in.ID)
	e.is(err, domain.ErrInvalidTransition)
}

func testRecover(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	pinnedCh := e.id()
	// take enqueues m and leases it at once; it is the only queued message.
	take := func(m domain.Message, c domain.ID, until time.Time, attempts int) domain.Message {
		t.Helper()
		e.enqueue(m)
		var got domain.Message
		for i := 0; i < attempts; i++ {
			var ok bool
			var err error
			got, ok, err = e.s.Messages().AcquireNext(e.ctx, store.AcquireRequest{ChannelID: c, Now: T0, LeaseUntil: until})
			e.ok(err)
			if !ok || got.ID != m.ID {
				t.Fatalf("message %s not acquired: %+v %v", m.ID, got, ok)
			}
			if i < attempts-1 {
				e.ok(e.s.Messages().Requeue(e.ctx, got.Lease(), store.RequeueParams{NextAttemptAt: T0}))
			}
		}
		return got
	}
	fresh, stale, last, pinned := e.msg("+48600000001"), e.msg("+48600000002"), e.msg("+48600000003"), e.msg("+48600000004")
	pinned.Route.ChannelID = pinnedCh
	take(fresh, ch, T0.Add(time.Hour), 1)
	take(stale, ch, T0.Add(time.Minute), 1)
	take(last, ch, T0.Add(time.Minute), 3)
	take(pinned, pinnedCh, T0.Add(time.Minute), 1)

	// Nothing is due yet.
	rec, err := e.s.Messages().RecoverExpiredLeases(e.ctx, T0, 3)
	e.ok(err)
	if len(rec) != 0 {
		t.Fatalf("recovered early: %+v", rec)
	}
	if got, _ := e.s.Messages().Get(e.ctx, last.ID); got.Attempts != 3 {
		t.Fatalf("attempts = %d", got.Attempts)
	}

	now := T0.Add(2 * time.Minute)
	rec, err = e.s.Messages().RecoverExpiredLeases(e.ctx, now, 3)
	e.ok(err)
	byID := map[domain.ID]store.RecoveredLease{}
	for _, r := range rec {
		byID[r.MessageID] = r
	}
	if len(rec) != 3 {
		t.Fatalf("recovered = %+v", rec)
	}
	if r := byID[stale.ID]; r.NewStatus != domain.StatusQueued || r.ChannelID != ch || r.Attempts != 1 {
		t.Fatalf("stale = %+v", r)
	}
	if r := byID[last.ID]; r.NewStatus != domain.StatusFailed || r.Attempts != 3 {
		t.Fatalf("last = %+v", r)
	}
	if r := byID[pinned.ID]; r.NewStatus != domain.StatusQueued || r.ChannelID != pinnedCh {
		t.Fatalf("pinned = %+v", r)
	}

	got, _ := e.s.Messages().Get(e.ctx, stale.ID)
	if got.Status != domain.StatusQueued || !got.PossibleDuplicate || got.ChannelID != "" ||
		!got.NextAttemptAt.Equal(now) || got.LastError == "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("stale = %+v", got)
	}
	got, _ = e.s.Messages().Get(e.ctx, last.ID)
	if got.Status != domain.StatusFailed || !got.PossibleDuplicate || got.LastError == "" {
		t.Fatalf("last = %+v", got)
	}
	got, _ = e.s.Messages().Get(e.ctx, pinned.ID)
	if got.Status != domain.StatusQueued || got.ChannelID != pinnedCh {
		t.Fatalf("pinned = %+v", got)
	}
	// The healthy lease is untouched.
	got, _ = e.s.Messages().Get(e.ctx, fresh.ID)
	if got.Status != domain.StatusSending || got.PossibleDuplicate {
		t.Fatalf("fresh = %+v", got)
	}
	// And the old worker cannot overwrite the recovered state.
	e.is(e.s.Messages().MarkSent(e.ctx, domain.Lease{MessageID: last.ID, Attempt: 3}, now), domain.ErrLeaseLost)
}

func testExpire(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	a, b, c := e.msg("+48600000001"), e.msg("+48600000002"), e.msg("+48600000003")
	a.ExpiresAt = T0.Add(time.Minute)
	b.ExpiresAt = T0.Add(time.Hour)
	e.enqueue(a, b, c)

	n, err := e.s.Messages().ExpireQueued(e.ctx, T0)
	e.ok(err)
	if n != 0 {
		t.Fatalf("expired %d too early", n)
	}
	n, err = e.s.Messages().ExpireQueued(e.ctx, T0.Add(time.Minute))
	e.ok(err)
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}
	got, _ := e.s.Messages().Get(e.ctx, a.ID)
	if got.Status != domain.StatusFailed || got.LastError != domain.LastErrorExpired {
		t.Fatalf("expired = %+v", got)
	}
	// Sending messages never expire.
	m, ok := e.acquire(ch, nil, T0.Add(2*time.Minute))
	if !ok || m.ID != b.ID {
		t.Fatalf("acquire = %+v %v", m, ok)
	}
	n, _ = e.s.Messages().ExpireQueued(e.ctx, T0.Add(3*time.Hour))
	if n != 0 {
		t.Fatalf("expired %d sending messages", n)
	}
}

func testListCount(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	var out []domain.Message
	for i := 0; i < 7; i++ {
		m := e.msg(fmt.Sprintf("+4860000000%d", i%3))
		m.CreatedAt = T0.Add(time.Duration(i) * time.Hour)
		m.NextAttemptAt = T0
		if i == 5 {
			m.CreatedBy = domain.UserActor(tokenOwner)
		}
		out = append(out, m)
	}
	for _, m := range out { // one request each: creators differ
		e.enqueue(m)
	}
	in := e.msg("+48600000009")
	in.DedupHash = "h-list"
	in.CreatedAt = T0.Add(10 * time.Hour)
	_, err := e.s.Messages().InsertInbound(e.ctx, in)
	e.ok(err)
	// out[0] -> sending (channel ch), out[1] -> cancelled.
	if m, ok := e.acquire(ch, nil, T0); !ok || m.ID != out[0].ID {
		t.Fatalf("acquire = %+v %v", m, ok)
	}
	if _, err := e.s.Messages().Cancel(e.ctx, out[1].ID); err != nil {
		t.Fatal(err)
	}

	// Paging, newest first.
	var seen []domain.ID
	cursor := ""
	pages := 0
	for {
		p, err := e.s.Messages().List(e.ctx, store.MessageFilter{}, store.PageRequest{Limit: 3, Cursor: cursor})
		e.ok(err)
		pages++
		for _, m := range p.Items {
			seen = append(seen, m.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 8 || pages != 3 || seen[0] != in.ID || seen[7] != out[0].ID {
		t.Fatalf("paged %d items in %d pages: %v", len(seen), pages, seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("not descending: %v", seen)
		}
	}
	if _, err := e.s.Messages().List(e.ctx, store.MessageFilter{}, store.PageRequest{Cursor: "bad"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}

	cases := []struct {
		name string
		f    store.MessageFilter
		want int
	}{
		{"all", store.MessageFilter{}, 8},
		{"out", store.MessageFilter{Direction: domain.DirectionOut}, 7},
		{"in", store.MessageFilter{Direction: domain.DirectionIn}, 1},
		{"queued", store.MessageFilter{Statuses: []domain.MessageStatus{domain.StatusQueued}}, 5},
		{"queued|sending", store.MessageFilter{Statuses: []domain.MessageStatus{domain.StatusQueued, domain.StatusSending}}, 6},
		{"peer", store.MessageFilter{Peer: "+48600000000"}, 3},
		{"channel", store.MessageFilter{ChannelID: ch}, 1},
		{"created by user", store.MessageFilter{CreatedBy: domain.UserActor(tokenOwner)}, 1},
		{"created by token", store.MessageFilter{CreatedBy: domain.TokenActor(tokenOwner)}, 6},
		{"since", store.MessageFilter{Since: T0.Add(3 * time.Hour)}, 5},
		{"until", store.MessageFilter{Until: T0.Add(3 * time.Hour)}, 3},
		{"combined", store.MessageFilter{Direction: domain.DirectionOut, Since: T0.Add(time.Hour), Until: T0.Add(4 * time.Hour), Peer: "+48600000001"}, 1},
	}
	for _, tc := range cases {
		n, err := e.s.Messages().Count(e.ctx, tc.f)
		e.ok(err)
		if n != tc.want {
			t.Errorf("Count %s = %d, want %d", tc.name, n, tc.want)
		}
		p, err := e.s.Messages().List(e.ctx, tc.f, store.PageRequest{Limit: 100})
		e.ok(err)
		if len(p.Items) != tc.want {
			t.Errorf("List %s = %d, want %d", tc.name, len(p.Items), tc.want)
		}
	}
}

func testInbound(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	hash := domain.InboundDedupHash(e.id(), "lte1", "+48600000001", "2026-01-02 03:04:05", "hi")
	m := domain.Message{
		ID: e.id(), ChannelID: ch, Peer: "+48600000001", Body: "hi", Encoding: domain.EncodingGSM7,
		Segments: 1, ReceivedAt: T0.Add(-time.Minute), DedupHash: hash,
		CreatedBy: domain.TokenActor(tokenOwner), Status: domain.StatusQueued, // overridden
	}
	created, err := e.s.Messages().InsertInbound(e.ctx, m)
	e.ok(err)
	if !created {
		t.Fatal("first insert reported duplicate")
	}
	got, err := e.s.Messages().Get(e.ctx, m.ID)
	e.ok(err)
	if got.Direction != domain.DirectionIn || got.Status != domain.StatusReceived || got.DedupHash != hash ||
		got.ChannelID != ch || !got.CreatedBy.IsZero() || !got.ReceivedAt.Equal(T0.Add(-time.Minute)) ||
		got.CreatedAt.IsZero() || got.Body != "hi" {
		t.Fatalf("stored = %+v", got)
	}

	// Same hash, new ID: duplicate, not an error, nothing stored.
	dup := m
	dup.ID = e.id()
	created, err = e.s.Messages().InsertInbound(e.ctx, dup)
	e.ok(err)
	if created {
		t.Fatal("duplicate reported as created")
	}
	if _, err := e.s.Messages().Get(e.ctx, dup.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("duplicate stored: %v", err)
	}
	n, _ := e.s.Messages().Count(e.ctx, store.MessageFilter{Direction: domain.DirectionIn})
	if n != 1 {
		t.Fatalf("inbound count = %d", n)
	}

	// Same ID, different hash is a real conflict.
	clash := m
	clash.DedupHash = "other"
	_, err = e.s.Messages().InsertInbound(e.ctx, clash)
	e.is(err, domain.ErrConflict)
	if _, err = e.s.Messages().InsertInbound(e.ctx, domain.Message{}); err == nil {
		t.Fatal("inbound without ID accepted")
	}

	// Inbound messages are never handed to dispatchers.
	if _, ok := e.acquire(ch, nil, T0); ok {
		t.Fatal("inbound acquired")
	}
}

func testInboundParts(t *testing.T, f Factory) {
	e := newEnv(t, f)
	dev := e.device("r1")
	ch := e.channel(dev, "lte1")
	other := e.channel(dev, "lte2")
	parts := e.s.InboundParts()
	mk := func(c domain.Channel, hash string, at time.Time) store.InboundPart {
		return store.InboundPart{
			ID: e.id(), ChannelID: c.ID, DedupHash: hash, Phone: "+48600000001", RawTimestamp: "2026-01-02 03:04:05",
			ReceivedAt: T0, Body: "body-" + hash, PDU: "pdu-" + hash, CreatedAt: at,
		}
	}

	none, err := parts.ListPending(e.ctx, ch.ID)
	e.ok(err)
	if len(none) != 0 {
		t.Fatalf("pending in empty store: %v", none)
	}

	p2 := mk(ch, "h2", T0.Add(2*time.Second))
	p1 := mk(ch, "h1", T0.Add(time.Second))
	po := mk(other, "h3", T0)
	for _, p := range []store.InboundPart{p2, p1, po} {
		created, err := parts.Add(e.ctx, p)
		e.ok(err)
		if !created {
			t.Fatalf("Add %s reported duplicate", p.DedupHash)
		}
	}

	// Duplicate hash with a new ID: not an error, nothing stored.
	dup := p1
	dup.ID = e.id()
	created, err := parts.Add(e.ctx, dup)
	e.ok(err)
	if created {
		t.Fatal("duplicate part reported as created")
	}

	// Per channel, oldest first, with all fields.
	got, err := parts.ListPending(e.ctx, ch.ID)
	e.ok(err)
	if len(got) != 2 || got[0].ID != p1.ID || got[1].ID != p2.ID {
		t.Fatalf("pending = %+v", got)
	}
	if got[0] != p1 {
		t.Fatalf("stored part = %+v, want %+v", got[0], p1)
	}

	// Consume turns parts into content-free tombstones that still dedupe.
	e.ok(parts.Consume(e.ctx, nil, e.id(), T0))
	e.ok(parts.Consume(e.ctx, []domain.ID{p1.ID, p2.ID, e.id()}, e.id(), T0.Add(time.Minute)))
	got, err = parts.ListPending(e.ctx, ch.ID)
	e.ok(err)
	if len(got) != 0 {
		t.Fatalf("pending after consume: %+v", got)
	}
	created, err = parts.Add(e.ctx, mk(ch, "h1", T0))
	e.ok(err)
	if created {
		t.Fatal("tombstone did not dedupe")
	}
	if err := parts.Consume(e.ctx, []domain.ID{po.ID}, "", T0); err == nil {
		t.Fatal("consume without message ID accepted")
	}
	// A consumed part cannot be consumed again by another message.
	e.ok(parts.Consume(e.ctx, []domain.ID{p1.ID}, e.id(), T0.Add(time.Hour)))

	// Purge: only tombstones consumed before the cutoff.
	n, err := parts.PurgeConsumed(e.ctx, T0.Add(time.Minute))
	e.ok(err)
	if n != 0 {
		t.Fatalf("purged %d tombstones too early", n)
	}
	n, err = parts.PurgeConsumed(e.ctx, T0.Add(2*time.Minute))
	e.ok(err)
	if n != 2 { // p1 kept its first consumed_at, so both go
		t.Fatalf("purged %d, want 2", n)
	}
	created, err = parts.Add(e.ctx, mk(ch, "h2", T0))
	e.ok(err)
	if !created {
		t.Fatal("purged tombstone still dedupes")
	}
	// Pending parts are never purged.
	n, err = parts.PurgeConsumed(e.ctx, T0.Add(1000*time.Hour))
	e.ok(err)
	if n != 0 {
		t.Fatalf("purged %d, want 0", n)
	}
	got, err = parts.ListPending(e.ctx, other.ID)
	e.ok(err)
	if len(got) != 1 {
		t.Fatalf("other channel pending = %+v", got)
	}

	// Parts go with their channel; unknown channel is rejected.
	e.ok(e.s.Channels().Delete(e.ctx, other.ID))
	got, err = parts.ListPending(e.ctx, other.ID)
	e.ok(err)
	if len(got) != 0 {
		t.Fatalf("parts of a deleted channel survived: %+v", got)
	}
	_, err = parts.Add(e.ctx, mk(other, "h9", T0))
	e.is(err, domain.ErrNotFound)
	if _, err := parts.Add(e.ctx, store.InboundPart{}); err == nil {
		t.Fatal("empty part accepted")
	}
}

func testRetention(t *testing.T, f Factory) {
	e := newEnv(t, f)
	ch := e.id()
	old1, old2, oldQueued, fresh := e.msg("+48600000001"), e.msg("+48600000002"), e.msg("+48600000003"), e.msg("+48600000004")
	old1.CreatedAt = T0.Add(-100 * 24 * time.Hour)
	old2.CreatedAt = T0.Add(-100 * 24 * time.Hour)
	oldQueued.CreatedAt = T0.Add(-100 * 24 * time.Hour)
	oldQueued.NextAttemptAt = T0.Add(24 * time.Hour) // not due, stays queued
	old1.NextAttemptAt, old2.NextAttemptAt = old1.CreatedAt, old1.CreatedAt
	e.enqueue(old1, old2, oldQueued, fresh)
	in := e.msg("+48600000005")
	in.CreatedAt = T0.Add(-100 * 24 * time.Hour)
	in.DedupHash = "h-ret"
	_, err := e.s.Messages().InsertInbound(e.ctx, in)
	e.ok(err)

	m, ok := e.acquire(ch, nil, T0)
	if !ok || m.ID != old1.ID {
		t.Fatalf("acquire = %+v", m)
	}
	e.ok(e.s.Messages().MarkSent(e.ctx, m.Lease(), T0))
	if _, err := e.s.Messages().Cancel(e.ctx, old2.ID); err != nil {
		t.Fatal(err)
	}

	n, err := e.s.Messages().PurgeBodies(e.ctx, T0.Add(-90*24*time.Hour))
	e.ok(err)
	if n != 3 { // sent, cancelled, received
		t.Fatalf("purged %d, want 3", n)
	}
	for id, wantBody := range map[domain.ID]bool{old1.ID: false, old2.ID: false, in.ID: false, oldQueued.ID: true, fresh.ID: true} {
		got, _ := e.s.Messages().Get(e.ctx, id)
		if (got.Body != "") != wantBody {
			t.Errorf("message %s body = %q, want kept=%v", id, got.Body, wantBody)
		}
		if got.Peer == "" {
			t.Errorf("metadata of %s lost", id)
		}
	}
	n, _ = e.s.Messages().PurgeBodies(e.ctx, T0.Add(-90*24*time.Hour))
	if n != 0 {
		t.Fatalf("second purge changed %d", n)
	}

	// Idempotency records.
	for i, at := range []time.Time{T0.Add(-48 * time.Hour), T0.Add(-time.Hour)} {
		msg := e.msg(fmt.Sprintf("+4860000010%d", i))
		msg.CreatedAt = at
		_, err := e.s.Messages().Enqueue(e.ctx, []domain.Message{msg}, store.Idempotency{Key: fmt.Sprint("k", i), RequestHash: "h", Since: at.Add(-time.Hour)})
		e.ok(err)
	}
	n, err = e.s.Messages().PurgeIdempotency(e.ctx, T0.Add(-24*time.Hour))
	e.ok(err)
	if n != 1 {
		t.Fatalf("purged %d idempotency records, want 1", n)
	}
	n, _ = e.s.Messages().PurgeIdempotency(e.ctx, T0.Add(-24*time.Hour))
	if n != 0 {
		t.Fatalf("second idempotency purge removed %d", n)
	}
}

func testWithTx(t *testing.T, f Factory) {
	e := newEnv(t, f)
	d := domain.Device{ID: e.id(), Name: "tx", Connector: domain.ConnectorRouterOSREST, BaseURL: "https://x",
		Username: "u", PasswordEnc: []byte("p"), TLSMode: domain.TLSVerify, Enabled: true}

	// Commit: several repositories, read-your-writes inside.
	err := e.s.WithTx(e.ctx, func(tx store.Repos) error {
		if err := tx.Devices().Create(e.ctx, d); err != nil {
			return err
		}
		if _, err := tx.Devices().Get(e.ctx, d.ID); err != nil {
			return err
		}
		return tx.Audit().Append(e.ctx, domain.AuditLog{ID: e.id(), Actor: domain.SystemActor, Action: domain.AuditDeviceCreate, At: T0})
	})
	e.ok(err)
	if _, err := e.s.Devices().Get(e.ctx, d.ID); err != nil {
		t.Fatalf("committed device missing: %v", err)
	}

	// Error: everything rolls back and the error is returned as is.
	sentinel := errors.New("boom")
	d2 := d
	d2.ID, d2.Name = e.id(), "tx2"
	err = e.s.WithTx(e.ctx, func(tx store.Repos) error {
		if err := tx.Devices().Create(e.ctx, d2); err != nil {
			return err
		}
		return sentinel
	})
	e.is(err, sentinel)
	if _, err := e.s.Devices().Get(e.ctx, d2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rolled back device visible: %v", err)
	}

	// Panic: rolled back and propagated; the store stays usable.
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Fatalf("recovered %v", r)
			}
		}()
		_ = e.s.WithTx(e.ctx, func(tx store.Repos) error {
			_ = tx.Devices().Create(e.ctx, d2)
			panic("kaboom")
		})
	}()
	if _, err := e.s.Devices().Get(e.ctx, d2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("panicked tx visible: %v", err)
	}
	e.ok(e.s.Devices().Create(e.ctx, d2))

	// A failing statement inside the tx surfaces its domain error.
	err = e.s.WithTx(e.ctx, func(tx store.Repos) error {
		return tx.Devices().Create(e.ctx, d2)
	})
	e.is(err, domain.ErrConflict)

	// Inbox batch in one transaction, with a duplicate inside.
	var created []bool
	h := "h-tx"
	err = e.s.WithTx(e.ctx, func(tx store.Repos) error {
		for i := 0; i < 2; i++ {
			c, err := tx.Messages().InsertInbound(e.ctx, domain.Message{ID: e.id(), Peer: "+48600000001", Body: "x",
				Encoding: domain.EncodingGSM7, Segments: 1, DedupHash: h, CreatedAt: T0})
			if err != nil {
				return err
			}
			created = append(created, c)
		}
		return nil
	})
	e.ok(err)
	if fmt.Sprint(created) != "[true false]" {
		t.Fatalf("created = %v", created)
	}
	e.ok(e.s.Ping(e.ctx))
}

func testConcurrency(t *testing.T, f Factory) {
	e := newEnv(t, f)
	const (
		messages = 60
		workers  = 8
	)
	ms := make([]domain.Message, messages)
	for i := range ms {
		ms[i] = e.msg(fmt.Sprintf("+4860002%04d", i))
	}
	e.enqueue(ms...)

	var mu sync.Mutex
	got := map[domain.ID]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		ch := e.id()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				m, ok, err := e.s.Messages().AcquireNext(e.ctx, store.AcquireRequest{
					ChannelID: ch, Now: T0, LeaseUntil: T0.Add(time.Minute)})
				if err != nil {
					t.Errorf("AcquireNext: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				got[m.ID]++
				mu.Unlock()
				if err := e.s.Messages().MarkSent(e.ctx, m.Lease(), T0); err != nil {
					t.Errorf("MarkSent: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(got) != messages {
		t.Fatalf("%d of %d messages acquired", len(got), messages)
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("message %s acquired %d times", id, n)
		}
	}
	if n, _ := e.s.Messages().Count(e.ctx, store.MessageFilter{Statuses: []domain.MessageStatus{domain.StatusSent}}); n != messages {
		t.Fatalf("sent = %d", n)
	}
}

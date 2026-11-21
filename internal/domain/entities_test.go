// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
)

const sampleID = ID("01JD5ZQ7Y1T9V3K8M2N4P6R8S0")

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func TestParseID(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{string(sampleID), true},
		{strings.ToLower(string(sampleID)), false}, // non-canonical
		{"", false},
		{"not-an-id", false},
		{"81JD5ZQ7Y1T9V3K8M2N4P6R8S0", false}, // overflow
	}
	for _, tt := range tests {
		id, err := ParseID(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("ParseID(%q) err = %v, want ok=%v", tt.in, err, tt.ok)
		}
		if tt.ok && (id.String() != tt.in || !id.Valid() || id.IsZero()) {
			t.Errorf("ParseID(%q) = %q", tt.in, id)
		}
	}
	if !ID("").IsZero() || ID("").Valid() {
		t.Error("empty ID must be zero and invalid")
	}
}

func TestULIDGenerator(t *testing.T) {
	clk := clock.NewFake(t0)
	g := NewULIDGenerator(clk)

	const n = 200
	var (
		mu  sync.Mutex
		ids []string
		wg  sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := g.NewID()
			mu.Lock()
			ids = append(ids, string(id))
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, s := range ids {
		if _, err := ParseID(s); err != nil {
			t.Fatalf("generated invalid ID %q", s)
		}
		if seen[s] {
			t.Fatalf("duplicate ID %q", s)
		}
		seen[s] = true
	}

	// IDs are time-ordered: a later clock yields a greater ID.
	before := g.NewID()
	clk.Advance(time.Millisecond)
	after := g.NewID()
	if before >= after {
		t.Fatalf("IDs not ordered: %s !< %s", before, after)
	}
	// Within the same millisecond they are still increasing (monotonic).
	a, b := g.NewID(), g.NewID()
	if a >= b {
		t.Fatalf("IDs not monotonic: %s !< %s", a, b)
	}
	// The first 10 characters encode the millisecond timestamp.
	sort.Strings(ids)
	if ids[0][:10] != string(before)[:10] {
		t.Fatalf("timestamp prefix differs: %s vs %s", ids[0], before)
	}
}

func TestActor(t *testing.T) {
	tests := []struct {
		actor Actor
		str   string
	}{
		{SystemActor, "system"},
		{UserActor(sampleID), "user:" + string(sampleID)},
		{TokenActor(sampleID), "token:" + string(sampleID)},
	}
	for _, tt := range tests {
		if got := tt.actor.String(); got != tt.str {
			t.Errorf("String() = %q, want %q", got, tt.str)
		}
		b, err := tt.actor.MarshalText()
		if err != nil || string(b) != tt.str {
			t.Errorf("MarshalText() = %q, %v", b, err)
		}
		var a Actor
		if err := a.UnmarshalText([]byte(tt.str)); err != nil || a != tt.actor {
			t.Errorf("UnmarshalText(%q) = %+v, %v", tt.str, a, err)
		}
		if tt.actor.IsZero() {
			t.Errorf("%v.IsZero() = true", tt.actor)
		}
	}
	if !(Actor{}).IsZero() {
		t.Error("zero Actor not IsZero")
	}
	// The zero actor (inbound messages) round-trips through "".
	if b, err := (Actor{}).MarshalText(); err != nil || len(b) != 0 {
		t.Errorf("zero MarshalText() = %q, %v", b, err)
	}
	if a, err := ParseActor(""); err != nil || !a.IsZero() {
		t.Errorf(`ParseActor("") = %+v, %v`, a, err)
	}

	for _, bad := range []string{"user", "user:", "user:bad", "admin:" + string(sampleID), "system:x"} {
		if _, err := ParseActor(bad); err == nil {
			t.Errorf("ParseActor(%q) succeeded", bad)
		}
		var a Actor
		if err := a.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("UnmarshalText(%q) succeeded", bad)
		}
	}
	if _, err := (Actor{Type: ActorUser, ID: "x"}).MarshalText(); err == nil {
		t.Error("invalid actor marshalled")
	}
}

func TestAPITokenExpired(t *testing.T) {
	tests := []struct {
		name    string
		expires time.Time
		want    bool
	}{
		{"never", time.Time{}, false},
		{"future", t0.Add(time.Second), false},
		{"exactly now", t0, true},
		{"past", t0.Add(-time.Second), true},
	}
	for _, tt := range tests {
		tok := APIToken{ExpiresAt: tt.expires}
		if got := tok.Expired(t0); got != tt.want {
			t.Errorf("%s: Expired = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestChannelHasTags(t *testing.T) {
	ch := Channel{Tags: []string{"pl", "otp", "primary"}}
	tests := []struct {
		tags []string
		want bool
	}{
		{nil, true},
		{[]string{"pl"}, true},
		{[]string{"pl", "otp"}, true},
		{[]string{"pl", "de"}, false},
		{[]string{"de"}, false},
	}
	for _, tt := range tests {
		if got := ch.HasTags(tt.tags); got != tt.want {
			t.Errorf("HasTags(%v) = %v, want %v", tt.tags, got, tt.want)
		}
	}
	if (Channel{}).HasTags([]string{"x"}) {
		t.Error("untagged channel matched a tag")
	}
}

func TestMessageLease(t *testing.T) {
	m := Message{ID: sampleID, ChannelID: "C", Attempts: 3, LeaseUntil: t0}
	want := Lease{MessageID: sampleID, ChannelID: "C", Attempt: 3, Until: t0}
	if got := m.Lease(); got != want {
		t.Fatalf("Lease() = %+v, want %+v", got, want)
	}
}

func TestInboundDedupHash(t *testing.T) {
	base := InboundDedupHash(sampleID, "lte1", "+48600000000", "nov/19/2026 12:00:00", "hello")
	if len(base) != 64 {
		t.Fatalf("hash length %d", len(base))
	}
	if again := InboundDedupHash(sampleID, "lte1", "+48600000000", "nov/19/2026 12:00:00", "hello"); again != base {
		t.Fatal("hash not deterministic")
	}
	variants := []string{
		InboundDedupHash("01JD5ZQ7Y1T9V3K8M2N4P6R8S1", "lte1", "+48600000000", "nov/19/2026 12:00:00", "hello"),
		InboundDedupHash(sampleID, "lte2", "+48600000000", "nov/19/2026 12:00:00", "hello"),
		InboundDedupHash(sampleID, "lte1", "+48600000001", "nov/19/2026 12:00:00", "hello"),
		InboundDedupHash(sampleID, "lte1", "+48600000000", "nov/19/2026 12:00:01", "hello"),
		InboundDedupHash(sampleID, "lte1", "+48600000000", "nov/19/2026 12:00:00", "hello!"),
		// Field boundaries matter: moving a separator must change the hash.
		InboundDedupHash(sampleID, "lte1|", "+48600000000", "nov/19/2026 12:00:00", "hello"),
	}
	for i, v := range variants {
		if v == base {
			t.Errorf("variant %d collides with base", i)
		}
	}
	if InboundDedupHash("", "a|b", "", "", "") == InboundDedupHash("", "a", "b", "", "") {
		t.Error("separator injection collides")
	}
}

func TestDeviceInsecureTransport(t *testing.T) {
	tests := []struct {
		url  string
		mode TLSMode
		want bool
	}{
		{"https://10.0.0.1", TLSVerify, false},
		{"HTTPS://10.0.0.1", TLSFingerprint, false},
		{"https://10.0.0.1", TLSCustomCA, false},
		{"https://10.0.0.1", TLSInsecure, true},
		{"http://172.17.0.1", TLSVerify, true},
		{"://bad", TLSVerify, true},
		{"", TLSVerify, true},
	}
	for _, tt := range tests {
		d := Device{BaseURL: tt.url, TLSMode: tt.mode}
		if got := d.InsecureTransport(); got != tt.want {
			t.Errorf("InsecureTransport(%q, %s) = %v, want %v", tt.url, tt.mode, got, tt.want)
		}
	}
}

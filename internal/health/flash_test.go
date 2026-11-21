// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

func TestQuantize(t *testing.T) {
	tests := []struct {
		name string
		prev *int
		raw  *int
		step int
		want *int
	}{
		{"nil stays nil", ip(-65), nil, 5, nil},
		{"first value is rounded", nil, ip(-67), 5, ip(-65)},
		{"first value rounds down", nil, ip(-68), 5, ip(-70)},
		{"rounds half away from zero", nil, ip(-5), 2, ip(-6)},
		{"positive", nil, ip(13), 2, ip(14)},
		{"inside the dead band", ip(-65), ip(-69), 5, ip(-65)},
		{"inside the dead band, above", ip(-65), ip(-61), 5, ip(-65)},
		{"leaves the dead band", ip(-65), ip(-70), 5, ip(-70)},
		{"leaves the dead band, above", ip(-65), ip(-60), 5, ip(-60)},
		{"dead band of a small step", ip(-10), ip(-11), 2, ip(-10)},
		{"small step moves at 2", ip(-10), ip(-12), 2, ip(-12)},
		{"step 1 is exact", ip(-65), ip(-66), 1, ip(-66)},
		{"step 0 is exact", nil, ip(-67), 0, ip(-67)},
		{"negative step is exact", nil, ip(-67), -3, ip(-67)},
		{"prev ignored when far", ip(0), ip(-67), 5, ip(-65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quantize(tt.prev, tt.raw, tt.step)
			if !equalInt(got, tt.want) {
				t.Fatalf("quantize = %v, want %v", deref(got), deref(tt.want))
			}
			if got != nil && tt.raw != nil && got == tt.raw {
				t.Fatal("result aliases the input")
			}
		})
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestQuantizationApply(t *testing.T) {
	q := Quantization{RSSI: 5, RSRP: 5, RSRQ: 2, SINR: 2}
	if q.apply(sig(1, 2, 3, 4), nil) != nil {
		t.Fatal("no raw signal must give nil")
	}
	if q.apply(nil, &domain.Signal{}) != nil {
		t.Fatal("an empty signal is no signal")
	}
	got := q.apply(nil, &domain.Signal{RSSI: ip(-67), SINR: ip(13)})
	if got == nil || *got.RSSI != -65 || *got.SINR != 14 || got.RSRP != nil || got.RSRQ != nil {
		t.Fatalf("partial signal: %+v", got)
	}
	prev := sig(-65, -95, -10, 12)
	got = q.apply(prev, sig(-68, -93, -11, 13))
	if !equalSignal(got, prev) {
		t.Fatalf("small wobble changed the signal: %+v", got)
	}
	if got == prev {
		t.Fatal("result aliases the previous signal")
	}
}

// dataVersion reads PRAGMA data_version through a second connection; it
// changes whenever another connection committed a write (SPEC §2.2).
type dataVersion struct {
	t  *testing.T
	db *sql.DB
}

func openObserver(t *testing.T, path string) dataVersion {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return dataVersion{t, db}
}

func (d dataVersion) get() int {
	d.t.Helper()
	var v int
	if err := d.db.QueryRow("PRAGMA data_version").Scan(&v); err != nil {
		d.t.Fatal(err)
	}
	return v
}

// fluctuating returns healthy snapshots whose metrics wobble around a fixed
// operating point.
func fluctuating() func(context.Context, int) (connector.ChannelHealth, error) {
	rssi := []int{-66, -67, -68, -67, -66, -67, -68, -66}
	return func(_ context.Context, n int) (connector.ChannelHealth, error) {
		h := goodHealth()
		w := rssi[n%len(rssi)]
		h.Signal = sig(w, -95+(w+67), -10+(n%3-1), 12+(n%3-1))
		return h, nil
	}
}

func TestNoWritesBetweenRefreshes(t *testing.T) {
	path := filePath(t)
	e := newEnv(t, envConfig{path: path})
	c := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(fluctuating()))})
	e.check(c.ID)
	obs := openObserver(t, path)
	before := obs.get()
	// 9 x 30 s = 4.5 min < RefreshInterval: zero writes, zero calls.
	for range 9 {
		e.clk.Advance(30 * time.Second)
		e.check(c.ID)
	}
	if after := obs.get(); after != before {
		t.Fatalf("data_version %d -> %d: steady state wrote to the database", before, after)
	}
	if got := e.repo.count(); got != 1 {
		t.Fatalf("UpdateHealth called %d times", got)
	}
	if got := len(e.disp.all()); got != 0 {
		t.Fatalf("dispatcher called %d times", got)
	}

	// The refresh happens exactly once the interval is over.
	e.clk.Advance(DefaultRefreshInterval - 4*time.Minute - 30*time.Second - time.Second)
	e.check(c.ID)
	if got := e.repo.count(); got != 1 {
		t.Fatalf("refreshed %v too early", got)
	}
	e.clk.Advance(time.Second)
	e.check(c.ID)
	if got := e.repo.count(); got != 2 {
		t.Fatalf("UpdateHealth calls = %d, want the refresh", got)
	}
	if after := obs.get(); after != before+1 {
		t.Fatalf("data_version %d -> %d", before, after)
	}
	if got := e.stored(c.ID).LastHealthAt; !got.Equal(e.clk.Now()) {
		t.Fatalf("last_health_at = %v", got)
	}
}

func TestWritesOnRealChanges(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	cur := goodHealth()
	conn := newStub(func(context.Context, int) (connector.ChannelHealth, error) { return cur, nil })
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.check(c.ID)
	n := e.repo.count()

	step := func(name string, mod func(*connector.ChannelHealth), wantWrite bool) {
		t.Helper()
		cur = goodHealth()
		mod(&cur)
		e.clk.Advance(time.Second)
		e.check(c.ID)
		wrote := e.repo.count() > n
		n = e.repo.count()
		if wrote != wantWrite {
			t.Fatalf("%s: wrote = %v, want %v", name, wrote, wantWrite)
		}
	}
	step("same", func(*connector.ChannelHealth) {}, false)
	step("rssi within the band", func(h *connector.ChannelHealth) { h.Signal.RSSI = ip(-69) }, false)
	step("rssi moved a step", func(h *connector.ChannelHealth) { h.Signal.RSSI = ip(-75) }, true)
	step("rsrq moved", func(h *connector.ChannelHealth) { h.Signal.RSSI = ip(-75); h.Signal.RSRQ = ip(-14) }, true)
	step("operator changed", func(h *connector.ChannelHealth) {
		h.Signal.RSSI = ip(-75)
		h.Signal.RSRQ = ip(-14)
		h.Operator = "Play"
	}, true)
	step("signal lost", func(h *connector.ChannelHealth) { h.Operator = "Play"; h.Signal = nil }, true)
	step("technology is not stored", func(h *connector.ChannelHealth) { h.Operator = "Play"; h.Technology = "5G NSA" }, true) // signal is back
	step("technology alone", func(h *connector.ChannelHealth) { h.Operator = "Play"; h.Technology = "LTE" }, false)

	// A missing operator keeps the last one: no flapping writes.
	step("operator not reported", func(h *connector.ChannelHealth) { h.Operator = "" }, false)
	if got := e.status(c.ID).Operator; got != "Play" {
		t.Fatalf("operator = %q", got)
	}
}

func TestQuantizationIsConfigurable(t *testing.T) {
	e := newEnv(t, envConfig{opts: func(o *Options) {
		o.Quantization = Quantization{RSSI: 1, RSRP: 10, RSRQ: 1, SINR: 1}
	}})
	c := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(nil))})
	st := e.check(c.ID)
	want := sig(-67, -100, -10, 12) // -95 rounds to -100 with a 10 dB step
	if !equalSignal(st.Signal, want) {
		t.Fatalf("signal %+v", st.Signal)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"abcdef", 3, "abc"},
		{"zażółć", 3, "za"}, // not in the middle of "ż"
		{"zażółć", 4, "zaż"},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

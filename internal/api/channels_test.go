// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
)

func TestChannelCRUD(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("Biuro")

	res := e.req(http.MethodPost, channelsPath, e.adminTok, map[string]any{"device_id": dev, "port": "lte1", "tags": []string{"orange", "bulk"}})
	if res.Status != http.StatusCreated {
		t.Fatal(res)
	}
	c := res.obj()
	id := c["id"].(string)
	want := map[string]any{
		"device_id": dev, "port": "lte1", "name": "Biuro/lte1", "tags": []any{"orange", "bulk"}, "rate_per_min": float64(6),
		"max_segments": float64(3), "transliterate": false, "enabled": true, "state": "down", "state_reason": "not checked yet",
		"last_health_at": nil, "signal": nil, "operator": nil, "queue_depth": float64(0),
	}
	for k, v := range want {
		if !reflect.DeepEqual(c[k], v) {
			t.Errorf("%s = %#v, want %#v", k, c[k], v)
		}
	}
	if res.Header.Get("Location") != "/api/v1/channels/"+id {
		t.Errorf("Location = %q", res.Header.Get("Location"))
	}
	// The same (device, port) twice.
	e.req(http.MethodPost, channelsPath, e.adminTok, map[string]any{"device_id": dev, "port": "lte1"}).problem(409, problemConflict)

	// Health arrives: signal and operator are shown.
	three := -71
	err := e.st.Channels().UpdateHealth(e.ctx, domain.ID(id), healthWithSignal(e.clk.Now(), &three))
	if err != nil {
		t.Fatal(err)
	}
	got := e.get(channelsPath+"/"+id, e.readTok).obj()
	if got["state"] != "ok" || got["operator"] != "Orange PL" || got["last_health_at"] == nil {
		t.Errorf("channel = %v", got)
	}
	if sig := got["signal"].(map[string]any); sig["rssi"] != float64(-71) || sig["rsrp"] != nil {
		t.Errorf("signal = %v", sig)
	}

	// Patch.
	res = e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, map[string]any{"rate_per_min": 3, "enabled": false, "tags": []string{}})
	if res.Status != 200 {
		t.Fatal(res)
	}
	c = res.obj()
	if c["rate_per_min"] != float64(3) || c["enabled"] != false || c["state"] != "disabled" || c["state_reason"] != "disabled" ||
		c["name"] != "Biuro/lte1" || c["max_segments"] != float64(3) || len(c["tags"].([]any)) != 0 {
		t.Errorf("patched = %v", c)
	}
	// A no-op patch changes (and reloads) nothing.
	before := e.reloader.count()
	e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, map[string]any{"rate_per_min": 3})
	if e.reloader.count() != before {
		t.Error("no-op patch reloaded channels")
	}
	e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, map[string]any{"name": "Nowa", "transliterate": true, "max_segments": 1, "enabled": true})
	if c := e.get(channelsPath+"/"+id, e.readTok).obj(); c["name"] != "Nowa" || c["transliterate"] != true || c["state"] != "ok" {
		t.Errorf("after patch = %v", c)
	}

	// Patch errors.
	for name, body := range map[string]string{
		"empty": `{}`, "null": `{"name": null}`, "blank name": `{"name": ""}`, "long name": `{"name": "` + strings.Repeat("n", 65) + `"}`,
		"rate low": `{"rate_per_min": 0}`, "rate high": `{"rate_per_min": 601}`, "segments": `{"max_segments": 11}`,
		"bad tags": `{"tags": ["Bad"]}`,
	} {
		t.Run(name, func(_ *testing.T) {
			e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, body).problem(422, problemValidation)
		})
	}
	e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, `{"port": "lte2"}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, `{"rate_per_min": "6"}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, channelsPath+"/"+id, e.adminTok, `{"tags": "a"}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, channelsPath+"/nope", e.adminTok, `{"name": "x"}`).problem(404, problemNotFound)

	// Delete.
	if r := e.req(http.MethodDelete, channelsPath+"/"+id, e.adminTok, nil); r.Status != 204 {
		t.Fatal(r)
	}
	e.get(channelsPath+"/"+id, e.readTok).problem(404, problemNotFound)
}

func TestChannelCreateValidation(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("d")
	tests := []struct {
		name  string
		body  any
		codes []string
	}{
		{"nothing", map[string]any{}, []string{"/device_id:required", "/port:required"}},
		{"unknown device", map[string]any{"device_id": "nope", "port": "lte1"}, []string{"/device_id:unknown_device"}},
		{"long device id", map[string]any{"device_id": strings.Repeat("d", 65), "port": "lte1"}, []string{"/device_id:out_of_range"}},
		{"bad port", map[string]any{"device_id": dev, "port": "lte 1"}, []string{"/port:invalid_format"}},
		{"long port", map[string]any{"device_id": dev, "port": strings.Repeat("p", 33)}, []string{"/port:out_of_range"}},
		{"long name", map[string]any{"device_id": dev, "port": "p", "name": strings.Repeat("n", 65)}, []string{"/name:out_of_range"}},
		{"rate", map[string]any{"device_id": dev, "port": "p", "rate_per_min": 601}, []string{"/rate_per_min:out_of_range"}},
		{"segments", map[string]any{"device_id": dev, "port": "p", "max_segments": 0}, []string{"/max_segments:out_of_range"}},
		{"too many tags", map[string]any{"device_id": dev, "port": "p", "tags": manyTags(17)}, []string{"/tags:out_of_range"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := e.req(http.MethodPost, channelsPath, e.adminTok, tc.body).problem(422, problemValidation)
			if got := problemCodes(p); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("errors = %v, want %v", got, tc.codes)
			}
		})
	}
	e.req(http.MethodPost, channelsPath, e.adminTok, map[string]any{"device_id": dev, "port": "p", "bogus": 1}).problem(400, problemMalformed)
	// A long default name is cut to the limit.
	long := e.addDevice(strings.Repeat("x", 60))
	res := e.req(http.MethodPost, channelsPath, e.adminTok, map[string]any{"device_id": long, "port": "lte-long-port"})
	if n := res.obj()["name"].(string); len([]rune(n)) != 64 {
		t.Errorf("name = %q", n)
	}
}

func manyTags(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strings.Repeat("x", i)
	}
	return out
}

func TestListChannels(t *testing.T) {
	e := newEnv(t)
	d1, d2 := e.addDevice("one"), e.addDevice("two")
	c1 := e.addChannel(d1, "lte1", nil)
	c2 := e.addChannel(d1, "lte2", map[string]any{"enabled": false})
	c3 := e.addChannel(d2, "lte1", nil)
	// Two queued messages on c1.
	for i := 0; i < 2; i++ {
		e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "q", "channel_id": c1})
	}

	list := func(q string) []string {
		t.Helper()
		res := e.get(channelsPath+q, e.readTok)
		if res.Status != 200 {
			t.Fatalf("%s: %s", q, res)
		}
		var ids []string
		for _, it := range res.obj()["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		return ids
	}
	if got := list(""); !reflect.DeepEqual(got, []string{c1, c2, c3}) {
		t.Errorf("all = %v", got)
	}
	if got := list("?device_id=" + d1); !reflect.DeepEqual(got, []string{c1, c2}) {
		t.Errorf("device = %v", got)
	}
	if got := list("?state=disabled"); !reflect.DeepEqual(got, []string{c2}) {
		t.Errorf("disabled = %v", got)
	}
	if got := list("?state=ok"); !reflect.DeepEqual(got, []string{c1, c3}) {
		t.Errorf("ok = %v", got)
	}
	if got := list("?state=down"); len(got) != 0 {
		t.Errorf("down = %v", got)
	}
	if got := list("?limit=2"); len(got) != 2 {
		t.Errorf("limit = %v", got)
	}
	first := e.get(channelsPath+"?limit=2", e.readTok).obj()
	if got := list("?limit=2&cursor=" + first["next_cursor"].(string)); !reflect.DeepEqual(got, []string{c3}) {
		t.Errorf("second page = %v", got)
	}
	if d := e.get(channelsPath+"/"+c1, e.readTok).obj()["queue_depth"]; d != float64(2) {
		t.Errorf("queue_depth = %v", d)
	}
	e.get(channelsPath+"?state=weird", e.readTok).problem(400, problemMalformed)
	e.get(channelsPath+"?bogus=1", e.readTok).problem(400, problemMalformed)
	e.get(channelsPath+"/"+strings.Repeat("x", 65), e.readTok).problem(400, problemMalformed)
}

// stubHealth overlays a fixed state, like health.Checker.ApplyTo.
type stubHealth struct{ state domain.ChannelState }

func (s stubHealth) ApplyTo(c *domain.Channel) bool {
	c.State, c.StateReason = s.state, "live"
	return true
}

func TestChannelHealthOverlay(t *testing.T) {
	e := newEnv(t)
	ch := e.addChannel(e.addDevice("d"), "lte1", nil)
	e.api.hv = stubHealth{state: domain.ChannelDegraded}
	c := e.get(channelsPath+"/"+ch, e.readTok).obj()
	if c["state"] != "degraded" || c["state_reason"] != "live" {
		t.Errorf("channel = %v", c)
	}
	if got := e.get(channelsPath+"?state=degraded", e.readTok).obj()["items"].([]any); len(got) != 1 {
		t.Errorf("filter by live state: %v", got)
	}
}

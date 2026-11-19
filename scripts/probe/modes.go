// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// App zbiera zależności i parametry trybów pracy.
type App struct {
	c    *Client
	rec  *Recorder
	anon *Anonymizer
	out  io.Writer

	port           string
	to             string
	message        string
	noStatusReport bool
	yes            bool
	gap            time.Duration
	only           string
	count          int
	parallel       int
	id             string
	policies       string
}

type apiCall struct {
	label, method, path string
	body                any
}

func (a *App) printResult(r *Result) {
	status := fmt.Sprintf("%d", r.Status)
	if r.Err != nil {
		status = "BŁĄD: " + a.anon.String(r.Err.Error())
	}
	fmt.Fprintf(a.out, "  #%03d %-28s -> %s (%d ms)\n", r.Seq, r.Label, status, r.Duration.Milliseconds())
}

func (a *App) run(ctx context.Context, c apiCall) *Result {
	r := a.c.Do(ctx, c.label, c.method, c.path, c.body)
	a.printResult(r)
	return r
}

func (a *App) needYes(what string) error {
	if a.yes {
		return nil
	}
	return fmt.Errorf("ten tryb wysyła prawdziwe SMS-y (%s) i może generować koszty; dodaj --yes, aby potwierdzić", what)
}

func (a *App) needTo() error {
	if a.to == "" {
		return fmt.Errorf("podaj numer testowy (-to lub PROBE_TO)")
	}
	return nil
}

func (a *App) sendBody(text string) map[string]any {
	b := map[string]any{"port": a.port, "phone-number": a.to, "message": text}
	if a.noStatusReport {
		b["status-report-request"] = "no"
	}
	return b
}

// jsonObjects dekoduje odpowiedź REST (lista lub pojedynczy obiekt) do listy map.
func jsonObjects(body []byte) []map[string]any {
	var list []map[string]any
	if json.Unmarshal(body, &list) == nil {
		return list
	}
	var one map[string]any
	if json.Unmarshal(body, &one) == nil {
		return []map[string]any{one}
	}
	return nil
}

func lteCalls(port string) []apiCall {
	return []apiCall{
		{"lte-list", "GET", "/rest/interface/lte", nil},
		{"lte-monitor", "POST", "/rest/interface/lte/monitor", map[string]any{"numbers": port, "once": ""}},
		{"lte-info", "POST", "/rest/interface/lte/info", map[string]any{"numbers": port, "once": ""}},
	}
}

// ModeInfo zbiera informacje o routerze, modemie i konfiguracji SMS/REST/kontenerów.
func (a *App) ModeInfo(ctx context.Context) (map[string]any, error) {
	calls := []apiCall{
		{"system-resource", "GET", "/rest/system/resource", nil},
		{"system-routerboard", "GET", "/rest/system/routerboard", nil},
		{"system-clock", "GET", "/rest/system/clock", nil},
		{"system-package", "GET", "/rest/system/package", nil},
		{"device-mode", "GET", "/rest/system/device-mode", nil},
		{"container-config", "GET", "/rest/container/config", nil},
		{"ip-service", "GET", "/rest/ip/service", nil},
		{"user-group", "GET", "/rest/user/group", nil},
		{"port-list", "GET", "/rest/port", nil},
		{"sms-settings", "GET", "/rest/tool/sms", nil},
	}
	calls = append(calls, lteCalls(a.port)...)
	extra := map[string]any{}
	for _, c := range calls {
		r := a.run(ctx, c)
		if c.label == "system-resource" && r.OK() {
			if objs := jsonObjects(r.Body); len(objs) > 0 {
				router := map[string]any{}
				for _, k := range []string{"version", "architecture-name", "board-name", "platform", "total-memory", "free-memory", "cpu", "cpu-count", "total-hdd-space", "free-hdd-space"} {
					if v, ok := objs[0][k]; ok {
						router[k] = v
					}
				}
				extra["router"] = router
			}
		}
	}
	return extra, nil
}

// ModeInbox odczytuje skrzynkę odbiorczą i opisuje format pól.
func (a *App) ModeInbox(ctx context.Context) (map[string]any, error) {
	a.run(ctx, apiCall{"sms-settings", "GET", "/rest/tool/sms", nil})
	r := a.run(ctx, apiCall{"inbox-get", "GET", "/rest/tool/sms/inbox", nil})
	a.run(ctx, apiCall{"inbox-count", "POST", "/rest/tool/sms/inbox/print", map[string]any{"count-only": ""}})
	extra := map[string]any{}
	if r.OK() {
		objs := jsonObjects(r.Body)
		fields := map[string]bool{}
		for _, o := range objs {
			for k := range o {
				fields[k] = true
			}
		}
		names := make([]string, 0, len(fields))
		for k := range fields {
			names = append(names, k)
		}
		sort.Strings(names)
		extra["inbox_items"] = len(objs)
		extra["inbox_fields"] = names
		extra["note"] = "treść pól message i pdu jest zasłonięta (tylko długości); użyj -keep-inbox-bodies, by ją zachować"
	}
	return extra, nil
}

// ModeInboxDelete sprawdza, który wariant usuwania wpisu ze skrzynki działa.
func (a *App) ModeInboxDelete(ctx context.Context) (map[string]any, error) {
	if err := a.needYes("usuwa wpis ze skrzynki routera"); err != nil {
		return nil, err
	}
	if a.id == "" {
		return nil, fmt.Errorf("podaj identyfikator wpisu (-id, np. *1; zobacz tryb inbox)")
	}
	extra := map[string]any{}
	r := a.run(ctx, apiCall{"inbox-delete-rest", "DELETE", "/rest/tool/sms/inbox/" + url.PathEscape(a.id), nil})
	extra["delete_rest_ok"] = r.OK()
	if !r.OK() {
		r = a.run(ctx, apiCall{"inbox-remove-post", "POST", "/rest/tool/sms/inbox/remove", map[string]any{".id": a.id}})
		extra["remove_post_ok"] = r.OK()
	}
	a.run(ctx, apiCall{"inbox-after", "GET", "/rest/tool/sms/inbox", nil})
	return extra, nil
}

// ModeSend wysyła jedną wiadomość.
func (a *App) ModeSend(ctx context.Context) (map[string]any, error) {
	if err := a.needTo(); err != nil {
		return nil, err
	}
	if err := a.needYes("1 wiadomość"); err != nil {
		return nil, err
	}
	r := a.run(ctx, apiCall{"send", "POST", "/rest/tool/sms/send", a.sendBody(a.message)})
	a.run(ctx, apiCall{"sms-settings-after", "GET", "/rest/tool/sms", nil})
	return map[string]any{"text": a.message, "analysis": AnalyzeSMS(a.message), "ok": r.OK()}, nil
}

// ModeSendSuite wysyła zestaw przypadków testowych.
func (a *App) ModeSendSuite(ctx context.Context) (map[string]any, error) {
	if err := a.needTo(); err != nil {
		return nil, err
	}
	cases := a.selectCases()
	if len(cases) == 0 {
		return nil, fmt.Errorf("brak przypadków pasujących do -only %q", a.only)
	}
	total := 0
	for _, c := range cases {
		total += AnalyzeSMS(c.Text).Segments
	}
	if err := a.needYes(fmt.Sprintf("%d wiadomości, łącznie ok. %d segmentów", len(cases), total)); err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(cases))
	for i, c := range cases {
		if i > 0 && a.gap > 0 {
			select {
			case <-ctx.Done():
				return map[string]any{"cases": results}, ctx.Err()
			case <-time.After(a.gap):
			}
		}
		an := AnalyzeSMS(c.Text)
		fmt.Fprintf(a.out, "przypadek %s: %d znaków, %s, %d jedn., oczekiwane segmenty %d\n", c.Name, an.Runes, an.Encoding, an.Units, an.Segments)
		r := a.run(ctx, apiCall{"send-" + c.Name, "POST", "/rest/tool/sms/send", a.sendBody(c.Text)})
		results = append(results, map[string]any{
			"name": c.Name, "desc": c.Desc, "text": c.Text, "analysis": an,
			"status": r.Status, "ok": r.OK(), "duration_ms": r.Duration.Milliseconds(),
			"response": a.anon.Body(r.Body, false),
		})
		if ctx.Err() != nil {
			break
		}
	}
	a.run(ctx, apiCall{"sms-settings-after", "GET", "/rest/tool/sms", nil})
	return map[string]any{"cases": results}, nil
}

func (a *App) selectCases() []Case {
	all := SuiteCases()
	if a.only == "" {
		return all
	}
	want := map[string]bool{}
	for _, n := range strings.Split(a.only, ",") {
		want[strings.TrimSpace(n)] = true
	}
	var out []Case
	for _, c := range all {
		if want[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// ModeThroughput wysyła -count krótkich wiadomości i mierzy opóźnienia.
func (a *App) ModeThroughput(ctx context.Context) (map[string]any, error) {
	if err := a.needTo(); err != nil {
		return nil, err
	}
	if a.count < 1 {
		return nil, fmt.Errorf("-count musi być dodatnie")
	}
	if a.parallel < 1 {
		a.parallel = 1
	}
	if err := a.needYes(fmt.Sprintf("%d wiadomości, równoległość %d", a.count, a.parallel)); err != nil {
		return nil, err
	}
	jobs := make(chan int)
	var mu sync.Mutex
	var lat []float64
	statuses := map[int]int{}
	var okN int
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < a.parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				txt := fmt.Sprintf("tikker probe throughput %d of %d", i, a.count)
				r := a.run(ctx, apiCall{fmt.Sprintf("tp-%03d", i), "POST", "/rest/tool/sms/send", a.sendBody(txt)})
				mu.Lock()
				statuses[r.Status]++
				if r.OK() {
					okN++
					lat = append(lat, float64(r.Duration.Microseconds())/1000)
				}
				mu.Unlock()
			}
		}()
	}
loop:
	for i := 1; i <= a.count; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	extra := map[string]any{
		"count": a.count, "parallel": a.parallel, "ok": okN,
		"status_counts": statuses, "total_ms": elapsed.Milliseconds(),
	}
	if elapsed > 0 {
		extra["ok_per_minute"] = float64(okN) / elapsed.Minutes()
	}
	if len(lat) > 0 {
		sort.Float64s(lat)
		var sum float64
		for _, v := range lat {
			sum += v
		}
		extra["latency_ms"] = map[string]any{
			"min": lat[0], "median": lat[len(lat)/2], "max": lat[len(lat)-1], "avg": sum / float64(len(lat)),
		}
	}
	fmt.Fprintf(a.out, "wysłano poprawnie %d/%d w %s\n", okN, a.count, elapsed.Round(time.Millisecond))
	return extra, nil
}

// ModePolicies wykonuje stały zestaw wywołań i zapisuje kody odpowiedzi.
// Uruchamiaj wielokrotnie, za każdym razem dla użytkownika z innym zestawem
// polityk grupy; -policies służy tylko jako opis w wynikach.
func (a *App) ModePolicies(ctx context.Context) (map[string]any, error) {
	calls := []apiCall{
		{"get-resource", "GET", "/rest/system/resource", nil},
		{"get-sms-settings", "GET", "/rest/tool/sms", nil},
		{"get-inbox", "GET", "/rest/tool/sms/inbox", nil},
		{"get-lte", "GET", "/rest/interface/lte", nil},
		{"post-lte-monitor", "POST", "/rest/interface/lte/monitor", map[string]any{"numbers": a.port, "once": ""}},
		{"post-ping-loopback", "POST", "/rest/ping", map[string]any{"address": "127.0.0.1", "count": "1"}},
	}
	matrix := map[string]any{}
	for _, c := range calls {
		r := a.run(ctx, c)
		matrix[c.label] = map[string]any{"status": r.Status, "ok": r.OK(), "body": a.anon.Body(r.Body, true)}
	}
	if a.yes && a.to != "" {
		r := a.run(ctx, apiCall{"post-sms-send", "POST", "/rest/tool/sms/send", a.sendBody("tikker probe policies")})
		matrix["post-sms-send"] = map[string]any{"status": r.Status, "ok": r.OK(), "body": a.anon.Body(r.Body, false)}
	} else {
		fmt.Fprintln(a.out, "  (pominięto wysyłkę SMS; dodaj -to i --yes, aby sprawdzić także /tool/sms/send)")
	}
	return map[string]any{"policies_declared": a.policies, "matrix": matrix}, nil
}

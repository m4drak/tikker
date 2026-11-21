// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testUser  = "probe"
	testPass  = "tajne-haslo-123"
	testPhone = "+48600700800"
)

// fakeRouterOS udaje podzbiór REST API RouterOS.
type fakeRouterOS struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (f *fakeRouterOS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != testUser || p != testPass {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":401,"message":"Unauthorized"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/rest/system/resource":
		_, _ = w.Write([]byte(`{"version":"7.16.2 (stable)","architecture-name":"arm","board-name":"RB912","total-memory":"67108864","uptime":"1d"}`))
	case r.Method == "GET" && r.URL.Path == "/rest/tool/sms/inbox":
		_, _ = w.Write([]byte(`[{".id":"*1","phone":"+48511222333","timestamp":"2026-11-19 12:00:00","message":"Prywatna treść","type":"","pdu":"0791448720"}]`))
	case r.Method == "POST" && r.URL.Path == "/rest/tool/sms/send":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["phone-number"] == nil || body["port"] == nil {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"missing"}`))
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/rest/tool/sms/inbox/"):
		_, _ = w.Write([]byte(`[]`))
	case r.URL.Path == "/rest/interface/lte/info":
		_, _ = w.Write([]byte(`{"imei":"356938035643809","current-operator":"Test","uicc":"8948010000012345678","wan":"10.64.1.2"}`))
	default:
		_, _ = w.Write([]byte(`[]`))
	}
}

func (f *fakeRouterOS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func baseEnv(url string) func(string) string {
	m := map[string]string{
		"PROBE_URL": url, "PROBE_USER": testUser, "PROBE_PASSWORD": testPass,
		"PROBE_TO": testPhone,
	}
	return func(k string) string { return m[k] }
}

func runProbe(t *testing.T, env func(string) string, args ...string) (int, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	var out, errb bytes.Buffer
	args = append([]string{"-out", dir, "-insecure", "-gap", "0"}, args...)
	code := run(context.Background(), args, env, &out, &errb)
	return code, out.String(), errb.String(), dir
}

func readAll(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		sb.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func assertNoLeaks(t *testing.T, text string, extra ...string) {
	t.Helper()
	leaks := append([]string{testPass, "600700800", "511222333", "356938035643809", "8948010000012345678", "10.64.1.2", "127.0.0.1:", "Basic "}, extra...)
	for _, l := range leaks {
		if strings.Contains(text, l) {
			t.Errorf("wyniki zawierają wrażliwą wartość %q", l)
		}
	}
}

func TestInfoAgainstFake(t *testing.T) {
	f := &fakeRouterOS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errs, dir := runProbe(t, baseEnv(srv.URL), "info")
	if code != 0 {
		t.Fatalf("kod %d, stderr: %s", code, errs)
	}
	if !strings.Contains(out, "system-resource") {
		t.Errorf("brak wyjścia: %s", out)
	}
	all := readAll(t, dir)
	assertNoLeaks(t, all)
	if !strings.Contains(all, "7.16.2") || !strings.Contains(all, "<REDACTED>") {
		t.Errorf("niewrażliwe dane powinny zostać, nagłówek Authorization zasłonięty")
	}
	var sum map[string]any
	files, _ := filepath.Glob(filepath.Join(dir, "*", "summary.json"))
	if len(files) != 1 {
		t.Fatalf("summary: %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatal(err)
	}
	router, _ := sum["router"].(map[string]any)
	if router["architecture-name"] != "arm" {
		t.Errorf("router: %v", sum["router"])
	}
}

func TestInboxRedactsBodiesByDefault(t *testing.T) {
	srv := httptest.NewServer(&fakeRouterOS{})
	defer srv.Close()
	code, _, errs, dir := runProbe(t, baseEnv(srv.URL), "inbox")
	if code != 0 {
		t.Fatalf("kod %d: %s", code, errs)
	}
	all := readAll(t, dir)
	assertNoLeaks(t, all)
	if strings.Contains(all, "Prywatna treść") || strings.Contains(all, "0791448720") {
		t.Error("treść i PDU wiadomości powinny być zasłonięte")
	}
	code, _, _, dir = runProbe(t, baseEnv(srv.URL), "-keep-inbox-bodies", "inbox")
	if code != 0 {
		t.Fatal(code)
	}
	if all = readAll(t, dir); !strings.Contains(all, "Prywatna treść") {
		t.Error("-keep-inbox-bodies powinno zachować treść")
	}
}

func TestSendRequiresYes(t *testing.T) {
	f := &fakeRouterOS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	for _, mode := range []string{"send", "send-suite", "throughput"} {
		code, _, errs, _ := runProbe(t, baseEnv(srv.URL), mode)
		if code == 0 || !strings.Contains(errs, "--yes") {
			t.Errorf("%s bez --yes: kod %d, %q", mode, code, errs)
		}
	}
	if f.count() != 0 {
		t.Fatalf("wysłano %d wiadomości bez potwierdzenia", f.count())
	}
}

func TestSendSuiteAgainstFake(t *testing.T) {
	f := &fakeRouterOS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, _, errs, dir := runProbe(t, baseEnv(srv.URL), "-yes", "send-suite")
	if code != 0 {
		t.Fatalf("kod %d: %s", code, errs)
	}
	if f.count() != len(SuiteCases()) {
		t.Fatalf("wysłano %d, oczekiwano %d", f.count(), len(SuiteCases()))
	}
	// router dostał prawdziwy numer, wyniki go nie zawierają
	if f.sent[0]["phone-number"] != testPhone {
		t.Errorf("numer: %v", f.sent[0]["phone-number"])
	}
	all := readAll(t, dir)
	assertNoLeaks(t, all)
	if !strings.Contains(all, "Zażółć gęślą jaźń") || !strings.Contains(all, "😀") {
		t.Error("treść przypadków testowych (UTF-8) powinna być zachowana w wynikach")
	}
	if !strings.Contains(all, `"encoding": "ucs2"`) {
		t.Error("brak analizy kodowania")
	}
}

func TestSendSuiteOnly(t *testing.T) {
	f := &fakeRouterOS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, _, errs, _ := runProbe(t, baseEnv(srv.URL), "-yes", "-only", "ascii-160,pl-70", "send-suite")
	if code != 0 || f.count() != 2 {
		t.Fatalf("kod %d, wysłano %d: %s", code, f.count(), errs)
	}
}

func TestThroughputParallel(t *testing.T) {
	f := &fakeRouterOS{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	code, out, errs, dir := runProbe(t, baseEnv(srv.URL), "-yes", "-count", "6", "-parallel", "3", "throughput")
	if code != 0 {
		t.Fatalf("kod %d: %s", code, errs)
	}
	if f.count() != 6 || !strings.Contains(out, "6/6") {
		t.Errorf("wysłano %d, wyjście %q", f.count(), out)
	}
	assertNoLeaks(t, readAll(t, dir))
}

func TestPoliciesRecordsDenials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/tool/sms") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":500,"message":"Internal Server Error","detail":"not enough permissions (9)"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	code, _, errs, dir := runProbe(t, baseEnv(srv.URL), "-policies", "api,rest-api,read", "policies")
	if code != 0 {
		t.Fatalf("kod %d: %s", code, errs)
	}
	all := readAll(t, dir)
	if !strings.Contains(all, "not enough permissions (9)") || !strings.Contains(all, "api,rest-api,read") {
		t.Error("macierz polityk powinna zawierać kody i opis")
	}
}

func TestInboxDeleteFallsBackToPost(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == "DELETE" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":400,"message":"Bad Request"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	code, _, errs, _ := runProbe(t, baseEnv(srv.URL), "-yes", "-id", "*1", "inbox-delete")
	if code != 0 {
		t.Fatalf("kod %d: %s", code, errs)
	}
	if len(seen) < 2 || seen[0] != "DELETE /rest/tool/sms/inbox/*1" || seen[1] != "POST /rest/tool/sms/inbox/remove" {
		t.Errorf("sekwencja: %v", seen)
	}
}

func TestBadAuthIsRecordedNotFatal(t *testing.T) {
	srv := httptest.NewServer(&fakeRouterOS{})
	defer srv.Close()
	env := func(k string) string {
		if k == "PROBE_PASSWORD" {
			return "zle-haslo-xyz"
		}
		return baseEnv(srv.URL)(k)
	}
	code, out, _, dir := runProbe(t, env, "info")
	if code != 0 || !strings.Contains(out, "401") {
		t.Errorf("kod %d, wyjście %q", code, out)
	}
	assertNoLeaks(t, readAll(t, dir), "zle-haslo-xyz")
}

func TestTLSFingerprint(t *testing.T) {
	srv := httptest.NewUnstartedServer(&fakeRouterOS{})
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	good := hex.EncodeToString(sum[:])

	run1 := func(extra ...string) int {
		dir := t.TempDir()
		var o, e bytes.Buffer
		args := append([]string{"-out", dir}, extra...)
		args = append(args, "policies")
		return run(context.Background(), args, baseEnv(srv.URL), &o, &e)
	}
	if code := run1("-fingerprint", good); code != 0 {
		t.Errorf("poprawny odcisk: kod %d", code)
	}
	dir := t.TempDir()
	var o, e bytes.Buffer
	run(context.Background(), []string{"-out", dir, "-fingerprint", strings.Repeat("00", 32), "info"}, baseEnv(srv.URL), &o, &e)
	if !strings.Contains(o.String(), "BŁĄD") {
		t.Errorf("zły odcisk powinien zakończyć się błędem połączenia: %s", o.String())
	}
	// bez opcji TLS certyfikat testowy nie jest zaufany
	o.Reset()
	run(context.Background(), []string{"-out", dir, "info"}, baseEnv(srv.URL), &o, &e)
	if !strings.Contains(o.String(), "BŁĄD") {
		t.Errorf("brak zaufania do certyfikatu powinien dać błąd: %s", o.String())
	}
}

func TestArgumentValidation(t *testing.T) {
	env := func(string) string { return "" }
	var o, e bytes.Buffer
	if code := run(context.Background(), []string{"info"}, env, &o, &e); code != 2 {
		t.Errorf("brak url/user: kod %d", code)
	}
	if code := run(context.Background(), []string{"-url", "ftp://x", "-user", "u", "info"}, env, &o, &e); code != 2 {
		t.Errorf("zły schemat: kod %d", code)
	}
	if code := run(context.Background(), []string{"-url", "http://x", "-user", "u", "nieistnieje"}, env, &o, &e); code != 2 {
		t.Errorf("zły tryb: kod %d", code)
	}
	if code := run(context.Background(), []string{"-password", "x", "info"}, env, &o, &e); code != 2 {
		t.Errorf("flaga -password nie może istnieć: kod %d", code)
	}
	o.Reset()
	if code := run(context.Background(), []string{"-list-cases"}, env, &o, &e); code != 0 || !strings.Contains(o.String(), "ascii-160") {
		t.Errorf("-list-cases: kod %d, %q", code, o.String())
	}
}

// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Program probe to narzędzie pomiarowe T00: wykonuje powtarzalne wywołania
// REST API RouterOS (SMS, LTE, system) i zapisuje zanonimizowane żądania
// oraz odpowiedzi jako JSON. Używa wyłącznie biblioteki standardowej.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

const usage = `probe — weryfikacja zachowania RouterOS (T00)

Użycie: probe [flagi] <tryb>

Tryby:
  info          informacje o routerze, modemie, device-mode, SMS (tylko odczyt)
  inbox         odczyt skrzynki odbiorczej i format pól (tylko odczyt)
  inbox-delete  usunięcie wpisu o podanym -id (wymaga --yes)
  send          jedna wiadomość (wymaga -to, --yes)
  send-suite    zestaw przypadków: ASCII 160/161/320/500, polskie 70/71, GSM, emoji (wymaga --yes)
  throughput    -count wiadomości, pomiar opóźnień i przepustowości (wymaga --yes)
  policies      stały zestaw wywołań do porównania zestawów polityk grupy

Hasło wyłącznie ze zmiennej PROBE_PASSWORD. Pozostałe zmienne: PROBE_URL,
PROBE_USER, PROBE_TO, PROBE_PORT, PROBE_FINGERPRINT.

Flagi:
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

var reSlug = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }

	a := &App{out: &syncWriter{w: stdout}}
	var (
		cfg        Config
		mode       string
		outDir     string
		label      string
		keepBodies bool
		listCases  bool
		showVer    bool
	)
	fs.StringVar(&cfg.BaseURL, "url", envOr(getenv, "PROBE_URL", ""), "adres routera, np. https://192.168.88.1 (env PROBE_URL)")
	fs.StringVar(&cfg.User, "user", envOr(getenv, "PROBE_USER", ""), "użytkownik testowy (env PROBE_USER)")
	fs.BoolVar(&cfg.Insecure, "insecure", false, "nie weryfikuj certyfikatu TLS (tylko testy)")
	fs.StringVar(&cfg.Fingerprint, "fingerprint", envOr(getenv, "PROBE_FINGERPRINT", ""), "oczekiwany odcisk SHA-256 certyfikatu routera (hex)")
	fs.StringVar(&cfg.CAFile, "ca", "", "plik PEM z CA do weryfikacji certyfikatu")
	fs.DurationVar(&cfg.Timeout, "timeout", 90*time.Second, "limit czasu jednego żądania (RouterOS przerywa po 60 s)")
	fs.StringVar(&a.port, "port", envOr(getenv, "PROBE_PORT", "lte1"), "port/interfejs LTE (env PROBE_PORT)")
	fs.StringVar(&a.to, "to", envOr(getenv, "PROBE_TO", ""), "numer testowy odbiorcy (env PROBE_TO)")
	fs.StringVar(&mode, "mode", "", "tryb pracy (alternatywa dla argumentu pozycyjnego)")
	fs.StringVar(&outDir, "out", "probe-results", "katalog bazowy wyników")
	fs.StringVar(&label, "label", "", "etykieta uruchomienia (dopisywana do nazwy katalogu)")
	fs.StringVar(&a.message, "message", "tikker probe", "treść wiadomości dla trybu send")
	fs.BoolVar(&a.noStatusReport, "no-status-report", false, "dodaj status-report-request=no do wysyłki")
	fs.BoolVar(&a.yes, "yes", false, "potwierdzam, że tryb może wysyłać prawdziwe SMS-y (koszty)")
	fs.DurationVar(&a.gap, "gap", 10*time.Second, "przerwa między wiadomościami w send-suite")
	fs.StringVar(&a.only, "only", "", "send-suite: lista przypadków po przecinku (domyślnie wszystkie)")
	fs.IntVar(&a.count, "count", 10, "throughput: liczba wiadomości")
	fs.IntVar(&a.parallel, "parallel", 1, "throughput: liczba równoległych żądań")
	fs.StringVar(&a.id, "id", "", "inbox-delete: identyfikator wpisu (.id)")
	fs.StringVar(&a.policies, "policies", "", "policies: opis testowanego zestawu polityk (tylko adnotacja)")
	fs.BoolVar(&keepBodies, "keep-inbox-bodies", false, "nie zasłaniaj treści (message, pdu) wiadomości w skrzynce")
	fs.BoolVar(&listCases, "list-cases", false, "wypisz przypadki send-suite i zakończ")
	fs.BoolVar(&showVer, "version", false, "wypisz wersję i zakończ")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if showVer {
		fmt.Fprintf(stdout, "probe %s (%s)\n", version, runtime.Version())
		return 0
	}
	if listCases {
		for _, c := range SuiteCases() {
			an := AnalyzeSMS(c.Text)
			fmt.Fprintf(stdout, "%-14s %4d zn. %-5s %4d jedn. %d seg.  %s\n", c.Name, an.Runes, an.Encoding, an.Units, an.Segments, c.Desc)
		}
		return 0
	}
	if mode == "" && fs.NArg() > 0 {
		mode = fs.Arg(0)
	}
	handlers := map[string]func(context.Context) (map[string]any, error){
		"info": a.ModeInfo, "inbox": a.ModeInbox, "inbox-delete": a.ModeInboxDelete,
		"send": a.ModeSend, "send-suite": a.ModeSendSuite, "throughput": a.ModeThroughput,
		"policies": a.ModePolicies,
	}
	h, ok := handlers[mode]
	if !ok {
		fmt.Fprintf(stderr, "nieznany lub brakujący tryb %q\n\n", mode)
		fs.Usage()
		return 2
	}
	cfg.Password = getenv("PROBE_PASSWORD")
	if cfg.BaseURL == "" || cfg.User == "" {
		fmt.Fprintln(stderr, "wymagane: -url i -user (lub PROBE_URL, PROBE_USER) oraz PROBE_PASSWORD")
		return 2
	}
	if cfg.Insecure && cfg.Fingerprint != "" {
		fmt.Fprintln(stderr, "-insecure i -fingerprint wykluczają się")
		return 2
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		fmt.Fprintln(stderr, "nieprawidłowy -url (oczekiwano http[s]://host[:port])")
		return 2
	}

	a.anon = NewAnonymizer()
	a.anon.AddLiteral("PASSWORD", cfg.Password)
	a.anon.AddLiteral("PHONE", a.to)
	a.anon.AddLiteral("HOST", u.Hostname())

	slug := mode
	if label != "" {
		slug += "-" + reSlug.ReplaceAllString(label, "_")
	}
	runDir := filepath.Join(outDir, time.Now().Format("20060102-150405")+"-"+slug)
	a.rec, err = NewRecorder(runDir, a.anon, keepBodies)
	if err != nil {
		fmt.Fprintln(stderr, "błąd:", err)
		return 1
	}
	a.c, err = NewClient(cfg, a.rec)
	if err != nil {
		fmt.Fprintln(stderr, "błąd:", err)
		return 1
	}

	fmt.Fprintf(stdout, "tryb %s, wyniki: %s\n", mode, runDir)
	started := time.Now()
	extra, herr := h(ctx)
	if herr != nil && extra == nil {
		fmt.Fprintln(stderr, "błąd:", a.anon.String(herr.Error()))
		return 2
	}
	meta := map[string]any{
		"tool_version": version, "go_version": runtime.Version(), "mode": mode, "label": label,
		"started": started.UTC().Format(time.RFC3339), "elapsed_ms": time.Since(started).Milliseconds(),
		"port": a.port, "tls": tlsDesc(cfg), "scheme": strings.ToLower(u.Scheme),
	}
	for k, v := range extra {
		meta[k] = v
	}
	if herr != nil {
		meta["error"] = a.anon.String(herr.Error())
	}
	if err := a.rec.WriteSummary(meta); err != nil {
		fmt.Fprintln(stderr, "błąd zapisu wyników:", err)
		return 1
	}
	fmt.Fprintln(stdout, "gotowe; sprawdź pliki w", runDir, "przed commitem")
	if herr != nil {
		fmt.Fprintln(stderr, "przerwano:", a.anon.String(herr.Error()))
		return 1
	}
	return 0
}

func tlsDesc(c Config) string {
	switch {
	case c.Fingerprint != "":
		return "fingerprint"
	case c.Insecure:
		return "insecure"
	case c.CAFile != "":
		return "custom-ca"
	}
	return "system-roots"
}

// syncWriter serializuje zapisy z równoległych goroutine (tryb throughput).
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

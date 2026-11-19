// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config to konfiguracja połączenia z routerem.
type Config struct {
	BaseURL     string
	User        string
	Password    string
	Insecure    bool
	Fingerprint string // SHA-256 certyfikatu serwera (hex, dowolna wielkość liter, opcjonalne ':')
	CAFile      string
	Timeout     time.Duration
}

// Result to nieanonimizowany wynik jednego wywołania (tylko w pamięci).
type Result struct {
	Seq      int
	Label    string
	Status   int
	Body     []byte
	Duration time.Duration
	Err      error
}

// OK zwraca true dla odpowiedzi 2xx bez błędu transportu.
func (r *Result) OK() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

// Client wykonuje żądania do REST API RouterOS i zapisuje każde z nich.
type Client struct {
	cfg  Config
	http *http.Client
	rec  *Recorder
}

// NewClient buduje klienta HTTP z wybraną polityką TLS.
func NewClient(cfg Config, rec *Recorder) (*Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.Fingerprint != "":
		want := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cfg.Fingerprint), ":", ""))
		tc.InsecureSkipVerify = true // weryfikacja własna: odcisk SHA-256 certyfikatu
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("brak certyfikatu serwera")
			}
			sum := sha256.Sum256(raw[0])
			if got := hex.EncodeToString(sum[:]); got != want {
				return fmt.Errorf("odcisk certyfikatu %s różni się od oczekiwanego", got)
			}
			return nil
		}
	case cfg.Insecure:
		tc.InsecureSkipVerify = true
	case cfg.CAFile != "":
		pem, err := os.ReadFile(filepath.Clean(cfg.CAFile))
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("nie udało się wczytać CA z pliku")
		}
		tc.RootCAs = pool
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	return &Client{
		cfg: cfg,
		rec: rec,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{TLSClientConfig: tc, DisableKeepAlives: false},
			// brak automatycznych przekierowań: chcemy je zobaczyć w wynikach
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Do wysyła żądanie (body: nil lub wartość serializowana do JSON) i zapisuje wynik.
func (c *Client) Do(ctx context.Context, label, method, path string, body any) *Result {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return &Result{Label: label, Err: err}
		}
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + path
	started := time.Now()
	res := &Result{Label: label}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		res.Err = err
		return res
	}
	req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ex := exchange{
		Label:   label,
		Started: started.UTC().Format(time.RFC3339Nano),
		Request: exchangeReq{Method: method, URL: url, Headers: req.Header.Clone(), Body: payload},
	}
	resp, err := c.http.Do(req)
	if err != nil {
		res.Err = err
	} else {
		defer resp.Body.Close()
		res.Status = resp.StatusCode
		res.Body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			res.Err = err
		}
		ex.Response = &exchangeResp{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: res.Body}
	}
	res.Duration = time.Since(started)
	ex.DurationMs = float64(res.Duration.Microseconds()) / 1000
	if res.Err != nil {
		ex.Error = res.Err.Error()
	}
	res.Seq = c.rec.Record(&ex)
	return res
}

// Recorder zapisuje zanonimizowane wymiany do katalogu wyników.
type Recorder struct {
	dir        string
	anon       *Anonymizer
	keepBodies bool
	mu         sync.Mutex
	seq        int
	entries    []summaryEntry
	werr       error
}

type exchangeReq struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

type exchangeResp struct {
	Status  int
	Headers http.Header
	Body    []byte
}

type exchange struct {
	Label      string
	Started    string
	DurationMs float64
	Request    exchangeReq
	Response   *exchangeResp
	Error      string
}

type summaryEntry struct {
	Seq        int     `json:"seq"`
	Label      string  `json:"label"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	DurationMs float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// NewRecorder tworzy katalog wyników.
func NewRecorder(dir string, anon *Anonymizer, keepBodies bool) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Recorder{dir: dir, anon: anon, keepBodies: keepBodies}, nil
}

var reLabel = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Record anonimizuje i zapisuje wymianę, zwraca jej numer.
func (r *Recorder) Record(ex *exchange) int {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()

	path := ex.Request.URL
	if i := strings.Index(path, "/rest"); i >= 0 {
		path = path[i:]
	}
	redact := !r.keepBodies && strings.Contains(path, "/tool/sms/inbox")
	out := map[string]any{
		"seq":         seq,
		"label":       ex.Label,
		"started":     ex.Started,
		"duration_ms": ex.DurationMs,
		"request": map[string]any{
			"method":  ex.Request.Method,
			"url":     r.anon.String(ex.Request.URL),
			"headers": r.anon.Headers(ex.Request.Headers),
			"body":    r.anon.Body(ex.Request.Body, false),
		},
	}
	status := 0
	if ex.Response != nil {
		status = ex.Response.Status
		out["response"] = map[string]any{
			"status":  ex.Response.Status,
			"headers": r.anon.Headers(ex.Response.Headers),
			"body":    r.anon.Body(ex.Response.Body, redact),
		}
	}
	if ex.Error != "" {
		out["error"] = r.anon.String(ex.Error)
	}
	name := fmt.Sprintf("%03d-%s.json", seq, reLabel.ReplaceAllString(ex.Label, "_"))
	err := writeJSON(filepath.Join(r.dir, name), out)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil && r.werr == nil {
		r.werr = err
	}
	r.entries = append(r.entries, summaryEntry{
		Seq: seq, Label: ex.Label, Method: ex.Request.Method, Path: r.anon.String(path),
		Status: status, DurationMs: ex.DurationMs, Error: r.anon.String(ex.Error),
	})
	return seq
}

// WriteSummary zapisuje summary.json (lista wymian + dane dodatkowe trybu).
func (r *Recorder) WriteSummary(extra map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.werr != nil {
		return r.werr
	}
	doc := map[string]any{"exchanges": r.entries}
	for k, v := range extra {
		doc[k] = v
	}
	return writeJSON(filepath.Join(r.dir, "summary.json"), doc)
}

func writeJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o640)
}

// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Anonymizer maskuje dane wrażliwe w zapisywanych wynikach pomiarów:
// numery telefonów, IMEI/IMSI/ICCID, adresy IP i MAC, numery seryjne,
// hasła i nagłówek Authorization. Maskowanie jest spójne w obrębie jednego
// uruchomienia (ta sama wartość = ten sam znacznik), co pozwala korelować wpisy.
type Anonymizer struct {
	mu       sync.Mutex
	literals map[string]litEntry // dokładne ciągi znane z konfiguracji
	mapping  map[string]map[string]string
}

type litEntry struct{ cat, key string }

// NewAnonymizer tworzy pusty anonimizator.
func NewAnonymizer() *Anonymizer {
	return &Anonymizer{
		literals: map[string]litEntry{},
		mapping:  map[string]map[string]string{},
	}
}

var (
	reMAC   = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)
	reIPv6  = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){4,7}[0-9a-f]{1,4}\b|(?:[0-9a-f]{1,4}:){1,6}:(?:[0-9a-f]{1,4})?`)
	reIPv4  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reICCID = regexp.MustCompile(`\b89\d{17,18}\b`)
	reID15  = regexp.MustCompile(`\b\d{15}\b`)
	rePlus  = regexp.MustCompile(`\+\d[\d ]{6,16}\d`)
	rePhone = regexp.MustCompile(`\b\d{9,14}\b`)
	reDigit = regexp.MustCompile(`\D`)
)

// klucze, których wartość jest zawsze w pełni zasłaniana
var secretKeys = map[string]bool{
	"password": true, "secret": true, "sim-pin": true, "pin": true,
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"set-cookie": true, "token": true, "old-password": true, "new-password": true,
	"private-key": true, "passphrase": true, "psk": true,
}

// klucze zastępowane spójnym znacznikiem kategorii
var identityKeys = map[string]string{
	"phone": "PHONE", "phone-number": "PHONE", "number": "PHONE", "from": "PHONE",
	"sender": "PHONE", "source": "PHONE", "allowed-number": "PHONE",
	"imei": "IMEI", "imsi": "IMSI", "uicc": "ICCID", "iccid": "ICCID", "sim-iccid": "ICCID",
	"serial-number": "SERIAL", "serial": "SERIAL", "mac-address": "MAC", "mac": "MAC",
	"identity": "IDENT", "current-cellid": "CELL", "cell-id": "CELL", "enb-id": "CELL",
	"lac": "CELL", "tac": "CELL", "phy-cellid": "CELL",
}

// AddLiteral rejestruje znaną wartość (np. numer testowy, hasło, host routera),
// która zostanie zamaskowana w dowolnym miejscu tekstu. Dla numerów telefonu
// dodawane są warianty (z "+" i bez, bez spacji i myślników).
func (a *Anonymizer) AddLiteral(cat, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := value
	variants := []string{value}
	if cat == "PHONE" {
		d := reDigit.ReplaceAllString(value, "")
		if d == "" {
			return
		}
		key = d
		variants = append(variants, d, "+"+d)
		if len(d) >= 11 {
			variants = append(variants, d[len(d)-9:]) // numer krajowy bez prefiksu
		}
	}
	for _, v := range variants {
		a.literals[v] = litEntry{cat, key}
	}
}

func (a *Anonymizer) pseudoLocked(cat, orig string) string {
	m := a.mapping[cat]
	if m == nil {
		m = map[string]string{}
		a.mapping[cat] = m
	}
	p, ok := m[orig]
	if !ok {
		p = fmt.Sprintf("<%s-%d>", cat, len(m)+1)
		m[orig] = p
	}
	return p
}

func (a *Anonymizer) pseudo(cat, orig string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pseudoLocked(cat, orig)
}

// String maskuje wrażliwe fragmenty dowolnego tekstu.
func (a *Anonymizer) String(s string) string {
	if s == "" {
		return s
	}
	a.mu.Lock()
	lits := make([]string, 0, len(a.literals))
	for l := range a.literals {
		lits = append(lits, l)
	}
	sort.Slice(lits, func(i, j int) bool { return len(lits[i]) > len(lits[j]) })
	for _, l := range lits {
		if strings.Contains(s, l) {
			e := a.literals[l]
			s = strings.ReplaceAll(s, l, a.pseudoLocked(e.cat, e.key))
		}
	}
	a.mu.Unlock()

	rep := func(re *regexp.Regexp, cat string, digitsKey bool) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			k := m
			if digitsKey {
				k = reDigit.ReplaceAllString(m, "")
			}
			return a.pseudo(cat, k)
		})
	}
	rep(reMAC, "MAC", false)
	rep(reIPv6, "IP", false)
	rep(reIPv4, "IP", false)
	rep(reICCID, "ICCID", false)
	rep(reID15, "ID15", false)
	rep(rePlus, "PHONE", true)
	rep(rePhone, "PHONE", true)
	return s
}

func isPlaceholder(s string) bool {
	return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">")
}

// Value maskuje zdekodowaną wartość JSON. redactMsg dodatkowo zastępuje pola
// "message" i "pdu" (treść cudzych SMS-ów) opisem długości.
func (a *Anonymizer) Value(key string, v any, redactMsg bool) any {
	k := strings.ToLower(key)
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for kk, vv := range t {
			out[kk] = a.Value(kk, vv, redactMsg)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = a.Value(key, vv, redactMsg)
		}
		return out
	case string:
		if secretKeys[k] {
			if t == "" {
				return t
			}
			return "<REDACTED>"
		}
		if redactMsg && (k == "message" || k == "pdu") {
			return fmt.Sprintf("<%s runes=%d bytes=%d>", strings.ToUpper(k), utf8.RuneCountInString(t), len(t))
		}
		if cat, ok := identityKeys[k]; ok && t != "" {
			masked := a.String(t)
			if isPlaceholder(masked) {
				return masked
			}
			if cat == "PHONE" {
				return a.pseudo(cat, reDigit.ReplaceAllString(t, ""))
			}
			return a.pseudo(cat, t)
		}
		return a.String(t)
	default:
		return v
	}
}

// Body maskuje treść żądania lub odpowiedzi. JSON jest przetwarzany strukturalnie,
// pozostałe treści jako tekst. Zwraca wartość gotową do zapisania w JSON.
func (a *Anonymizer) Body(raw []byte, redactMsg bool) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil && !dec.More() {
		return a.Value("", v, redactMsg)
	}
	const maxText = 64 << 10
	s := string(raw)
	if len(s) > maxText {
		s = s[:maxText] + "…[obcięto]"
	}
	return a.String(strings.ToValidUTF8(s, "�"))
}

// Headers maskuje nagłówki HTTP (Authorization, Cookie itd.).
func (a *Anonymizer) Headers(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		v := strings.Join(vs, ", ")
		if secretKeys[strings.ToLower(k)] {
			v = "<REDACTED>"
		} else {
			v = a.String(v)
		}
		out[k] = v
	}
	return out
}

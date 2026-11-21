// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package i18n holds the panel translations (Polish and English) and picks
// the language of a request.
//
// Translations are flat JSON objects (pl.json, en.json) embedded in the
// binary: key -> message. A message may contain fmt verbs (%s, %d) that
// [Bundle.T] fills in. Every key must exist in every language; [Load]
// enforces it. The key "meta.name" is the language's own name for the
// language switcher.
package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed pl.json en.json
var files embed.FS

// Default is the language used when the browser states no supported
// preference.
const Default = "en"

// Languages lists the supported language codes in the order shown in the
// switcher.
var Languages = []string{"pl", "en"}

// Bundle holds all translations. It is immutable after [Load] and safe for
// concurrent use.
type Bundle struct {
	msgs map[string]map[string]string
}

// Load parses the embedded translation files.
func Load() (*Bundle, error) {
	b := &Bundle{msgs: make(map[string]map[string]string, len(Languages))}
	for _, lang := range Languages {
		raw, err := files.ReadFile(lang + ".json")
		if err != nil {
			return nil, fmt.Errorf("i18n: read %s: %w", lang, err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("i18n: parse %s.json: %w", lang, err)
		}
		b.msgs[lang] = m
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	return b, nil
}

// check verifies that every language has the same keys.
func (b *Bundle) check() error {
	ref := b.msgs[Default]
	if ref == nil {
		return errors.New("i18n: default language missing")
	}
	for _, lang := range Languages {
		for k := range ref {
			if _, ok := b.msgs[lang][k]; !ok {
				return fmt.Errorf("i18n: key %q missing in %s", k, lang)
			}
		}
		for k := range b.msgs[lang] {
			if _, ok := ref[k]; !ok {
				return fmt.Errorf("i18n: key %q of %s missing in %s", k, lang, Default)
			}
		}
	}
	return nil
}

// Supported reports whether lang is a supported language code.
func (b *Bundle) Supported(lang string) bool {
	_, ok := b.msgs[lang]
	return ok
}

// T returns the message for key in lang, formatted with args (fmt verbs).
// An unknown language falls back to [Default]; an unknown key yields the key
// itself, so a missing translation is visible but never breaks a page.
func (b *Bundle) T(lang, key string, args ...any) string {
	m, ok := b.msgs[lang]
	if !ok {
		m = b.msgs[Default]
	}
	msg, ok := m[key]
	if !ok {
		return key
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// Has reports whether key exists (in every language, see [Load]).
func (b *Bundle) Has(key string) bool {
	_, ok := b.msgs[Default][key]
	return ok
}

// Keys returns the sorted keys.
func (b *Bundle) Keys() []string {
	keys := make([]string, 0, len(b.msgs[Default]))
	for k := range b.msgs[Default] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Match picks the best supported language for an Accept-Language header
// value, or [Default] when none matches. Only the primary subtag counts
// ("pl-PL" matches "pl"); q-values order the candidates, ties keep header
// order, q=0 excludes a language.
func (b *Bundle) Match(header string) string {
	type cand struct {
		lang string
		q    float64
	}
	var cands []cand
	for _, part := range strings.Split(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.ToLower(strings.TrimSpace(tag))
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		primary, _, _ := strings.Cut(tag, "-")
		if q > 0 && b.Supported(primary) {
			cands = append(cands, cand{primary, q})
		}
	}
	if len(cands) == 0 {
		return Default
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].q > cands[j].q })
	return cands[0].lang
}

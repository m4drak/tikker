// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"unicode/utf8"
)

const (
	gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsmExt   = "\f^{}\\[~]|€"
)

// SMSInfo opisuje oczekiwane kodowanie i liczbę segmentów tekstu.
type SMSInfo struct {
	Runes    int    `json:"runes"`
	Units    int    `json:"units"` // septety (GSM-7) lub jednostki UTF-16 (UCS-2)
	Encoding string `json:"encoding"`
	Segments int    `json:"segments"`
}

// AnalyzeSMS liczy kodowanie (gsm7/ucs2), jednostki i segmenty zgodnie ze
// standardem 3GPP TS 23.038 (160/153 dla GSM-7, 70/67 dla UCS-2).
func AnalyzeSMS(s string) SMSInfo {
	info := SMSInfo{Runes: utf8.RuneCountInString(s), Encoding: "gsm7"}
	septets := 0
	for _, r := range s {
		switch {
		case strings.ContainsRune(gsmBasic, r):
			septets++
		case strings.ContainsRune(gsmExt, r):
			septets += 2
		default:
			info.Encoding = "ucs2"
		}
	}
	if info.Encoding == "gsm7" {
		info.Units = septets
		info.Segments = segments(septets, 160, 153)
		return info
	}
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	info.Units = units
	info.Segments = segments(units, 70, 67)
	return info
}

func segments(n, single, multi int) int {
	switch {
	case n == 0:
		return 0
	case n <= single:
		return 1
	default:
		return (n + multi - 1) / multi
	}
}

// Case to jeden przypadek testowy zestawu send-suite.
type Case struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	Text string `json:"-"`
}

// fill buduje tekst o dokładnie n znakach z podanego wzorca (bez cyfr,
// bo długie ciągi cyfr byłyby maskowane jak numery telefonów w wynikach).
func fill(pattern string, n int) string {
	var b strings.Builder
	rs := []rune(pattern)
	for i := 0; i < n; i++ {
		b.WriteRune(rs[i%len(rs)])
	}
	return b.String()
}

const (
	asciiPattern  = "tikker probe the quick brown fox jumps over the lazy dog. "
	polishPattern = "zażółć gęślą jaźń, ZAŻÓŁĆ GĘŚLĄ JAŹŃ. "
)

// SuiteCases zwraca zdefiniowany zestaw przypadków (kolejność = kolejność wysyłki).
func SuiteCases() []Case {
	return []Case{
		{"ascii-short", "krótki ASCII (kontrola)", "tikker probe ascii short"},
		{"ascii-160", "ASCII, dokładnie 160 znaków (granica 1 segmentu GSM-7)", fill(asciiPattern, 160)},
		{"ascii-161", "ASCII, 161 znaków (pierwszy SMS wieloczęściowy lub błąd)", fill(asciiPattern, 161)},
		{"ascii-320", "ASCII, 320 znaków (3 segmenty po 153)", fill(asciiPattern, 320)},
		{"ascii-500", "ASCII, 500 znaków (4 segmenty po 153)", fill(asciiPattern, 500)},
		{"pl-short", "krótkie polskie znaki (UCS-2 lub utrata znaków)", "Zażółć gęślą jaźń"},
		{"pl-70", "polskie znaki, 70 znaków (granica 1 segmentu UCS-2)", fill(polishPattern, 70)},
		{"pl-71", "polskie znaki, 71 znaków (2 segmenty UCS-2 lub błąd)", fill(polishPattern, 71)},
		{"gsm7-accents", "znaki spoza ASCII należące do alfabetu GSM-7", "éèùìòÇØøÅåΔΦΓΛΩΠΨΣΘΞÆæßÉÄÖÑÜ§¿äöñüà ¥£¤¡"},
		{"gsm-ext-short", "znaki rozszerzone GSM (po 2 septety): ^{}\\[~]|€", "ext: ^ { } \\ [ ~ ] | €"},
		{"gsm-ext-80", "80 znaków rozszerzonych = 160 septetów (granica)", strings.Repeat("{", 80)},
		{"gsm-ext-81", "81 znaków rozszerzonych = 162 septety (przekroczenie)", strings.Repeat("{", 81)},
		{"emoji", "emoji (poza BMP, para zastępcza UTF-16)", "tikker emoji 😀 test"},
		{"newline", "tekst z końcem wiersza i cudzysłowem", "linia pierwsza\nlinia \"druga\" i 'trzecia'"},
	}
}

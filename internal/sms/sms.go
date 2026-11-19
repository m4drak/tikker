// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package sms decides how a text goes out as SMS and how raw inbound PDUs
// are read. It has no I/O and no state.
//
// Outbound: [Plan] picks the encoding (GSM-7 or UCS-2), optionally
// transliterates, counts segments and, for devices that cannot send long
// messages, rejects, truncates or splits the text (SPEC §4.6). [Analyze] is
// the same arithmetic for UIs and the API ("how many segments is this?").
//
// Sub-packages:
//
//   - gsm: the GSM 03.38 alphabet, segment counting, transliteration;
//   - phone: E.164 normalisation of recipients and senders;
//   - pdu: SMS-DELIVER decoding and reassembly of concatenated parts.
//
// [ErrorCode] maps this package's errors (and phone.ErrInvalidNumber) to the
// validation codes of api/openapi.yaml.
package sms

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms/gsm"
	"github.com/m4drak/tikker/internal/sms/phone"
)

// Strategy says what to do with a text that is longer than the device can
// send in one Send (SPEC §4.6).
type Strategy string

// Strategy values.
const (
	// StrategyReject fails with [ErrTooLong]. The default.
	StrategyReject Strategy = "reject"
	// StrategySplit cuts the text into several parts, each fitting one Send
	// and prefixed with "(i/n) ".
	StrategySplit Strategy = "split"
	// StrategyTruncate cuts the text to what one Send can carry.
	StrategyTruncate Strategy = "truncate"
)

// ParseStrategy parses s; the empty string means [StrategyReject].
func ParseStrategy(s string) (Strategy, error) {
	switch st := Strategy(s); st {
	case "":
		return StrategyReject, nil
	case StrategyReject, StrategySplit, StrategyTruncate:
		return st, nil
	}
	return "", fmt.Errorf("sms: unknown long-message strategy %q", s)
}

// Errors returned by [Plan]; test with errors.Is.
var (
	// ErrTooLong: the text needs more segments than allowed. API code
	// too_many_segments.
	ErrTooLong = errors.New("sms: text exceeds the segment limit")
	// ErrUnsupportedEncoding: the text needs an encoding the device cannot
	// send (typically UCS-2 on RouterOS) and transliteration is off.
	ErrUnsupportedEncoding = errors.New("sms: text needs an encoding the device does not support")
	// ErrEmptyBody: nothing to send (also after transliteration dropped
	// invisible characters).
	ErrEmptyBody = errors.New("sms: empty text")
	// ErrInvalidText: the text is not valid UTF-8.
	ErrInvalidText = errors.New("sms: text is not valid UTF-8")
)

// ErrorCode maps err to the validation code of api/openapi.yaml, or "" when
// err is not one of this package's errors.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrTooLong):
		return "too_many_segments"
	case errors.Is(err, phone.ErrInvalidNumber):
		return "invalid_phone_number"
	case errors.Is(err, ErrUnsupportedEncoding), errors.Is(err, ErrEmptyBody), errors.Is(err, ErrInvalidText):
		return "invalid_format"
	}
	return ""
}

// Options are the per-channel and per-request settings of [Plan].
type Options struct {
	// Transliterate rewrites characters outside GSM-7 to look-alikes
	// (gsm.Transliterate) before choosing the encoding.
	Transliterate bool
	// Strategy for text longer than one Send can carry; empty means reject.
	Strategy Strategy
	// MaxSegments is the most segments the whole message may use (SPEC
	// "max_segments"); 0 means no limit beyond the device's.
	MaxSegments int
}

// Part is one Send worth of text.
type Part struct {
	Text     string
	Segments int // SMS segments this Send occupies
	Index    int // 1-based position among Parts
	Of       int // len(Parts)
}

// Result is the outcome of [Plan].
type Result struct {
	// Encoding for every part.
	Encoding domain.Encoding
	// Body is the final text before splitting: transliterated and possibly
	// truncated. With StrategySplit the parts carry prefixes, Body does not.
	Body string
	// Parts to send, in order; always at least one.
	Parts []Part
	// Segments is the total over all parts.
	Segments int
	// Transliterated is true when transliteration changed the text.
	Transliterated bool
	// Substituted lists characters that became '?' during transliteration.
	Substituted []gsm.Replacement
	// Truncated is true when StrategyTruncate cut the text.
	Truncated bool
}

// Plan decides how to send body through a device with capabilities caps.
//
// The text is transliterated first (if asked), then its encoding is chosen:
// GSM-7 when possible, else UCS-2. A device that supports neither the needed
// encoding fails with [ErrUnsupportedEncoding]; Plan never transliterates
// behind the caller's back. Then the text's segment count n is compared with
// what one Send may carry: caps.MaxSegments if the device concatenates,
// otherwise 1, capped by opts.MaxSegments. A longer text is rejected,
// truncated or split according to opts.Strategy; a split's total segment
// count must not exceed opts.MaxSegments.
func Plan(body string, caps connector.Capabilities, opts Options) (Result, error) {
	if !utf8.ValidString(body) {
		return Result{}, ErrInvalidText
	}
	strategy, err := ParseStrategy(string(opts.Strategy))
	if err != nil {
		return Result{}, err
	}
	res := Result{Body: body}
	if opts.Transliterate {
		tr := gsm.Transliterate(body)
		res.Body, res.Transliterated, res.Substituted = tr.Text, tr.Changed, tr.Substituted
	}
	if res.Body == "" {
		return Result{}, ErrEmptyBody
	}

	enc := gsm.Analyze(res.Body).Encoding
	if !caps.SupportsEncoding(enc) {
		if enc == domain.EncodingGSM7 && caps.SupportsEncoding(domain.EncodingUCS2) {
			enc = domain.EncodingUCS2 // GSM-7 text is also valid UCS-2
		} else {
			return Result{}, fmt.Errorf("%w: need %s", ErrUnsupportedEncoding, enc)
		}
	}
	res.Encoding = enc

	perSend := caps.MaxSegments
	if !caps.Concatenation && perSend > 1 {
		perSend = 1
	}
	if opts.MaxSegments > 0 && opts.MaxSegments < perSend {
		perSend = opts.MaxSegments
	}

	counter := gsm.NewCounter(enc)
	counter.AddString(res.Body)
	n := counter.Segments()
	if n <= perSend { // also false for perSend < 1
		res.Parts = []Part{{Text: res.Body, Segments: n, Index: 1, Of: 1}}
		res.Segments = n
		return res, nil
	}

	switch strategy {
	case StrategyTruncate:
		if perSend < 1 {
			break
		}
		res.Body = truncate(res.Body, enc, perSend)
		res.Truncated = true
		res.Parts = []Part{{Text: res.Body, Segments: perSend, Index: 1, Of: 1}}
		c := gsm.NewCounter(enc)
		c.AddString(res.Body)
		res.Parts[0].Segments = c.Segments()
		res.Segments = res.Parts[0].Segments
		return res, nil
	case StrategySplit:
		if perSend < 1 {
			break
		}
		parts, err := split(res.Body, enc, perSend, opts.MaxSegments)
		if err != nil {
			return Result{}, err
		}
		res.Parts = parts
		for _, p := range parts {
			res.Segments += p.Segments
		}
		if opts.MaxSegments > 0 && res.Segments > opts.MaxSegments {
			return Result{}, fmt.Errorf("%w: split needs %d segments, limit %d", ErrTooLong, res.Segments, opts.MaxSegments)
		}
		return res, nil
	}
	return Result{}, fmt.Errorf("%w: needs %d segments, one send carries %d", ErrTooLong, n, perSend)
}

// truncate returns the longest prefix of s that needs at most segs segments.
func truncate(s string, enc domain.Encoding, segs int) string {
	c := gsm.NewCounter(enc)
	for i, r := range s {
		t := c
		t.Add(r)
		if t.Segments() > segs {
			return s[:i]
		}
		c = t
	}
	return s
}

// maxSplitParts bounds splitting when the caller sets no limit.
const maxSplitParts = 999

// split cuts s into parts of at most perSend segments each, prefixed with
// "(i/n) ". totalLimit (0 = none) bounds the number of parts early.
func split(s string, enc domain.Encoding, perSend, totalLimit int) ([]Part, error) {
	runes := []rune(s)
	limit := maxSplitParts
	if totalLimit > 0 && totalLimit < limit {
		limit = totalLimit
	}
	c := gsm.NewCounter(enc)
	c.AddString(s)
	n := (c.Segments() + perSend - 1) / perSend
	// The prefix length depends on n and n on the prefix length. More parts
	// never make a part longer, so iterate to a fixed point.
	for range 32 {
		texts, ok := pack(runes, enc, perSend, n, limit)
		if !ok {
			return nil, fmt.Errorf("%w: split exceeds %d parts", ErrTooLong, limit)
		}
		if len(texts) == n {
			parts := make([]Part, n)
			for i, t := range texts {
				pc := gsm.NewCounter(enc)
				pc.AddString(t)
				parts[i] = Part{Text: t, Segments: pc.Segments(), Index: i + 1, Of: n}
			}
			return parts, nil
		}
		n = len(texts)
	}
	return nil, fmt.Errorf("%w: cannot number split parts", ErrTooLong)
}

// pack fills parts greedily for a fixed part count n in the prefixes. ok is
// false when a part cannot take even one character or more than limit parts
// would be needed.
func pack(runes []rune, enc domain.Encoding, perSend, n, limit int) (texts []string, ok bool) {
	pos := 0
	for i := 1; pos < len(runes); i++ {
		if i > limit {
			return nil, false
		}
		prefix := fmt.Sprintf("(%d/%d) ", i, n)
		c := gsm.NewCounter(enc)
		c.AddString(prefix)
		end := pos
		for end < len(runes) {
			t := c
			t.Add(runes[end])
			if t.Segments() > perSend {
				break
			}
			c = t
			end++
		}
		if end == pos {
			return nil, false
		}
		if end < len(runes) {
			end = breakAt(runes, pos, end)
		}
		texts = append(texts, prefix+string(runes[pos:end]))
		pos = end
	}
	return texts, true
}

// breakAt prefers to end a part right after whitespace within its last 20
// characters, so words are not cut when it is cheap to avoid.
func breakAt(runes []rune, pos, end int) int {
	for j := end; j > pos && j > end-20; j-- {
		if unicode.IsSpace(runes[j-1]) {
			return j
		}
	}
	return end
}

// Info describes a text for previews.
type Info = gsm.Info

// Analyze reports encoding, length and segment count of body, as the UI and
// the API show them while the user types. It assumes a device that
// concatenates and does not transliterate; see [AnalyzeTransliterated].
func Analyze(body string) Info { return gsm.Analyze(body) }

// AnalyzeTransliterated is [Analyze] for the text as [Plan] sends it with
// Options.Transliterate: it also returns what transliteration substituted.
func AnalyzeTransliterated(body string) (Info, gsm.Result) {
	tr := gsm.Transliterate(body)
	return gsm.Analyze(tr.Text), tr
}

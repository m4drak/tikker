// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sending

import (
	"context"
	"errors"
	"fmt"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms"
)

// PreviewRequest is what the live counter of the send form knows.
type PreviewRequest struct {
	Body      string
	ChannelID string // "" - any channel
	Tags      []string
}

// Preview is the live analysis of a text for the channels that may send it:
// what sms.Plan would do, without enqueueing anything.
type Preview struct {
	// Text analysis (after transliteration when the channels do it).
	Encoding   domain.Encoding
	Chars      int
	Units      int
	Segments   int // segments of the text as one message
	PerSegment int
	Remaining  int
	// NonGSM are the characters that force UCS-2 (before transliteration).
	NonGSM []rune

	// Transliterate: the channels replace diacritics with ASCII.
	Transliterate bool
	// Changed: transliteration changed the text.
	Changed bool
	// Substituted are the characters replaced by '?'.
	Substituted []rune

	// Channels is the number of channels that may send the message (0 when
	// selection failed).
	Channels int
	// Planned is true when the planner accepted the text; Parts and
	// TotalSegments describe what would be enqueued.
	Planned       bool
	Parts         int
	TotalSegments int
	// MaxSegments is the limit that applies (0 - unknown).
	MaxSegments int

	// Errors are the problems the send would be rejected with (codes of the
	// API); Field is "" (channel selection), "/body", "/channel_id", "/tags".
	Errors []FieldError
}

// Preview analyses req.Body for the channels selected by req. Problems with
// the selection or the text are reported in Errors, not as an error.
func (s *Service) Preview(ctx context.Context, req PreviewRequest) (Preview, error) {
	rt, err := s.LoadRouting(ctx)
	if err != nil {
		return Preview{}, err
	}
	var p Preview
	var id domain.ID
	if req.ChannelID != "" {
		if len(req.ChannelID) > MaxIDLen {
			p.Errors = append(p.Errors, errf("/channel_id", CodeInvalidFormat, "A channel ID has 1 to %d characters.", MaxIDLen))
		}
		id = domain.ID(req.ChannelID)
	}
	p.Errors = append(p.Errors, ValidateTags("/tags", req.Tags)...)
	if len(p.Errors) > 0 {
		analyse(&p, req.Body, false)
		return p, nil
	}

	t, code, detail := s.selectChannels(rt, id, req.Tags)
	if code != "" {
		p.Errors = append(p.Errors, errf(channelField("", id, req.Tags), code, "%s", detail))
		analyse(&p, req.Body, false)
		return p, nil
	}
	p.Channels = len(t.channels)
	p.MaxSegments = t.opts.MaxSegments
	analyse(&p, req.Body, t.opts.Transliterate)
	if req.Body == "" {
		return p, nil
	}
	if runeLen(req.Body) > MaxBodyChars {
		p.Errors = append(p.Errors, errf("/body", CodeOutOfRange, "At most %d characters.", MaxBodyChars))
		return p, nil
	}
	res, perr := sms.Plan(req.Body, t.caps, t.opts)
	if perr != nil {
		code := sms.ErrorCode(perr)
		switch {
		case code == "":
			return Preview{}, fmt.Errorf("plan message: %w", perr)
		case errors.Is(perr, sms.ErrUnsupportedEncoding):
			p.Errors = append(p.Errors, errf("/body", code, "%s", unsupportedDetail(req.Body)))
		default:
			p.Errors = append(p.Errors, errf("/body", code, "%s", planDetail(perr)))
		}
		return p, nil
	}
	p.Planned = true
	p.Parts = len(res.Parts)
	p.TotalSegments = res.Segments
	p.Encoding = res.Encoding
	return p, nil
}

// analyse fills the text analysis of p.
func analyse(p *Preview, body string, translit bool) {
	p.Transliterate = translit
	plain := sms.Analyze(body)
	if !translit {
		fill(p, plain)
		return
	}
	info, tr := sms.AnalyzeTransliterated(body)
	p.Changed = tr.Changed
	for _, r := range tr.Substituted {
		p.Substituted = append(p.Substituted, r.Char)
	}
	fill(p, info)
	p.NonGSM = plain.NonGSM // what forces UCS-2 is judged on the original text
}

func fill(p *Preview, info sms.Info) {
	p.Encoding, p.Chars, p.Units, p.Segments = info.Encoding, info.Chars, info.Units, info.Segments
	p.PerSegment, p.Remaining, p.NonGSM = info.PerSegment, info.Remaining, info.NonGSM
}

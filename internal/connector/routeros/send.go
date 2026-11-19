// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
)

// maxSeptets is the length of one GSM-7 SMS. ASSUMPTION(T00): RouterOS
// counts the extension characters as two (research W5).
const maxSeptets = 160

// gsm7Basic is the GSM 03.38 default alphabet (without ESC), gsm7Ext the
// characters reached through the escape (two septets each).
const (
	gsm7Basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
		"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsm7Ext = "\f^{}\\[~]|€"
)

// septets returns the GSM-7 length of s and whether every character is in
// the GSM-7 alphabet.
func septets(s string) (int, bool) {
	n := 0
	for _, r := range s {
		switch {
		case strings.ContainsRune(gsm7Basic, r):
			n++
		case strings.ContainsRune(gsm7Ext, r):
			n += 2
		default:
			return n, false
		}
	}
	return n, true
}

// validate checks msg against Capabilities before anything is sent, so that
// a message the router cannot send fails permanently without a request.
func (c *Conn) validate(ch connector.ChannelRef, msg connector.OutboundSMS) error {
	if ch.Port == "" {
		return localError(domain.KindPermanent, opSend, fmt.Errorf("%w: channel has no port", ErrInvalidMessage))
	}
	if strings.TrimSpace(msg.To) == "" {
		return localError(domain.KindPermanent, opSend, fmt.Errorf("%w: no recipient", ErrInvalidMessage))
	}
	enc := msg.Encoding
	if enc == "" {
		enc = domain.EncodingGSM7
	}
	if !c.caps.SupportsEncoding(enc) {
		return localError(domain.KindPermanent, opSend, fmt.Errorf("%w: %s", ErrUnsupportedEncoding, enc))
	}
	segs := msg.Segments
	if segs <= 0 {
		segs = 1
	}
	if segs > c.caps.MaxSegments {
		return localError(domain.KindPermanent, opSend, fmt.Errorf("%w: %d segments, at most %d", ErrTooLong, segs, c.caps.MaxSegments))
	}
	n, ok := septets(msg.Body)
	if !ok {
		return localError(domain.KindPermanent, opSend, fmt.Errorf("%w: text contains characters outside GSM-7", ErrUnsupportedEncoding))
	}
	if n > maxSeptets*c.caps.MaxSegments {
		return localError(domain.KindPermanent, opSend, fmt.Errorf("%w: %d septets, at most %d", ErrTooLong, n, maxSeptets*c.caps.MaxSegments))
	}
	return nil
}

// Send implements connector.Connector. At most MaxConcurrentSends sends run
// per port; further calls wait for a slot or for ctx.
func (c *Conn) Send(ctx context.Context, ch connector.ChannelRef, msg connector.OutboundSMS) (connector.SendResult, error) {
	log := c.log.With(
		slog.String("port", ch.Port), slog.String("channel_id", ch.ChannelID.String()),
		slog.String("message_id", msg.MessageID.String()), logging.PhoneAttr("to", msg.To),
	)
	if err := c.validate(ch, msg); err != nil {
		log.Warn("sms rejected before sending", slog.Any("error", err))
		return connector.SendResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return connector.SendResult{}, localError(domain.KindTransient, opSend, err)
	}
	slot := c.portSlot(ch.Port)
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		// Nothing was sent: the port never became free.
		err := &domain.ConnectorError{Kind: domain.KindTransient, Op: opSend, Code: transportCode(ctx.Err()),
			Err: fmt.Errorf("waiting for port %s: %w", ch.Port, ctx.Err())}
		log.Warn("sms not sent: port busy until cancellation", slog.Any("error", err))
		return connector.SendResult{}, err
	}
	defer func() { <-slot }()

	start := time.Now()
	// ASSUMPTION(T00): no "smsc", "channel" or "status-report-request"
	// arguments; the router defaults apply.
	resp, err := c.do(ctx, call{
		op: opSend, method: http.MethodPost, path: "/tool/sms/send",
		body: map[string]string{
			"port":         ch.Port,
			"phone-number": msg.To,
			"message":      msg.Body,
		},
		timeout:  c.caps.SendTimeout,
		mutating: true,
	})
	elapsed := slog.Duration("elapsed", time.Since(start))
	if resp.ok() {
		// 2xx means RouterOS accepted the command, whatever the body is
		// (ASSUMPTION(T00): "[]"); a broken body does not undo the send.
		if err != nil {
			log.Warn("sms accepted but response unreadable", elapsed, slog.Any("error", err))
		}
		log.Info("sms accepted by router", elapsed)
		return connector.SendResult{AcceptedAt: c.clk.Now()}, nil
	}
	log.Warn("sms send failed", elapsed, slog.Any("error", err),
		slog.String("kind", domain.KindOf(err).String()), slog.Bool("ambiguous", domain.IsAmbiguous(err)))
	return connector.SendResult{}, err
}

// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package inbox

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
	"github.com/m4drak/tikker/internal/sms/pdu"
	"github.com/m4drak/tikker/internal/store"
)

// Warnings stored in Message.LastError of inbound messages.
const (
	warnPDUDecode = "pdu decode failed: text taken from device"
	// incompleteFmt is "incomplete: 2/3 parts".
	incompleteFmt = "incomplete: %d/%d parts"
)

// processed is the outcome of storing one batch.
type processed struct {
	created                              []domain.Message
	stored, duplicates, pending, expired int
}

// single is a router entry that is a whole message by itself.
type single struct {
	hash       string
	phone      string
	body       string // device-decoded text
	receivedAt time.Time
	sms        *pdu.SMS // decoded PDU, nil if there is none or it failed
	warn       string
}

// classify turns a router entry into a single message or a part of a
// concatenated one. The dedup hash is taken from the raw data: the PDU when
// the connector reports one, else the device text (ADR 0002 §4).
func (p *Poller) classify(it connector.InboundSMS, now time.Time) (*single, *store.InboundPart) {
	raw := it.Body
	var sms *pdu.SMS
	warn := ""
	if p.usePDU && it.PDU != "" {
		raw = it.PDU
		d, err := pdu.Decode(it.PDU, pdu.FormatAuto)
		if err != nil {
			warn = warnPDUDecode
			p.log.Warn("inbox pdu not decodable, using device text",
				slog.String("remote_id", it.RemoteID), slog.String("error", err.Error()))
		} else {
			sms = &d
		}
	}
	hash := domain.InboundDedupHash(p.ch.DeviceID, p.ch.Port, it.Phone, it.RawTimestamp, raw)

	received := it.Timestamp
	if received.IsZero() && sms != nil {
		received = sms.Timestamp
	}
	if received.IsZero() {
		received = now
	}

	if sms != nil && sms.Concat != nil && sms.Concat.Total > 1 && sms.Concat.Seq >= 1 && sms.Concat.Seq <= sms.Concat.Total {
		return nil, &store.InboundPart{
			ID: p.ids.NewID(), ChannelID: p.ch.ID, DedupHash: hash, Phone: it.Phone,
			RawTimestamp: it.RawTimestamp, ReceivedAt: received, Body: it.Body, PDU: it.PDU, CreatedAt: now,
		}
	}
	return &single{hash: hash, phone: it.Phone, body: it.Body, receivedAt: received, sms: sms, warn: warn}, nil
}

// message builds the stored form of a single entry. ASSUMPTION(T00): without
// a PDU the device text is GSM-7 (RouterOS decodes everything that way).
func (p *Poller) message(s *single, now time.Time) domain.Message {
	m := domain.Message{
		ID: p.ids.NewID(), Direction: domain.DirectionIn, ChannelID: p.ch.ID, Peer: s.phone,
		Body: s.body, Encoding: domain.EncodingGSM7, Segments: 1, Status: domain.StatusReceived,
		LastError: s.warn, CreatedAt: now, ReceivedAt: s.receivedAt, DedupHash: s.hash,
	}
	if s.sms != nil {
		m.Body = s.sms.Text
		m.Encoding = encodingOf(s.sms.Alphabet)
		if m.Peer == "" {
			m.Peer = s.sms.Sender
		}
	}
	return m
}

// encodingOf maps the PDU alphabet to the stored encoding. 8-bit data is
// decoded best-effort to Unicode text and is labelled UCS-2.
func encodingOf(a pdu.Alphabet) domain.Encoding {
	if a == pdu.AlphabetGSM7 {
		return domain.EncodingGSM7
	}
	return domain.EncodingUCS2
}

// groupHash is the dedup hash of a message joined from parts. It derives from
// the parts' hashes, so the same parts always give the same message.
func (p *Poller) groupHash(phone, tag string, parts []store.InboundPart) string {
	hashes := make([]string, len(parts))
	for i, sp := range parts {
		hashes[i] = sp.DedupHash
	}
	return domain.InboundDedupHash(p.ch.DeviceID, p.ch.Port, phone, tag, strings.Join(hashes, "\n"))
}

// process stores a batch of router entries in one transaction: whole messages,
// new parts, messages completed by them or by earlier parts, and incomplete
// messages whose parts timed out. It writes nothing when there is nothing to
// do.
func (p *Poller) process(ctx context.Context, items []connector.InboundSMS) (processed, error) {
	now := p.clk.Now()
	var singles []*single
	var parts []store.InboundPart
	for _, it := range items {
		if s, part := p.classify(it, now); s != nil {
			singles = append(singles, s)
		} else {
			parts = append(parts, *part)
		}
	}

	if len(items) == 0 {
		// Only a timed-out message can still need a write.
		pend, err := p.st.InboundParts().ListPending(ctx, p.ch.ID)
		if err != nil {
			return processed{}, fmt.Errorf("inbox: list pending parts: %w", err)
		}
		if len(pend) == 0 || now.Sub(pend[0].CreatedAt) < p.incompleteAfter {
			return processed{}, nil
		}
	}

	var out processed
	err := p.st.WithTx(ctx, func(tx store.Repos) error {
		out = processed{}
		return p.storeBatch(ctx, tx, now, singles, parts, &out)
	})
	if err != nil {
		return processed{}, fmt.Errorf("inbox: store: %w", err)
	}
	for _, m := range out.created {
		p.log.Debug("inbound message stored",
			slog.String("message", string(m.ID)), logging.PhoneAttr("peer", m.Peer),
			slog.Int("segments", m.Segments), slog.String("encoding", m.Encoding.String()))
	}
	return out, nil
}

// storeBatch is the body of the transaction of [Poller.process].
func (p *Poller) storeBatch(ctx context.Context, tx store.Repos, now time.Time,
	singles []*single, parts []store.InboundPart, out *processed) error {
	msgs, partRepo := tx.Messages(), tx.InboundParts()
	insert := func(m domain.Message) (bool, error) {
		created, err := msgs.InsertInbound(ctx, m)
		if created {
			out.created = append(out.created, m)
			out.stored++
		}
		return created, err
	}

	for _, s := range singles {
		created, err := insert(p.message(s, now))
		if err != nil {
			return err
		}
		if !created {
			out.duplicates++
		}
	}
	fresh := map[string]bool{} // part IDs stored by this cycle
	for _, part := range parts {
		created, err := partRepo.Add(ctx, part)
		if err != nil {
			return err
		}
		if created {
			fresh[string(part.ID)] = true
		} else {
			out.duplicates++
		}
	}

	pend, err := partRepo.ListPending(ctx, p.ch.ID)
	if err != nil {
		return err
	}
	byID := make(map[string]store.InboundPart, len(pend))
	items := make([]pdu.Item, 0, len(pend))
	for _, sp := range pend {
		byID[string(sp.ID)] = sp
		d, derr := pdu.Decode(sp.PDU, pdu.FormatAuto)
		if derr == nil && d.Concat != nil && d.Concat.Total > 1 {
			items = append(items, pdu.Item{ID: string(sp.ID), SMS: d})
			continue
		}
		// Not a part after all (stored data is damaged): keep its text as a
		// message of its own rather than losing it.
		s := &single{hash: sp.DedupHash, phone: sp.Phone, body: sp.Body, receivedAt: sp.ReceivedAt, warn: warnPDUDecode}
		if derr == nil {
			s.sms, s.warn = &d, ""
		}
		m := p.message(s, now)
		if _, err := insert(m); err != nil {
			return err
		}
		if err := partRepo.Consume(ctx, []domain.ID{sp.ID}, m.ID, now); err != nil {
			return err
		}
	}

	res := pdu.Assemble(items)
	for _, cm := range res.Complete {
		first := byID[cm.PartIDs[0]]
		used := make([]store.InboundPart, cm.Parts)
		for i := range used {
			used[i] = byID[cm.PartIDs[i]]
		}
		m := domain.Message{
			ID: p.ids.NewID(), Direction: domain.DirectionIn, ChannelID: p.ch.ID, Peer: peerOf(first.Phone, cm.Sender),
			Body: cm.Text, Encoding: encodingOf(cm.Alphabet), Segments: cm.Parts, Status: domain.StatusReceived,
			CreatedAt: now, ReceivedAt: first.ReceivedAt, DedupHash: p.groupHash(first.Phone, "multipart", used),
		}
		if _, err := insert(m); err != nil {
			return err
		}
		if err := partRepo.Consume(ctx, partIDs(cm.PartIDs), m.ID, now); err != nil {
			return err
		}
	}

	return p.expire(ctx, partRepo, insert, res.Pending, byID, fresh, now, out)
}

// expire stores the groups of pending parts that wait longer than
// IncompleteAfter as incomplete messages and counts the rest as pending.
func (p *Poller) expire(ctx context.Context, partRepo store.InboundPartRepo, insert func(domain.Message) (bool, error),
	pending []pdu.Item, byID map[string]store.InboundPart, fresh map[string]bool, now time.Time, out *processed) error {
	type key struct {
		sender string
		ref    uint16
		total  int
	}
	var order []key
	groups := map[key][]pdu.Item{}
	for _, it := range pending {
		k := key{it.SMS.Sender, it.SMS.Concat.Ref, it.SMS.Concat.Total}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], it)
	}
	for _, k := range order {
		g := groups[k]
		oldest := byID[g[0].ID].CreatedAt
		for _, it := range g {
			if c := byID[it.ID].CreatedAt; c.Before(oldest) {
				oldest = c
			}
		}
		if now.Sub(oldest) < p.incompleteAfter {
			for _, it := range g {
				if fresh[it.ID] {
					out.pending++
				}
			}
			continue
		}

		// Lowest sequence number first; a repeated part is consumed with
		// the message but contributes no text.
		sort.SliceStable(g, func(i, j int) bool { return g[i].SMS.Concat.Seq < g[j].SMS.Concat.Seq })
		var used []store.InboundPart
		var text strings.Builder
		lastSeq := 0
		for _, it := range g {
			if it.SMS.Concat.Seq == lastSeq {
				continue
			}
			lastSeq = it.SMS.Concat.Seq
			used = append(used, byID[it.ID])
			text.WriteString(it.SMS.Text)
		}
		first := byID[g[0].ID]
		m := domain.Message{
			ID: p.ids.NewID(), Direction: domain.DirectionIn, ChannelID: p.ch.ID, Peer: peerOf(first.Phone, k.sender),
			Body: text.String(), Encoding: encodingOf(g[0].SMS.Alphabet), Segments: k.total, Status: domain.StatusReceived,
			LastError: fmt.Sprintf(incompleteFmt, len(used), k.total),
			CreatedAt: now, ReceivedAt: first.ReceivedAt, DedupHash: p.groupHash(first.Phone, "incomplete", used),
		}
		if _, err := insert(m); err != nil {
			return err
		}
		ids := make([]domain.ID, len(g))
		for i, it := range g {
			ids[i] = domain.ID(it.ID)
		}
		if err := partRepo.Consume(ctx, ids, m.ID, now); err != nil {
			return err
		}
		out.expired++
		p.log.Warn("incomplete multi-part message stored",
			slog.String("message", string(m.ID)), logging.PhoneAttr("peer", m.Peer),
			slog.Int("parts", len(used)), slog.Int("total", k.total))
	}
	return nil
}

// peerOf prefers the number the device reported over the one in the PDU.
func peerOf(device, fromPDU string) string {
	if device != "" {
		return device
	}
	return fromPDU
}

func partIDs(ss []string) []domain.ID {
	ids := make([]domain.ID, len(ss))
	for i, s := range ss {
		ids[i] = domain.ID(s)
	}
	return ids
}

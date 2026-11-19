// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package pdu

import (
	"sort"
	"time"
)

// Item is one decoded inbox entry together with the caller's identifier for
// it (e.g. the device's RemoteID or a database key).
type Item struct {
	ID  string
	SMS SMS
}

// Message is a complete inbound message: a single SMS or concatenated parts
// joined in order.
type Message struct {
	Sender     string
	SenderKind AddressKind
	// Timestamp and Class are those of the first part.
	Timestamp time.Time
	Alphabet  Alphabet
	Class     int
	Text      string
	// Parts is the number of parts (1 for a single SMS).
	Parts int
	// PartIDs are the IDs of all items that make up the message, in part
	// order, followed by IDs of duplicate deliveries of a part. All of them
	// are consumed and may be deleted from the device.
	PartIDs []string
}

// Result of [Assemble].
type Result struct {
	// Complete messages, ordered by the position of their first item in the
	// input.
	Complete []Message
	// Pending are items of messages that still miss parts, in input order.
	// The caller keeps them and passes them again with the next items.
	Pending []Item
}

type groupKey struct {
	sender string
	ref    uint16
	total  int
}

type group struct {
	first int // input index of the first item
	items []int
}

// Assemble joins concatenated parts. Parts belong together when sender,
// reference number and total match. Callers pass the items they hold (new
// ones plus the Pending of earlier calls); nothing is remembered between
// calls, and expiring parts that never complete is up to the caller. A
// repeated part (same sequence number) is consumed with its message and its
// ID is reported in PartIDs.
func Assemble(items []Item) Result {
	type done struct {
		first int
		msg   Message
	}
	var finished []done
	groups := map[groupKey]*group{}
	var order []*group
	for i, it := range items {
		c := it.SMS.Concat
		if c == nil || c.Total <= 1 || c.Seq < 1 || c.Seq > c.Total {
			finished = append(finished, done{i, build([]Item{it})})
			continue
		}
		k := groupKey{it.SMS.Sender, c.Ref, c.Total}
		g := groups[k]
		if g == nil {
			g = &group{first: i}
			groups[k] = g
			order = append(order, g)
		}
		g.items = append(g.items, i)
	}

	pending := map[int]bool{}
	for _, g := range order {
		bySeq := map[int]Item{}
		var dups []Item
		for _, i := range g.items {
			seq := items[i].SMS.Concat.Seq
			if _, dup := bySeq[seq]; dup {
				dups = append(dups, items[i])
			} else {
				bySeq[seq] = items[i]
			}
		}
		total := items[g.first].SMS.Concat.Total
		if len(bySeq) < total {
			for _, i := range g.items {
				pending[i] = true
			}
			continue
		}
		ordered := make([]Item, 0, total)
		for seq := 1; seq <= total; seq++ {
			ordered = append(ordered, bySeq[seq])
		}
		m := build(ordered)
		for _, d := range dups {
			m.PartIDs = append(m.PartIDs, d.ID)
		}
		finished = append(finished, done{g.first, m})
	}

	sort.SliceStable(finished, func(i, j int) bool { return finished[i].first < finished[j].first })
	var res Result
	for _, d := range finished {
		res.Complete = append(res.Complete, d.msg)
	}
	for i, it := range items {
		if pending[i] {
			res.Pending = append(res.Pending, it)
		}
	}
	return res
}

// build joins the ordered parts of one message. The raw user data of
// parts with the same alphabet is decoded together so that a character
// split over a part boundary (a GSM-7 escape pair or a UTF-16 surrogate
// pair) is still read correctly.
func build(parts []Item) Message {
	first := parts[0].SMS
	m := Message{
		Sender: first.Sender, SenderKind: first.SenderKind, Timestamp: first.Timestamp,
		Alphabet: first.Alphabet, Class: first.Class, Parts: len(parts),
	}
	var text []byte
	var cur Alphabet
	var buf []byte
	flush := func() {
		if len(buf) > 0 {
			text = append(text, decodeText(cur, buf)...)
			buf = nil
		}
	}
	for i, p := range parts {
		m.PartIDs = append(m.PartIDs, p.ID)
		if i > 0 && p.SMS.Alphabet != cur {
			flush()
		}
		cur = p.SMS.Alphabet
		buf = append(buf, p.SMS.raw...)
	}
	flush()
	m.Text = string(text)
	if len(parts) == 1 && len(first.raw) == 0 {
		m.Text = first.Text // hand-built SMS without raw user data
	}
	return m
}

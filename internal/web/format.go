// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Translation keys of enum values. The literals are listed here (not built
// from the value) so that the key check of the tests sees them.
var (
	statusKeys = map[domain.MessageStatus]string{
		domain.StatusQueued:    "status.queued",
		domain.StatusSending:   "status.sending",
		domain.StatusSent:      "status.sent",
		domain.StatusFailed:    "status.failed",
		domain.StatusCancelled: "status.cancelled",
		domain.StatusReceived:  "status.received",
	}
	stateKeys = map[domain.ChannelState]string{
		domain.ChannelOK:       "state.ok",
		domain.ChannelDegraded: "state.degraded",
		domain.ChannelDown:     "state.down",
		domain.ChannelDisabled: "state.disabled",
	}
	directionKeys = map[domain.Direction]string{
		domain.DirectionOut: "direction.out",
		domain.DirectionIn:  "direction.in",
	}
)

// statusKey returns the translation key of a message status.
func statusKey(st domain.MessageStatus) string {
	if k, ok := statusKeys[st]; ok {
		return k
	}
	return "status.unknown"
}

func stateKey(st domain.ChannelState) string {
	if k, ok := stateKeys[st]; ok {
		return k
	}
	return "state.unknown"
}

func directionKey(d domain.Direction) string {
	if k, ok := directionKeys[d]; ok {
		return k
	}
	return "direction.unknown"
}

// formatTime renders t in UTC for tables; the zero time is a dash.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// encodingLabel is the display name of an SMS encoding.
func encodingLabel(e domain.Encoding) string {
	switch e {
	case domain.EncodingGSM7:
		return "GSM-7"
	case domain.EncodingUCS2:
		return "UCS-2"
	}
	return string(e)
}

// signalText renders the known signal values ("RSRP -95 dBm, SINR 12 dB").
func signalText(sg *domain.Signal) string {
	if sg == nil {
		return ""
	}
	var parts []string
	add := func(name string, v *int, unit string) {
		if v != nil {
			parts = append(parts, fmt.Sprintf("%s %d %s", name, *v, unit))
		}
	}
	add("RSRP", sg.RSRP, "dBm")
	add("RSRQ", sg.RSRQ, "dB")
	add("SINR", sg.SINR, "dB")
	add("RSSI", sg.RSSI, "dBm")
	return strings.Join(parts, ", ")
}

// preview shortens a message body for a table cell: whitespace runs become a
// single space, and at most limit characters are kept.
func preview(body string, limit int) string {
	s := strings.Join(strings.Fields(body), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

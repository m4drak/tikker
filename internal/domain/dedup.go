// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// InboundDedupHash returns Message.DedupHash for an inbound SMS (SPEC §4.3):
// the hex SHA-256 of device, port, phone, timestamp and body.
//
// timestamp must be the router's raw timestamp string, not a parsed and
// re-formatted time, so that the hash is stable regardless of how tikker
// parses RouterOS dates. Each field is length-prefixed, so values containing
// the separator cannot collide.
//
// Two genuinely different SMS with identical sender, second-precision
// timestamp and text on the same port are indistinguishable and are stored
// once; this is accepted (see ADR 0002).
func InboundDedupHash(deviceID ID, port, phone, timestamp, body string) string {
	h := sha256.New()
	for _, f := range []string{string(deviceID), port, phone, timestamp, body} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{'|'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

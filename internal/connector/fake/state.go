// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
)

// Signal is the radio quality reported by /interface/lte monitor.
type Signal struct {
	RSSI, RSRP, RSRQ, SINR int
}

// LTEState is the modem state of one port.
type LTEState struct {
	// Running is the interface "running" flag.
	Running bool
	// Registration is registration-status: "home", "roaming", "searching",
	// "denied" or "not-registered". Only home and roaming can send.
	// ASSUMPTION(T00): exact value strings of the real monitor output.
	Registration     string
	Operator         string
	AccessTechnology string
	// Signal nil means no signal figures are reported (not registered).
	Signal *Signal
	IMEI   string
	IMSI   string
	CellID string
	Band   string
}

// Registered reports whether the modem is on a network.
func (l LTEState) Registered() bool {
	return l.Registration == "home" || l.Registration == "roaming"
}

func defaultLTE() *LTEState {
	return &LTEState{
		Running: true, Registration: "home", Operator: "Fake Mobile PL",
		AccessTechnology: "LTE",
		Signal:           &Signal{RSSI: -67, RSRP: -95, RSRQ: -10, SINR: 12},
		IMEI:             "356938035643809", IMSI: "260060000000001",
		CellID: "12345678", Band: "B3@20Mhz",
	}
}

// SetLTE edits the modem state of port under the server lock. It returns
// false for an unknown port.
func (s *Server) SetLTE(port string, edit func(*LTEState)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.lte[port]
	if !ok {
		return false
	}
	edit(st)
	return true
}

// LTE returns a copy of the modem state of port.
func (s *Server) LTE(port string) (LTEState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.lte[port]
	if !ok {
		return LTEState{}, false
	}
	c := *st
	if st.Signal != nil {
		sig := *st.Signal
		c.Signal = &sig
	}
	return c, true
}

// SentMessage is one send the router accepted.
type SentMessage struct {
	Seq         int
	Port        string
	PhoneNumber string
	Message     string
	// Type is the optional "type" argument (e.g. "class-0").
	Type string
	// Septets is the GSM-7 length of Message (0 if not representable).
	Septets int
	// ReceivedAt is when the request arrived; StartedAt when it got the
	// modem port; CompletedAt when the send finished. Wall-clock times, for
	// asserting serialization and delays.
	ReceivedAt, StartedAt, CompletedAt time.Time
	// Ambiguous is true when the client did not get a success answer
	// although the SMS went out (timeout/forced fault after accepting).
	Ambiguous bool
}

// Sent returns the accepted sends in completion order.
func (s *Server) Sent() []SentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SentMessage(nil), s.sent...)
}

// WaitSent waits until at least n sends are recorded or timeout passes.
func (s *Server) WaitSent(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		got := len(s.sent)
		s.mu.Unlock()
		if got >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// InboxEntry is one stored SMS as /tool sms inbox reports it.
type InboxEntry struct {
	ID        string // "*1", "*2", ...
	Port      string
	Phone     string
	Timestamp time.Time
	Type      string
	Message   string
	PDU       string

	rawTimestamp string // verbatim timestamp text, overrides Timestamp
}

// Inbox returns the stored messages, oldest first.
func (s *Server) Inbox() []InboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]InboxEntry(nil), s.inbox...)
}

// SetInboxLimit sets the inbox capacity (0 = unlimited).
func (s *Server) SetInboxLimit(n int) {
	s.mu.Lock()
	s.inboxLimit = n
	s.mu.Unlock()
}

// ErrInboxFull is returned by the Inject functions when the inbox is full and
// auto-erase is off; the message is lost, like on a full modem.
var ErrInboxFull = errors.New("fake: inbox full")

// IncomingSMS is a received message to inject. The PDU is built from it.
type IncomingSMS struct {
	Port string // default: first port
	From string // sender, "+48511222333"
	Text string
	Time time.Time // default: server clock
	// Encoding defaults to GSM-7 when possible, else UCS-2.
	Encoding Encoding
	Class0   bool
	// Concat injects exactly this part (Text is the part's text) instead of
	// splitting automatically; use it for out-of-order or missing parts.
	Concat *Concat
}

// RawInbox is an inbox record stored verbatim, for malformed-data tests.
type RawInbox struct {
	Port      string
	Phone     string
	Timestamp time.Time
	// TimestampText, when set, replaces the formatted Timestamp in the REST
	// output (to test unparsable timestamps).
	TimestampText string
	Type          string
	Message       string
	PDU           string
}

// InjectSMS stores an incoming SMS and returns the inbox IDs created: one per
// part (long texts are split into concatenated parts with UDH). The "message"
// field is what RouterOS would show: the text for GSM-7, but UCS-2 bytes
// misread as GSM-7 (research W5/W9), so clients must decode the PDU.
func (s *Server) InjectSMS(in IncomingSMS) ([]string, error) {
	port, err := s.portOrDefault(in.Port)
	if err != nil {
		return nil, err
	}
	when := in.Time
	if when.IsZero() {
		when = s.opts.Now()
	}
	enc := in.Encoding
	if enc == EncodingAuto {
		enc = EncodingUCS2
		if IsGSM7(in.Text) {
			enc = EncodingGSM7
		}
	}
	p := PDUParams{
		SMSC: s.opts.SMSC, Sender: in.From, Time: when,
		Encoding: enc, Class0: in.Class0,
	}
	var parts []PDUPart
	if in.Concat != nil {
		p.Concat = in.Concat
		pdu, err := BuildDeliverPDU(p, in.Text)
		if err != nil {
			return nil, err
		}
		parts = []PDUPart{{PDU: pdu, Text: in.Text}}
	} else {
		s.mu.Lock()
		ref := s.nextRef
		s.nextRef = s.nextRef%255 + 1
		s.mu.Unlock()
		parts, err = BuildDeliverPDUs(p, in.Text, ref, false)
		if err != nil {
			return nil, err
		}
	}
	typ := ""
	if in.Class0 {
		typ = "class-0"
	}
	entries := make([]InboxEntry, len(parts))
	for i, pt := range parts {
		entries[i] = InboxEntry{
			Port: port, Phone: in.From, Timestamp: when, Type: typ,
			Message: displayText(pt.Text, enc), PDU: pt.PDU,
		}
	}
	return s.store(entries)
}

// InjectRaw stores r verbatim and returns its inbox ID.
func (s *Server) InjectRaw(r RawInbox) (string, error) {
	port, err := s.portOrDefault(r.Port)
	if err != nil {
		return "", err
	}
	when := r.Timestamp
	if when.IsZero() {
		when = s.opts.Now()
	}
	ids, err := s.store([]InboxEntry{{
		Port: port, Phone: r.Phone, Timestamp: when, Type: r.Type,
		Message: r.Message, PDU: r.PDU, rawTimestamp: r.TimestampText,
	}})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

func (s *Server) portOrDefault(p string) (string, error) {
	if p == "" {
		return s.opts.Ports[0], nil
	}
	if _, ok := s.portSem[p]; !ok {
		return "", fmt.Errorf("fake: unknown port %q", p)
	}
	return p, nil
}

// store appends entries honouring the inbox limit. With auto-erase the oldest
// entries are dropped to make room; without it nothing is stored.
func (s *Server) store(entries []InboxEntry) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lim := s.inboxLimit; lim > 0 {
		over := len(s.inbox) + len(entries) - lim
		if over > 0 {
			if !s.settings.AutoErase {
				return nil, ErrInboxFull
			}
			if over > len(s.inbox) {
				over = len(s.inbox)
			}
			s.inbox = s.inbox[over:]
		}
	}
	ids := make([]string, len(entries))
	for i := range entries {
		entries[i].ID = fmt.Sprintf("*%X", s.nextInboxID)
		s.nextInboxID++
		ids[i] = entries[i].ID
	}
	if lim := s.inboxLimit; lim > 0 && len(entries) > lim {
		entries = entries[len(entries)-lim:] // earlier parts are erased again
	}
	s.inbox = append(s.inbox, entries...)
	return ids, nil
}

// displayText is the "message" field RouterOS would show for text sent in
// enc.
// ASSUMPTION(T00): RouterOS decodes everything as GSM-7 (research W5/W9), so
// UCS-2 user data appears as garbage; we emulate that by reading the UTF-16BE
// octets through the GSM-7 table.
func displayText(text string, enc Encoding) string {
	if enc != EncodingUCS2 {
		return text
	}
	var oct []byte
	for _, u := range utf16.Encode([]rune(text)) {
		oct = append(oct, byte(u>>8), byte(u))
	}
	return gsm7Decode(oct)
}

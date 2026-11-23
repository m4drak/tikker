// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// JSON documents of /agent/v1 (docs/agent.md). RouterOS scripts build JSON
// by hand or with :serialize, which may quote numbers and booleans, so the
// scalar fields accept both forms (flexInt, flexBool).

// checkinRequest is POST /agent/v1/checkin.
type checkinRequest struct {
	RouterOSVersion string       `json:"routeros_version"`
	Board           string       `json:"board"`
	AgentVersion    string       `json:"agent_version"`
	Ports           []portReport `json:"ports"`
}

type portReport struct {
	Port         string    `json:"port"`
	Running      *flexBool `json:"running"`    // default true
	Registered   *flexBool `json:"registered"` // default from Registration
	Registration string    `json:"registration"`
	Operator     string    `json:"operator"`
	Technology   string    `json:"technology"`
	RSSI         optLevel  `json:"rssi"`
	RSRP         optLevel  `json:"rsrp"`
	RSRQ         optLevel  `json:"rsrq"`
	SINR         optLevel  `json:"sinr"`
}

type checkinResponse struct {
	DeviceID   string `json:"device_id"`
	ServerTime string `json:"server_time"`
	// StaleAfterSeconds tells the script how often it must check in at least.
	StaleAfterSeconds int `json:"stale_after_s"`
}

// commandsRequest is POST /agent/v1/commands; the body is optional.
type commandsRequest struct {
	Ports []string `json:"ports"`
	Max   flexInt  `json:"max"`
	// Wait is the long-poll time in seconds (0: answer at once).
	Wait flexInt `json:"wait"`
}

type commandDTO struct {
	MessageID string      `json:"message_id"`
	Port      string      `json:"port"`
	To        string      `json:"to"`
	Body      string      `json:"body"`
	Encoding  string      `json:"encoding"`
	Segments  int         `json:"segments"`
	PDUs      []SubmitPDU `json:"pdus"`
}

type inboxRef struct {
	Port     string `json:"port"`
	RemoteID string `json:"remote_id"`
}

type commandsResponse struct {
	Commands []commandDTO `json:"commands"`
	// InboxDelete are inbox entries tikker has stored: the script removes
	// them on the router and acknowledges with POST /agent/v1/inbox/ack.
	InboxDelete []inboxRef `json:"inbox_delete"`
}

// resultRequest is POST /agent/v1/commands/{message_id}/result.
type resultRequest struct {
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	RemoteRef string `json:"remote_ref"`
}

type resultResponse struct {
	MessageID string `json:"message_id"`
	// Known is false when no send was waiting for the result any more; a
	// final result is remembered anyway.
	Known bool `json:"known"`
}

// inboxRequest is POST /agent/v1/inbox.
type inboxRequest struct {
	Messages []inboxMessage `json:"messages"`
}

type inboxMessage struct {
	Port      string `json:"port"`
	RemoteID  string `json:"remote_id"`
	Phone     string `json:"phone"`
	Timestamp string `json:"timestamp"`
	PDU       string `json:"pdu"`
	Body      string `json:"body"`
	Type      string `json:"type"`
}

type inboxResponse struct {
	Accepted    int        `json:"accepted"`
	Refused     int        `json:"refused"`
	InboxDelete []inboxRef `json:"inbox_delete"`
}

// ackRequest is POST /agent/v1/inbox/ack.
type ackRequest struct {
	Deleted []inboxRef `json:"deleted"`
}

type ackResponse struct {
	Removed     int        `json:"removed"`
	InboxDelete []inboxRef `json:"inbox_delete"`
}

type statusResponse struct {
	DeviceID        string  `json:"device_id"`
	LastCheckin     *string `json:"last_checkin"`
	LastSeen        *string `json:"last_seen"`
	PendingCommands int     `json:"pending_commands"`
	InboxEntries    int     `json:"inbox_entries"`
	PendingDeletes  int     `json:"pending_deletes"`
}

// flexInt is an integer that may arrive as a JSON number or string; strings
// may carry a unit ("-67dBm", "-10.5dB"), rounded to an integer. An empty
// string or null leaves it at zero / nil.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	v, ok, err := parseFlexInt(b)
	if ok {
		*f = flexInt(v)
	}
	return err
}

// parseFlexInt parses a JSON number or string; ok is false for null or an
// empty string.
func parseFlexInt(b []byte) (v int, ok bool, err error) {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return 0, false, nil
	}
	s := string(b)
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(b, &s); err != nil {
			return 0, false, err
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, nil
	}
	v, ok = parseLevel(s)
	if !ok {
		return 0, false, fmt.Errorf("not a number: %q", truncate(s, 32))
	}
	return v, true, nil
}

// optLevel is a signal level that may be absent: a missing field, null or
// an empty string leave it unset.
type optLevel struct {
	v   int
	set bool
}

func (o *optLevel) UnmarshalJSON(b []byte) error {
	v, ok, err := parseFlexInt(b)
	o.v, o.set = v, ok
	return err
}

func (o optLevel) ptr() *int {
	if !o.set {
		return nil
	}
	v := o.v
	return &v
}

// parseLevel reads the leading number of s, rounded.
func parseLevel(s string) (int, bool) {
	end := 0
	for end < len(s) && (s[end] == '-' || s[end] == '+' || s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e6 {
		return 0, false
	}
	return int(math.Round(f)), true
}

// flexBool accepts true/false, "true"/"false", "yes"/"no" and 1/0.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.ToLower(string(bytes.TrimSpace(b))), `"`)
	switch s {
	case "true", "yes", "1":
		*f = true
	case "false", "no", "0", "", "null":
		*f = false
	default:
		return fmt.Errorf("not a boolean: %q", truncate(s, 32))
	}
	return nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

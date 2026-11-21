// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"strings"

	"github.com/m4drak/tikker/internal/domain"
)

// The types below are the JSON forms of the schemas of api/openapi.yaml. They
// are filled field by field from the domain entities, which have no JSON tags
// and carry secrets (password hashes, token hashes, encrypted passwords).

// messageDTO is schema Message.
type messageDTO struct {
	ID            string  `json:"id"`
	Direction     string  `json:"direction"`
	ChannelID     *string `json:"channel_id"`
	Peer          string  `json:"peer"`
	Body          *string `json:"body"`
	Encoding      string  `json:"encoding"`
	Segments      int     `json:"segments"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	NextAttemptAt *string `json:"next_attempt_at"`
	LastError     *string `json:"last_error"`
	// PossibleDuplicate: a send may have reached the router twice (ADR 0002).
	PossibleDuplicate bool     `json:"possible_duplicate"`
	ClientRef         *string  `json:"client_ref"`
	Tags              []string `json:"tags"`
	ExpiresAt         *string  `json:"expires_at"`
	CreatedBy         *string  `json:"created_by"`
	CreatedAt         string   `json:"created_at"`
	SentAt            *string  `json:"sent_at"`
	ReceivedAt        *string  `json:"received_at"`
}

func toMessageDTO(m domain.Message) messageDTO {
	d := messageDTO{
		ID: string(m.ID), Direction: string(m.Direction), ChannelID: nullableString(string(m.ChannelID)),
		Peer: m.Peer, Body: nullableString(m.Body), Encoding: string(m.Encoding), Segments: m.Segments,
		Status: string(m.Status), Attempts: m.Attempts, LastError: nullableString(m.LastError),
		PossibleDuplicate: m.PossibleDuplicate,
		ClientRef:         nullableString(m.ClientRef), Tags: append([]string{}, m.Route.Tags...),
		ExpiresAt: nullableTime(m.ExpiresAt), CreatedAt: formatTime(m.CreatedAt),
		SentAt: nullableTime(m.SentAt), ReceivedAt: nullableTime(m.ReceivedAt),
	}
	// next_attempt_at only means something for a message waiting for a retry.
	if m.Status == domain.StatusQueued && m.Attempts > 0 {
		d.NextAttemptAt = nullableTime(m.NextAttemptAt)
	}
	if !m.CreatedBy.IsZero() {
		s := m.CreatedBy.String()
		d.CreatedBy = &s
	}
	return d
}

// signalDTO is schema Signal.
type signalDTO struct {
	RSSI *int `json:"rssi"`
	RSRP *int `json:"rsrp"`
	RSRQ *int `json:"rsrq"`
	SINR *int `json:"sinr"`
}

func toSignalDTO(s *domain.Signal) *signalDTO {
	if s == nil {
		return nil
	}
	return &signalDTO{RSSI: s.RSSI, RSRP: s.RSRP, RSRQ: s.RSRQ, SINR: s.SINR}
}

// channelDTO is schema Channel.
type channelDTO struct {
	ID            string     `json:"id"`
	DeviceID      string     `json:"device_id"`
	Port          string     `json:"port"`
	Name          string     `json:"name"`
	Tags          []string   `json:"tags"`
	RatePerMin    int        `json:"rate_per_min"`
	MaxSegments   int        `json:"max_segments"`
	Transliterate bool       `json:"transliterate"`
	Enabled       bool       `json:"enabled"`
	State         string     `json:"state"`
	StateReason   *string    `json:"state_reason"`
	LastHealthAt  *string    `json:"last_health_at"`
	Signal        *signalDTO `json:"signal"`
	Operator      *string    `json:"operator"`
	QueueDepth    int        `json:"queue_depth"`
}

// toChannelDTO maps c. deviceEnabled is the state of its device: a channel
// of a disabled device is reported as disabled, like a disabled channel.
func toChannelDTO(c domain.Channel, deviceEnabled bool, queueDepth int) channelDTO {
	state, reason := c.State, c.StateReason
	if !c.Enabled || !deviceEnabled {
		state = domain.ChannelDisabled
		reason = "disabled"
		if c.Enabled {
			reason = "device disabled"
		}
	}
	if state != domain.ChannelOK && reason == "" {
		reason = "not checked yet"
	}
	if state == domain.ChannelOK {
		reason = ""
	}
	return channelDTO{
		ID: string(c.ID), DeviceID: string(c.DeviceID), Port: c.Port, Name: c.Name, Tags: append([]string{}, c.Tags...),
		RatePerMin: c.RatePerMin, MaxSegments: c.MaxSegments, Transliterate: c.Transliterate, Enabled: c.Enabled,
		State: string(state), StateReason: nullableString(reason), LastHealthAt: nullableTime(c.LastHealthAt),
		Signal: toSignalDTO(c.Signal), Operator: nullableString(c.Operator), QueueDepth: queueDepth,
	}
}

// deviceDTO is schema Device. There is deliberately no password field.
type deviceDTO struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	BaseURL           string  `json:"base_url"`
	Username          string  `json:"username"`
	TLSMode           string  `json:"tls_mode"`
	CAPEM             *string `json:"ca_pem"`
	Fingerprint       *string `json:"fingerprint"`
	Enabled           bool    `json:"enabled"`
	InsecureTransport bool    `json:"insecure_transport"`
	CreatedAt         string  `json:"created_at"`
}

func toDeviceDTO(d domain.Device) deviceDTO {
	return deviceDTO{
		ID: string(d.ID), Name: d.Name, BaseURL: d.BaseURL, Username: d.Username, TLSMode: string(d.TLSMode),
		CAPEM: nullableString(d.CAPEM), Fingerprint: nullableString(formatFingerprint(d.Fingerprint)),
		Enabled: d.Enabled, InsecureTransport: d.InsecureTransport(), CreatedAt: formatTime(d.CreatedAt),
	}
}

// formatFingerprint renders hex as "AB:CD:...", the form the contract returns.
func formatFingerprint(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(s, ":", ""))
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i+1 < len(s); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String()
}

// tokenDTO is schema Token (no secret, no hash).
type tokenDTO struct {
	ID         string   `json:"id"`
	UserID     string   `json:"user_id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	ExpiresAt  *string  `json:"expires_at"`
	LastUsedAt *string  `json:"last_used_at"`
	CreatedAt  string   `json:"created_at"`
}

func toTokenDTO(t domain.APIToken) tokenDTO {
	scopes := make([]string, len(t.Scopes))
	for i, s := range t.Scopes {
		scopes[i] = string(s)
	}
	return tokenDTO{
		ID: string(t.ID), UserID: string(t.UserID), Name: t.Name, Prefix: t.Prefix, Scopes: scopes,
		ExpiresAt: nullableTime(t.ExpiresAt), LastUsedAt: nullableTime(t.LastUsedAt), CreatedAt: formatTime(t.CreatedAt),
	}
}

// tokenCreatedDTO is schema TokenCreated: the only place a token secret is
// ever serialised.
type tokenCreatedDTO struct {
	tokenDTO
	Token string `json:"token"`
}

// userDTO is schema User (no password hash).
type userDTO struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
}

func toUserDTO(u domain.User) userDTO {
	return userDTO{ID: string(u.ID), Username: u.Username, Role: string(u.Role), Disabled: u.Disabled, CreatedAt: formatTime(u.CreatedAt)}
}

// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package connector defines how tikker talks to a device. The rest of the
// application (queue, inbox, health, device test in the API) only ever sees
// the [Connector] interface; implementations live in sub-packages:
//
//   - connector/routeros: RouterOS REST API, tikker calls the router (MVP, T12);
//   - connector/fake: in-process fake for unit tests (T13);
//   - connector/agent: pull model, the router calls tikker (post-MVP).
//
// # One connector per device
//
// A Connector instance is bound to one device: it owns that device's HTTP
// client, TLS settings and credentials. Channels (modem ports) of the device
// are addressed with [ChannelRef]. The application builds connectors from
// stored devices through a [Registry], which maps [domain.ConnectorKind]
// (Device.Connector) to a [Factory]:
//
//	reg := connector.NewRegistry()
//	reg.Register(domain.ConnectorRouterOSREST, routeros.New)
//	conn, err := reg.New(ctx, cfg) // cfg built from domain.Device + decrypted password
//
// There is no global registry and no init-time registration: cmd/tikker
// wires the factories explicitly.
//
// # Plugging in the pull-based agent connector
//
// With the agent connector, tikker cannot call the router; a RouterOS script
// polls tikker over HTTPS instead (works behind CGNAT). It still satisfies
// this interface, so queue, inbox and health need no changes:
//
//   - its Factory returns a per-device object backed by an in-memory mailbox
//     that an HTTP handler (mounted by the agent package, e.g. under
//     /agent/v1/) shares with it;
//   - Send puts an [OutboundSMS] into the mailbox and blocks until the agent
//     acknowledges it or ctx ends. A missing acknowledgement is reported as
//     domain.TransientAmbiguous, because the router may have picked the
//     command up. OutboundSMS.MessageID lets the agent and tikker
//     deduplicate redelivered commands;
//   - FetchInbox returns SMS the agent has pushed and DeleteInbox
//     acknowledges them, so the agent can remove them on the router - the
//     same store-then-delete order as with REST;
//   - Health is derived from the agent's last check-in and the modem status
//     it reports; a stale agent is a Transient error (channel down);
//   - Probe returns what the agent reported at its last check-in (version,
//     board, ports), or a Transient error until it has checked in.
//
// # Errors
//
// Every method returns either nil or an error classified with
// domain.Transient, domain.TransientAmbiguous, domain.Permanent or
// domain.AuthError (see [domain.ConnectorError]); unclassified errors are
// treated as Transient by callers. Errors must not contain secrets or message
// bodies.
//
// # Concurrency
//
// Implementations must be safe for concurrent use. The queue runs one send at
// a time per channel, but inbox polling, health checks and sends on other
// channels of the same device run concurrently.
package connector

import (
	"context"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Connector talks to one device.
type Connector interface {
	// Send hands one SMS to the device for sending on ch. Success means the
	// device accepted it (status "sent"); there are no delivery reports in
	// the MVP. Send must honour ctx: when ctx ends after the request was
	// written, return a domain.TransientAmbiguous error.
	Send(ctx context.Context, ch ChannelRef, msg OutboundSMS) (SendResult, error)

	// FetchInbox returns the SMS currently stored in the device inbox for
	// ch, oldest first. Multi-part messages are returned as the device
	// stores them; joining parts is the inbox poller's job.
	FetchInbox(ctx context.Context, ch ChannelRef) ([]InboundSMS, error)

	// DeleteInbox removes the given inbox entries (InboundSMS.RemoteID) from
	// the device. Callers invoke it only after the messages were committed
	// to the store. Deleting an entry that no longer exists is not an
	// error. On partial failure it returns an error; the poller retries on
	// the next cycle and the store's deduplication absorbs the repeats.
	DeleteInbox(ctx context.Context, ch ChannelRef, remoteIDs []string) error

	// Health reports the modem state of ch. An unreachable device is a
	// Transient error, bad credentials an Auth error.
	Health(ctx context.Context, ch ChannelRef) (ChannelHealth, error)

	// Probe checks connectivity, TLS and credentials and lists the device's
	// modem ports (the "test device" action, POST /devices/{id}/test). On
	// failure it returns an error carrying a domain.ErrorCode together with
	// whatever it learned before failing - in particular TLS.PeerFingerprint
	// after a certificate mismatch, so the UI can offer it for pinning.
	Probe(ctx context.Context) (DeviceProbe, error)

	// Close releases resources (idle connections, agent sessions). The
	// connector must not be used afterwards.
	Close() error
}

// ChannelRef addresses one modem port of the connector's device.
type ChannelRef struct {
	ChannelID domain.ID // for logging and correlation only
	Port      string    // RouterOS interface name, e.g. "lte1"
}

// OutboundSMS is a message to send. Body is already transliterated and
// within the channel's segment limit (internal/sms).
type OutboundSMS struct {
	MessageID domain.ID // idempotency/correlation key for connectors that support it
	To        string    // E.164 or an allowed short/alphanumeric number
	Body      string
	Encoding  domain.Encoding
	Segments  int
}

// SendResult describes an accepted SMS.
type SendResult struct {
	// RemoteRef is a device-side reference, if the device returns one.
	RemoteRef string
	// AcceptedAt is when the device accepted the message, as measured by
	// the connector (zero if unknown; the dispatcher then uses its clock).
	AcceptedAt time.Time
}

// InboundSMS is one entry of a device inbox.
type InboundSMS struct {
	// RemoteID identifies the entry on the device (RouterOS ".id", e.g.
	// "*1A") for DeleteInbox. It may be reused by the device after
	// deletion, so it is never used for deduplication.
	RemoteID string
	Phone    string // sender as reported by the device
	// RawTimestamp is the device's timestamp string, unparsed. It feeds
	// domain.InboundDedupHash. [WERYFIKACJA: RouterOS format]
	RawTimestamp string
	// Timestamp is RawTimestamp parsed in the device's time zone, or zero
	// if it could not be parsed.
	Timestamp time.Time
	Body      string
	// Type is the device's message type field, if any (RouterOS "type").
	Type string
	// Part describes the concatenation header of a multi-part SMS, if the
	// device exposes it; nil otherwise. [WERYFIKACJA]
	Part *PartInfo
}

// PartInfo is the concatenation info of one part of a multi-part SMS.
type PartInfo struct {
	Ref   int // reference number shared by all parts
	Index int // 1-based
	Total int
}

// ChannelHealth is a snapshot of a modem port. Fields the device does not
// report stay at their zero value. The health checker (internal/health)
// turns snapshots into domain.ChannelState.
type ChannelHealth struct {
	CheckedAt  time.Time
	Running    bool // interface is up
	Registered bool // attached to a network
	// Registration is the raw registration state, e.g. "registered",
	// "searching", "no-sim". [WERYFIKACJA T00]
	Registration string
	Operator     string
	Technology   string         // e.g. "LTE", "5G NSA"
	Signal       *domain.Signal // nil when not reported
}

// DeviceProbe is the result of [Connector.Probe].
type DeviceProbe struct {
	RouterOSVersion string // e.g. "7.16.1 (stable)"
	BoardName       string
	TLS             *TLSInfo // nil for http:// devices
	Ports           []PortInfo
}

// TLSInfo describes the TLS session observed by Probe.
type TLSInfo struct {
	Mode            domain.TLSMode
	Verified        bool
	PeerFingerprint string // SHA-256 of the leaf certificate, "AB:CD:..." form
}

// PortInfo describes a modem port found by Probe.
type PortInfo struct {
	Port   string // interface name, e.g. "lte1"
	Health ChannelHealth
}

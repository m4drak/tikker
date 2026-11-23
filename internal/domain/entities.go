// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"net/url"
	"strings"
	"time"
)

// Actor identifies who created a message or performed an audited action.
// Its string form is "user:<id>", "token:<id>" or "system".
type Actor struct {
	Type ActorType
	ID   ID // empty for ActorSystem
}

// SystemActor is the actor for actions taken by tikker itself.
var SystemActor = Actor{Type: ActorSystem}

// UserActor returns the actor for a panel user.
func UserActor(id ID) Actor { return Actor{Type: ActorUser, ID: id} }

// TokenActor returns the actor for an API token.
func TokenActor(id ID) Actor { return Actor{Type: ActorToken, ID: id} }

// String returns the canonical string form.
func (a Actor) String() string {
	if a.Type == ActorSystem {
		return string(ActorSystem)
	}
	return string(a.Type) + ":" + string(a.ID)
}

// IsZero reports whether the actor is unset (e.g. CreatedBy of an inbound
// message, exposed as null in the API).
func (a Actor) IsZero() bool { return a.Type == "" && a.ID == "" }

// ParseActor parses the canonical string form produced by [Actor.String].
// The empty string parses to the zero Actor.
func ParseActor(s string) (Actor, error) {
	if s == "" {
		return Actor{}, nil
	}
	if s == string(ActorSystem) {
		return SystemActor, nil
	}
	typ, id, ok := strings.Cut(s, ":")
	if !ok || (typ != string(ActorUser) && typ != string(ActorToken)) {
		return Actor{}, &ParseError{Type: "actor", Value: s}
	}
	pid, err := ParseID(id)
	if err != nil {
		return Actor{}, &ParseError{Type: "actor", Value: s}
	}
	return Actor{Type: ActorType(typ), ID: pid}, nil
}

// MarshalText implements encoding.TextMarshaler. The zero Actor marshals to
// an empty string.
func (a Actor) MarshalText() ([]byte, error) {
	if a.IsZero() {
		return []byte{}, nil
	}
	if _, err := ParseActor(a.String()); err != nil {
		return nil, err
	}
	return []byte(a.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Actor) UnmarshalText(b []byte) error {
	v, err := ParseActor(string(b))
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// User is a panel account.
type User struct {
	ID           ID
	Username     string
	PasswordHash string // argon2id in PHC string format; never exposed
	Role         Role
	Disabled     bool
	CreatedAt    time.Time
}

// APIToken is a bearer token for the REST API. The secret is shown to the
// user once, as "tkr_<prefix>_<secret>"; only its SHA-256 hash is stored.
type APIToken struct {
	ID         ID
	UserID     ID
	Name       string
	Prefix     string // public, used to look the token up
	Hash       []byte // SHA-256 of the secret; compare in constant time
	Scopes     Scopes
	ExpiresAt  time.Time // zero: never expires
	LastUsedAt time.Time // zero: never used
	CreatedAt  time.Time
}

// Expired reports whether the token is expired at now.
func (t APIToken) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// Device is a MikroTik router reachable through a connector.
type Device struct {
	ID          ID
	Name        string
	Connector   ConnectorKind // routeros-rest in the MVP
	BaseURL     string        // e.g. https://172.17.0.1
	Username    string
	PasswordEnc []byte // AES-256-GCM ciphertext (internal/secrets); write-only in API/UI
	// AgentTokenEnc is the sealed bearer token the router script presents
	// to /agent/v1 (ConnectorAgent only; internal/secrets); nil when the
	// device has none. Write-only in API/UI like the password.
	AgentTokenEnc []byte
	TLSMode       TLSMode
	CAPEM         string // for TLSCustomCA
	Fingerprint   string // hex SHA-256 of the certificate, for TLSFingerprint
	Enabled       bool
	CreatedAt     time.Time
}

// InsecureTransport reports whether traffic to the device is unencrypted or
// unauthenticated (http:// base URL or TLSInsecure); the UI shows a warning.
// An unparseable BaseURL counts as insecure.
func (d Device) InsecureTransport() bool {
	if d.Connector == ConnectorAgent {
		// tikker never connects to an agent device: the router calls
		// tikker's own listener, whose TLS is configured server-wide.
		return false
	}
	u, err := url.Parse(d.BaseURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return true
	}
	return d.TLSMode == TLSInsecure
}

// Signal holds modem radio metrics. A nil field was not reported.
type Signal struct {
	RSSI *int // dBm
	RSRP *int // dBm
	RSRQ *int // dB
	SINR *int // dB
}

// Channel is one SIM/modem port of a device ("lte1"). Messages are sent and
// received through channels.
type Channel struct {
	ID            ID
	DeviceID      ID
	Port          string // RouterOS interface name, e.g. "lte1"
	Name          string
	Tags          []string
	RatePerMin    int  // token bucket refill rate
	MaxSegments   int  // longer messages are rejected with 422
	Transliterate bool // replace Polish diacritics with ASCII before sending
	Enabled       bool

	// Health, maintained by the health checker.
	State        ChannelState
	StateReason  string // human-readable cause of degraded/down, e.g. "RouterOS returned 401"
	LastHealthAt time.Time
	Signal       *Signal // nil when unknown
	Operator     string
}

// HasTags reports whether the channel carries every tag in tags. An empty
// tags list matches every channel.
func (c Channel) HasTags(tags []string) bool {
	for _, want := range tags {
		found := false
		for _, have := range c.Tags {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Route holds the routing constraints a client attached to an outbound
// message (SPEC §4.4). The dispatcher resolves it to a concrete channel only
// when it acquires the message, so a channel going down never strands
// messages that were not pinned to it.
type Route struct {
	ChannelID ID       // pinned channel; empty when not requested
	Tags      []string // the sending channel must carry all of them
}

// LastErrorExpired prefixes Message.LastError of a message that failed
// because it was still queued at its ExpiresAt.
const LastErrorExpired = "expired"

// Message is an outbound or inbound SMS.
type Message struct {
	ID        ID
	Direction Direction
	// ChannelID is the channel that sent, is sending or received the message.
	// For a queued outbound message it is empty unless Route pins a channel.
	// It is set at enqueue time only when Route pins a channel; otherwise
	// the dispatcher sets it on acquire (API: null until routed).
	ChannelID ID
	Route     Route  // outbound only; Route.Tags is the API "tags" field
	Peer      string // E.164 (or short/alphanumeric when allowed)
	// Body is the text. PurgeBodies empties it when retention expires; the
	// API reports an empty body of a terminal message as null.
	Body     string
	Encoding Encoding
	Segments int
	Status   MessageStatus

	// Outbound delivery bookkeeping.
	Attempts      int       // send attempts started; incremented on acquire
	NextAttemptAt time.Time // not acquired before this time
	LeaseUntil    time.Time // set while StatusSending
	LastError     string    // starts with LastErrorExpired after expiry
	ExpiresAt     time.Time // zero: never; queued past this time -> failed
	// PossibleDuplicate is set when tikker cannot know whether a previous
	// attempt reached the router (expired lease or ambiguous send error) and
	// the message was retried anyway. See ADR 0002.
	PossibleDuplicate bool

	// Client-provided references.
	IdempotencyKey string // shared by all messages of one request; see store.Idempotency
	ClientRef      string

	CreatedBy  Actor // zero for inbound messages
	CreatedAt  time.Time
	SentAt     time.Time // outbound: accepted by RouterOS
	ReceivedAt time.Time // inbound: router timestamp, or fetch time if unparseable
	DedupHash  string    // inbound only; see InboundDedupHash
}

// Lease returns the lease held on an outbound message in StatusSending. Store
// methods that complete a send take the lease and succeed only if it is still
// current, which fences off a worker whose lease expired and was recovered.
func (m Message) Lease() Lease {
	return Lease{MessageID: m.ID, ChannelID: m.ChannelID, Attempt: m.Attempts, Until: m.LeaseUntil}
}

// Lease is a dispatcher's exclusive claim on an outbound message.
//
// Attempt doubles as the fencing token: every acquisition increments
// Message.Attempts, so a stale lease never matches the stored row.
type Lease struct {
	MessageID ID
	ChannelID ID
	Attempt   int
	Until     time.Time
}

// AuditAction names an audited action. The constants below are the actions
// required by SPEC §7; other packages may define more using the
// "<object>.<verb>" convention.
type AuditAction string

// Audited actions.
const (
	AuditLoginSuccess  AuditAction = "auth.login"
	AuditLoginFailure  AuditAction = "auth.login_failed"
	AuditLogout        AuditAction = "auth.logout"
	AuditUserCreate    AuditAction = "user.create"
	AuditUserUpdate    AuditAction = "user.update"
	AuditTokenCreate   AuditAction = "token.create"
	AuditTokenDelete   AuditAction = "token.delete"
	AuditDeviceCreate  AuditAction = "device.create"
	AuditDeviceUpdate  AuditAction = "device.update"
	AuditDeviceDelete  AuditAction = "device.delete"
	AuditChannelUpdate AuditAction = "channel.update"
	AuditMessageSend   AuditAction = "message.send"
	AuditMessageCancel AuditAction = "message.cancel"
)

// AuditLog is an append-only record of a security-relevant action.
// Meta must never contain secrets or message bodies.
type AuditLog struct {
	ID     ID
	Actor  Actor
	Action AuditAction
	Target string // e.g. "device:<id>"
	Meta   map[string]string
	At     time.Time
}

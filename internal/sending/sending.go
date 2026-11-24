// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package sending is the outbound message service shared by the REST API
// (internal/api) and the web panel (internal/web): validation of a send
// request, channel selection, text planning (sms.Plan) and enqueueing.
//
// # Planning
//
// The text of an outbound message is planned once, at write time, because
// the store keeps one body per message (SPEC §4.6, §12.3). The channel that
// finally sends the message is chosen later by the queue, so the plan must be
// valid for every channel that may be chosen:
//
//   - channel_id given: that channel (unknown: unknown_channel, disabled or
//     on a disabled device: channel_disabled);
//   - otherwise the enabled channels (on enabled devices) that carry all the
//     given tags, or all enabled channels without tags; none:
//     no_matching_channel. Channels that are down or degraded still count:
//     the message waits in the queue.
//
// Over the candidate channels the strictest values are used: the encodings
// every connector supports, the smallest connector MaxSegments, concatenation
// only if all connectors support it, the smallest channel max_segments and
// transliteration if any channel has it enabled (so the one stored body is
// sendable everywhere). With a single candidate these are its own values.
//
// # Use
//
//	svc, _ := sending.New(sending.Deps{Store: st, Capabilities: sync, Notifier: disp, Options: ...})
//
//	rt, _ := svc.LoadRouting(ctx)                        // once per request
//	msgs, errs, err := svc.Plan(rt, req, "", actor)      // errs: validation problems
//	res, err := svc.Enqueue(ctx, msgs, idem)             // stores and wakes the queue
//
//	res, errs, err := svc.Send(ctx, actor, req, opts)    // all of the above plus an audit entry (web panel)
//	pv, err := svc.Preview(ctx, sending.PreviewRequest{...}) // live counter of the send form
//
// Field errors carry the codes of api/openapi.yaml and a JSON pointer in
// Field (prefixed with the pointer given to Plan), so the API can report them
// as they are and the panel can show them next to the form fields.
package sending

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms"
	"github.com/m4drak/tikker/internal/sms/gsm"
	"github.com/m4drak/tikker/internal/sms/phone"
	"github.com/m4drak/tikker/internal/store"
)

// Limits of schema SendMessageRequest and Tag.
const (
	MaxToLen     = 32
	MaxBodyChars = 1600
	MaxClientRef = 128
	MaxTags      = 16
	MaxTagLen    = 32
	MaxIDLen     = 64
)

// Validation codes (errors[].code of api/openapi.yaml) produced here.
const (
	CodeRequired          = "required"
	CodeInvalidFormat     = "invalid_format"
	CodeOutOfRange        = "out_of_range"
	CodeInvalidPhone      = "invalid_phone_number"
	CodeUnknownChannel    = "unknown_channel"
	CodeChannelDisabled   = "channel_disabled"
	CodeNoMatchingChannel = "no_matching_channel"
	CodeExpiresInPast     = "expires_at_in_past"
)

var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// CapabilityProvider reports what the connector that serves a channel can
// send. ok is false when the channel has no usable connector. The API's
// *ChannelSync implements it.
type CapabilityProvider interface {
	ChannelCapabilities(id domain.ID) (caps connector.Capabilities, ok bool)
}

// Notifier wakes the queue after messages were enqueued; *queue.Dispatcher
// implements it.
type Notifier interface{ Notify() }

// Options are the settings of the service (from internal/config).
type Options struct {
	// DefaultRegion is the region of numbers without a country prefix;
	// empty means "PL".
	DefaultRegion string
	// AllowShortNumbers accepts short codes and alphanumeric numbers.
	AllowShortNumbers bool
	// LongMessageStrategy is what to do with text longer than one send;
	// empty means reject.
	LongMessageStrategy sms.Strategy
}

// Deps are the collaborators of the [Service]. Store, Capabilities and
// Notifier are required; the others have defaults.
type Deps struct {
	Store        store.Store
	Capabilities CapabilityProvider
	Notifier     Notifier

	Clock clock.Clock        // default: system clock
	IDs   domain.IDGenerator // default: ULIDs from Clock

	Options Options
}

// Service validates, plans and enqueues outbound messages.
type Service struct {
	st   store.Store
	caps CapabilityProvider
	note Notifier
	clk  clock.Clock
	ids  domain.IDGenerator
	opts Options
}

// New validates deps and returns the service.
func New(d Deps) (*Service, error) {
	var missing []string
	for _, m := range []struct {
		name string
		nil  bool
	}{
		{"Store", d.Store == nil}, {"Capabilities", d.Capabilities == nil}, {"Notifier", d.Notifier == nil},
	} {
		if m.nil {
			missing = append(missing, m.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("sending: missing dependencies: %s", strings.Join(missing, ", "))
	}
	s := &Service{st: d.Store, caps: d.Capabilities, note: d.Notifier, clk: d.Clock, ids: d.IDs, opts: d.Options}
	if s.clk == nil {
		s.clk = clock.System()
	}
	if s.ids == nil {
		s.ids = domain.NewULIDGenerator(s.clk)
	}
	if s.opts.DefaultRegion == "" {
		s.opts.DefaultRegion = "PL"
	}
	if _, err := sms.ParseStrategy(string(s.opts.LongMessageStrategy)); err != nil {
		return nil, fmt.Errorf("sending: %w", err)
	}
	return s, nil
}

// Request is schema SendMessageRequest. Fields are pointers so that a missing
// member can be told from an empty one.
type Request struct {
	To        *string
	Body      *string
	ChannelID *string
	Tags      []string
	ClientRef *string
	ExpiresAt *string // RFC 3339
}

// FieldError is one validation problem. Field is a JSON pointer ("/to",
// "/tags/1"; prefixed with the pointer given to [Service.Plan]); it is the
// pointer itself ("" for a single request) for a problem of the whole request,
// such as no matching channel.
type FieldError struct {
	Field  string
	Code   string
	Detail string
}

func errf(field, code, format string, args ...any) FieldError {
	return FieldError{Field: field, Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Routing is a snapshot of channels and devices, read once per request.
type Routing struct {
	channels []domain.Channel
	devices  map[domain.ID]domain.Device
}

// LoadRouting reads the channels and devices.
func (s *Service) LoadRouting(ctx context.Context) (*Routing, error) {
	chs, err := s.st.Channels().List(ctx, store.ChannelFilter{})
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	devs, err := s.st.Devices().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	rt := &Routing{channels: chs, devices: make(map[domain.ID]domain.Device, len(devs))}
	for _, d := range devs {
		rt.devices[d.ID] = d
	}
	return rt, nil
}

// Usable returns the enabled channels on enabled devices, in store order.
func (rt *Routing) Usable() []domain.Channel {
	var out []domain.Channel
	for _, c := range rt.channels {
		if rt.usable(c) {
			out = append(out, c)
		}
	}
	return out
}

func (rt *Routing) usable(c domain.Channel) bool {
	d, ok := rt.devices[c.DeviceID]
	return c.Enabled && ok && d.Enabled
}

func (rt *Routing) channel(id domain.ID) (domain.Channel, bool) {
	for _, c := range rt.channels {
		if c.ID == id {
			return c, true
		}
	}
	return domain.Channel{}, false
}

// target is the outcome of channel selection for one message.
type target struct {
	channels []domain.Channel
	caps     connector.Capabilities
	opts     sms.Options
}

// selectChannels applies the routing rules of the package documentation:
// channel_id, else tags, else every enabled channel. On failure it returns
// the validation code and detail.
func (s *Service) selectChannels(rt *Routing, channelID domain.ID, tags []string) (target, string, string) {
	var cands []domain.Channel
	switch {
	case channelID != "":
		c, ok := rt.channel(channelID)
		if !ok {
			return target{}, CodeUnknownChannel, "Channel does not exist."
		}
		if !rt.usable(c) {
			return target{}, CodeChannelDisabled, "Channel (or its device) is disabled."
		}
		cands = []domain.Channel{c}
	default:
		for _, c := range rt.channels {
			if rt.usable(c) && c.HasTags(tags) {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			if len(tags) > 0 {
				return target{}, CodeNoMatchingChannel, "No enabled channel carries all the given tags."
			}
			return target{}, CodeNoMatchingChannel, "There is no enabled channel."
		}
	}

	// Keep the channels with a working connector and combine their
	// capabilities (strictest values).
	var t target
	for _, c := range cands {
		caps, ok := s.caps.ChannelCapabilities(c.ID)
		if !ok {
			continue
		}
		if len(t.channels) == 0 {
			t.caps = connector.Capabilities{
				Encodings: append([]domain.Encoding(nil), caps.Encodings...), MaxSegments: caps.MaxSegments,
				Concatenation: caps.Concatenation,
			}
			t.opts = sms.Options{MaxSegments: c.MaxSegments, Transliterate: c.Transliterate}
		} else {
			t.caps = strictest(t.caps, caps)
			t.opts.MaxSegments = min(t.opts.MaxSegments, c.MaxSegments)
			t.opts.Transliterate = t.opts.Transliterate || c.Transliterate
		}
		t.channels = append(t.channels, c)
	}
	if len(t.channels) == 0 {
		if channelID != "" {
			return target{}, CodeChannelDisabled, "Channel has no working connector."
		}
		return target{}, CodeNoMatchingChannel, "No matching channel has a working connector."
	}
	t.opts.Strategy = s.opts.LongMessageStrategy
	return t, "", ""
}

// strictest returns capabilities that every one of a and b can satisfy.
func strictest(a, b connector.Capabilities) connector.Capabilities {
	out := connector.Capabilities{
		MaxSegments:   min(a.MaxSegments, b.MaxSegments),
		Concatenation: a.Concatenation && b.Concatenation,
	}
	for _, e := range a.Encodings {
		if b.SupportsEncoding(e) {
			out.Encodings = append(out.Encodings, e)
		}
	}
	return out
}

// ValidateTags checks tags (schema Tags and Tag) and returns the problems;
// field is the JSON pointer of the array.
func ValidateTags(field string, tags []string) []FieldError {
	var errs []FieldError
	if len(tags) > MaxTags {
		errs = append(errs, errf(field, CodeOutOfRange, "At most %d tags are allowed.", MaxTags))
	}
	seen := map[string]bool{}
	for i, t := range tags {
		f := fmt.Sprintf("%s/%d", field, i)
		switch {
		case len(t) == 0 || len(t) > MaxTagLen:
			errs = append(errs, errf(f, CodeOutOfRange, "A tag has 1 to %d characters.", MaxTagLen))
		case !tagPattern.MatchString(t):
			errs = append(errs, errf(f, CodeInvalidFormat, "A tag consists of lower-case letters, digits, '_', '.' and '-' and starts with a letter or digit."))
		case seen[t]:
			errs = append(errs, errf(f, CodeInvalidFormat, "Tags must be unique."))
		}
		seen[t] = true
	}
	return errs
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// parseTimestamp parses an RFC 3339 timestamp.
func parseTimestamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// Plan validates one request, selects channels, plans the text and returns the
// messages (one, or several parts) to enqueue. pointer is the JSON pointer of
// the request inside the body ("" for a single request, "/messages/3" in a
// batch). Validation problems are returned as errs (the messages are nil
// then); err is an internal failure.
func (s *Service) Plan(rt *Routing, req Request, pointer string, actor domain.Actor) (msgs []domain.Message, errs []FieldError, err error) {
	f := func(name string) string { return pointer + "/" + name }

	var to phone.Number
	switch {
	case req.To == nil || *req.To == "":
		errs = append(errs, errf(f("to"), CodeRequired, "A recipient number is required."))
	case runeLen(*req.To) > MaxToLen:
		errs = append(errs, errf(f("to"), CodeOutOfRange, "At most %d characters.", MaxToLen))
	default:
		n, nerr := phone.Normalize(*req.To, phone.Options{DefaultRegion: s.opts.DefaultRegion, AllowShort: s.opts.AllowShortNumbers})
		switch {
		case errors.Is(nerr, phone.ErrInvalidNumber):
			errs = append(errs, errf(f("to"), CodeInvalidPhone, "Not a valid phone number for region %s.", strings.ToUpper(s.opts.DefaultRegion)))
		case nerr != nil:
			return nil, nil, fmt.Errorf("normalize number: %w", nerr) // misconfigured region
		default:
			to = n
		}
	}

	switch {
	case req.Body == nil || *req.Body == "":
		errs = append(errs, errf(f("body"), CodeRequired, "A message body is required."))
	case runeLen(*req.Body) > MaxBodyChars:
		errs = append(errs, errf(f("body"), CodeOutOfRange, "At most %d characters.", MaxBodyChars))
	}

	var channelID domain.ID
	if req.ChannelID != nil {
		switch {
		case *req.ChannelID == "" || len(*req.ChannelID) > MaxIDLen:
			errs = append(errs, errf(f("channel_id"), CodeInvalidFormat, "A channel ID has 1 to %d characters.", MaxIDLen))
		default:
			channelID = domain.ID(*req.ChannelID)
		}
	}
	errs = append(errs, ValidateTags(f("tags"), req.Tags)...)

	if req.ClientRef != nil && runeLen(*req.ClientRef) > MaxClientRef {
		errs = append(errs, errf(f("client_ref"), CodeOutOfRange, "At most %d characters.", MaxClientRef))
	}
	var expires time.Time
	hasExpiry := false
	if req.ExpiresAt != nil {
		t, ok := parseTimestamp(*req.ExpiresAt)
		switch {
		case !ok:
			errs = append(errs, errf(f("expires_at"), CodeInvalidFormat, "Must be an RFC 3339 timestamp."))
		case !t.After(s.clk.Now()):
			errs = append(errs, errf(f("expires_at"), CodeExpiresInPast, "expires_at must be in the future."))
		default:
			expires, hasExpiry = t, true
		}
	}
	if len(errs) > 0 {
		return nil, errs, nil
	}

	t, code, detail := s.selectChannels(rt, channelID, req.Tags)
	if code != "" {
		return nil, []FieldError{errf(channelField(pointer, channelID, req.Tags), code, "%s", detail)}, nil
	}
	res, perr := sms.Plan(*req.Body, t.caps, t.opts)
	if perr != nil {
		code := sms.ErrorCode(perr)
		switch {
		case code == "":
			return nil, nil, fmt.Errorf("plan message: %w", perr)
		case errors.Is(perr, sms.ErrUnsupportedEncoding):
			return nil, []FieldError{errf(f("body"), code, "%s", unsupportedDetail(*req.Body))}, nil
		default:
			return nil, []FieldError{errf(f("body"), code, "%s", planDetail(perr))}, nil
		}
	}

	now := s.clk.Now()
	msgs = make([]domain.Message, len(res.Parts))
	for i, part := range res.Parts {
		m := domain.Message{
			ID: s.ids.NewID(), Direction: domain.DirectionOut,
			Route: domain.Route{ChannelID: channelID, Tags: append([]string(nil), req.Tags...)},
			Peer:  to.Value, Body: part.Text, Encoding: res.Encoding, Segments: part.Segments,
			Status: domain.StatusQueued, NextAttemptAt: now, CreatedBy: actor, CreatedAt: now,
		}
		if req.ClientRef != nil {
			m.ClientRef = *req.ClientRef
		}
		if hasExpiry {
			m.ExpiresAt = expires
		}
		msgs[i] = m
	}
	return msgs, nil, nil
}

// channelField is the field a routing problem is attached to.
func channelField(pointer string, channelID domain.ID, tags []string) string {
	switch {
	case channelID != "":
		return pointer + "/channel_id"
	case len(tags) > 0:
		return pointer + "/tags"
	}
	return pointer
}

// unsupportedDetail explains code unsupported_characters: the text needs an
// encoding (UCS-2) that the channels which may send it cannot use, and
// transliteration is off for them.
func unsupportedDetail(body string) string {
	for _, c := range body {
		if !gsm.InGSM(c) {
			return fmt.Sprintf("Character %q is outside the GSM-7 alphabet, and the channel sends GSM-7 only; "+
				"enable transliterate on the channel or remove such characters.", c)
		}
	}
	return "The channel cannot send this text in any encoding it supports."
}

// planDetail turns a planner error into a sentence for the client.
func planDetail(err error) string {
	msg := strings.TrimPrefix(err.Error(), "sms: ")
	r := []rune(msg)
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	msg = string(r)
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// Enqueue stores msgs (and the idempotency record, see store.MessageRepo)
// in one transaction and wakes the queue unless the request was replayed.
// Errors are returned unwrapped so that callers can test the store sentinels
// (domain.ErrIdempotencyMismatch).
func (s *Service) Enqueue(ctx context.Context, msgs []domain.Message, idem store.Idempotency) (store.EnqueueResult, error) {
	res, err := s.st.Messages().Enqueue(ctx, msgs, idem)
	if err != nil {
		return store.EnqueueResult{}, err //nolint:wrapcheck // callers test sentinels and add context
	}
	if !res.Replayed {
		s.note.Notify()
	}
	return res, nil
}

// SendOptions tune [Service.Send] and [Service.SendBatch].
type SendOptions struct {
	// IdempotencyKey, when not empty, makes a repeated submission of the
	// same request return the original messages (the web form sends a nonce
	// so that a double click or a reload does not send twice; the SMTP
	// gateway uses the Message-ID). A different request under the same key
	// yields domain.ErrIdempotencyMismatch.
	IdempotencyKey string
	// Source is recorded in the audit entry ("source" meta); empty means
	// "ui".
	Source string
}

// Send validates, plans and enqueues one request on behalf of actor and
// records an audit entry (domain.AuditMessageSend; neither the number nor the
// text is stored in it). Validation problems are returned as errs. It is the
// entry point of the web panel; the API plans and enqueues itself because it
// batches requests.
func (s *Service) Send(ctx context.Context, actor domain.Actor, req Request, so SendOptions) (store.EnqueueResult, []FieldError, error) {
	return s.SendBatch(ctx, actor, []Request{req}, so)
}

// SendBatch is [Service.Send] for several requests that are stored
// atomically: either every message is enqueued or none (one SMTP e-mail to
// several recipients). Field errors of request i carry the pointer
// "/messages/i" when there is more than one request. With an idempotency
// key a replay is answered before validation, as the API does, so a retry
// succeeds even if routing changed meanwhile. One audit entry is written.
func (s *Service) SendBatch(ctx context.Context, actor domain.Actor, reqs []Request, so SendOptions) (store.EnqueueResult, []FieldError, error) {
	if len(reqs) == 0 {
		return store.EnqueueResult{}, nil, errors.New("sending: no requests")
	}
	var idem store.Idempotency
	if so.IdempotencyKey != "" {
		idem = store.Idempotency{
			Key: so.IdempotencyKey, RequestHash: batchHash(reqs),
			Since: s.clk.Now().Add(-24 * time.Hour),
		}
		res, ok, err := s.st.Messages().Replay(ctx, actor, idem)
		if err != nil {
			if errors.Is(err, domain.ErrIdempotencyMismatch) {
				return store.EnqueueResult{}, nil, err //nolint:wrapcheck // sentinel tested by callers
			}
			return store.EnqueueResult{}, nil, fmt.Errorf("replay: %w", err)
		}
		if ok {
			return res, nil, nil
		}
	}
	rt, err := s.LoadRouting(ctx)
	if err != nil {
		return store.EnqueueResult{}, nil, err
	}
	var msgs []domain.Message
	var errs []FieldError
	groups := make([]int, 0, len(reqs))
	for i, req := range reqs {
		pointer := ""
		if len(reqs) > 1 {
			pointer = fmt.Sprintf("/messages/%d", i)
		}
		parts, perrs, err := s.Plan(rt, req, pointer, actor)
		if err != nil {
			return store.EnqueueResult{}, nil, err
		}
		errs = append(errs, perrs...)
		msgs = append(msgs, parts...)
		groups = append(groups, len(parts))
	}
	if len(errs) > 0 {
		return store.EnqueueResult{}, errs, nil
	}
	if len(reqs) > 1 {
		idem.Groups = groups
	}
	source := so.Source
	if source == "" {
		source = "ui"
	}
	var res store.EnqueueResult
	err = s.st.WithTx(ctx, func(tx store.Repos) error {
		var err error
		if res, err = tx.Messages().Enqueue(ctx, msgs, idem); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if res.Replayed {
			return nil
		}
		meta := map[string]string{"parts": fmt.Sprint(len(res.Messages)), "source": source}
		if len(reqs) > 1 {
			meta["recipients"] = fmt.Sprint(len(reqs))
		}
		return tx.Audit().Append(ctx, domain.AuditLog{
			ID: s.ids.NewID(), Actor: actor, Action: domain.AuditMessageSend,
			Target: "message:" + string(res.Messages[0].ID), Meta: meta, At: s.clk.Now(),
		})
	})
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyMismatch) {
			return store.EnqueueResult{}, nil, err
		}
		return store.EnqueueResult{}, nil, fmt.Errorf("enqueue: %w", err)
	}
	if !res.Replayed {
		s.note.Notify()
	}
	return res, nil, nil
}

// batchHash is the idempotency digest of reqs; for one request it equals
// requestHash, so records written by earlier versions still replay.
func batchHash(reqs []Request) string {
	if len(reqs) == 1 {
		return requestHash(reqs[0])
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "batch:%d", len(reqs))
	for _, r := range reqs {
		_, _ = fmt.Fprintf(h, ":%s", requestHash(r))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// requestHash is a digest of the request for the idempotency record.
func requestHash(req Request) string {
	h := sha256.New()
	str := func(p *string) {
		if p == nil {
			_, _ = h.Write([]byte{0})
			return
		}
		_, _ = fmt.Fprintf(h, "1%d:%s", len(*p), *p)
	}
	str(req.To)
	str(req.Body)
	str(req.ChannelID)
	str(req.ClientRef)
	str(req.ExpiresAt)
	_, _ = fmt.Fprintf(h, "%q", req.Tags)
	return hex.EncodeToString(h.Sum(nil))
}

// Cancel moves a queued message to cancelled and records an audit entry in
// the same transaction. Errors are returned unwrapped (domain.ErrNotFound;
// domain.ErrInvalidTransition, with the message in its current state, when it
// is no longer queued) so that callers can map them.
func (s *Service) Cancel(ctx context.Context, actor domain.Actor, id domain.ID) (domain.Message, error) {
	var m domain.Message
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		var err error
		if m, err = tx.Messages().Cancel(ctx, id); err != nil {
			return err //nolint:wrapcheck // sentinels are tested by the caller
		}
		return tx.Audit().Append(ctx, domain.AuditLog{
			ID: s.ids.NewID(), Actor: actor, Action: domain.AuditMessageCancel,
			Target: "message:" + string(id), At: s.clk.Now(),
		})
	})
	return m, err //nolint:wrapcheck // see above
}

// NormalizePeer turns a number typed by a user into the form stored with
// messages (E.164) for filtering by peer. Inbound peers can be alphanumeric
// senders that never normalise, so anything that does not normalise is
// returned as it is.
func (s *Service) NormalizePeer(peer string) string {
	n, err := phone.Normalize(peer, phone.Options{DefaultRegion: s.opts.DefaultRegion, AllowShort: s.opts.AllowShortNumbers})
	if err != nil {
		return peer
	}
	return n.Value
}

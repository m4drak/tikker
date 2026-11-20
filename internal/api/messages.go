// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms"
	"github.com/m4drak/tikker/internal/sms/gsm"
	"github.com/m4drak/tikker/internal/sms/phone"
	"github.com/m4drak/tikker/internal/store"
)

// Limits of schema SendMessageRequest and Tag.
const (
	maxToLen       = 32
	maxBodyChars   = 1600
	maxClientRef   = 128
	maxTags        = 16
	maxTagLen      = 32
	maxIDLen       = 64
	replayedHeader = "Idempotent-Replayed"
	opSendMessage  = "POST /messages"
	opSendBatch    = "POST /messages/batch"
)

var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// sendRequest is schema SendMessageRequest. Fields are pointers so that a
// missing member can be told from an empty one.
type sendRequest struct {
	To        *string  `json:"to"`
	Body      *string  `json:"body"`
	ChannelID *string  `json:"channel_id"`
	Tags      []string `json:"tags"`
	ClientRef *string  `json:"client_ref"`
	ExpiresAt *string  `json:"expires_at"`
}

// batchRequest is schema SendBatchRequest.
type batchRequest struct {
	Messages []sendRequest `json:"messages"`
}

// routingView is a snapshot of channels and devices read once per request.
type routingView struct {
	channels []domain.Channel
	devices  map[domain.ID]domain.Device
}

func (a *API) loadRouting(ctx context.Context) (*routingView, error) {
	chs, err := a.st.Channels().List(ctx, store.ChannelFilter{})
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	devs, err := a.st.Devices().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	rv := &routingView{channels: chs, devices: make(map[domain.ID]domain.Device, len(devs))}
	for _, d := range devs {
		rv.devices[d.ID] = d
	}
	return rv, nil
}

// usable reports whether c and its device are enabled.
func (rv *routingView) usable(c domain.Channel) bool {
	d, ok := rv.devices[c.DeviceID]
	return c.Enabled && ok && d.Enabled
}

func (rv *routingView) channel(id domain.ID) (domain.Channel, bool) {
	for _, c := range rv.channels {
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
func (a *API) selectChannels(rv *routingView, channelID domain.ID, tags []string) (target, string, string) {
	var cands []domain.Channel
	switch {
	case channelID != "":
		c, ok := rv.channel(channelID)
		if !ok {
			return target{}, codeUnknownChannel, "Channel does not exist."
		}
		if !rv.usable(c) {
			return target{}, codeChannelDisabled, "Channel (or its device) is disabled."
		}
		cands = []domain.Channel{c}
	default:
		for _, c := range rv.channels {
			if rv.usable(c) && c.HasTags(tags) {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			if len(tags) > 0 {
				return target{}, codeNoMatchingChannel, "No enabled channel carries all the given tags."
			}
			return target{}, codeNoMatchingChannel, "There is no enabled channel."
		}
	}

	// Keep the channels with a working connector and combine their
	// capabilities (strictest values).
	var t target
	for _, c := range cands {
		caps, ok := a.caps.ChannelCapabilities(c.ID)
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
			return target{}, codeChannelDisabled, "Channel has no working connector."
		}
		return target{}, codeNoMatchingChannel, "No matching channel has a working connector."
	}
	t.opts.Strategy = a.opts.LongMessageStrategy
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

// validateTags checks Tags (schema Tags and Tag).
func validateTags(v *validation, field string, tags []string) {
	if len(tags) > maxTags {
		v.add(field, codeOutOfRange, "At most %d tags are allowed.", maxTags)
	}
	seen := map[string]bool{}
	for i, t := range tags {
		f := fmt.Sprintf("%s/%d", field, i)
		switch {
		case len(t) == 0 || len(t) > maxTagLen:
			v.add(f, codeOutOfRange, "A tag has 1 to %d characters.", maxTagLen)
		case !tagPattern.MatchString(t):
			v.add(f, codeInvalidFormat, "A tag consists of lower-case letters, digits, '_', '.' and '-' and starts with a letter or digit.")
		case seen[t]:
			v.add(f, codeInvalidFormat, "Tags must be unique.")
		}
		seen[t] = true
	}
}

// planOne validates one request, selects channels, plans the text and returns
// the messages (one, or several parts) to enqueue. pointer is the JSON
// pointer of the request inside the body ("" for POST /messages,
// "/messages/3" in a batch). Problems are added to v; the result is nil then.
func (a *API) planOne(rv *routingView, req sendRequest, pointer string, actor domain.Actor, v *validation) ([]domain.Message, error) {
	before := len(v.errs)
	f := func(name string) string { return pointer + "/" + name }

	var to phone.Number
	switch {
	case req.To == nil || *req.To == "":
		v.add(f("to"), codeRequired, "A recipient number is required.")
	case runeLen(*req.To) > maxToLen:
		v.add(f("to"), codeOutOfRange, "At most %d characters.", maxToLen)
	default:
		n, err := phone.Normalize(*req.To, phone.Options{DefaultRegion: a.opts.DefaultRegion, AllowShort: a.opts.AllowShortNumbers})
		switch {
		case errors.Is(err, phone.ErrInvalidNumber):
			v.add(f("to"), codeInvalidPhone, "Not a valid phone number for region %s.", strings.ToUpper(a.opts.DefaultRegion))
		case err != nil:
			return nil, fmt.Errorf("normalize number: %w", err) // misconfigured region
		default:
			to = n
		}
	}

	switch {
	case req.Body == nil || *req.Body == "":
		v.add(f("body"), codeRequired, "A message body is required.")
	case runeLen(*req.Body) > maxBodyChars:
		v.add(f("body"), codeOutOfRange, "At most %d characters.", maxBodyChars)
	}

	var channelID domain.ID
	if req.ChannelID != nil {
		switch {
		case *req.ChannelID == "" || len(*req.ChannelID) > maxIDLen:
			v.add(f("channel_id"), codeInvalidFormat, "A channel ID has 1 to %d characters.", maxIDLen)
		default:
			channelID = domain.ID(*req.ChannelID)
		}
	}
	validateTags(v, f("tags"), req.Tags)

	if req.ClientRef != nil && runeLen(*req.ClientRef) > maxClientRef {
		v.add(f("client_ref"), codeOutOfRange, "At most %d characters.", maxClientRef)
	}
	var expires time.Time
	hasExpiry := false
	if req.ExpiresAt != nil {
		t, ok := parseTimestamp(*req.ExpiresAt)
		switch {
		case !ok:
			v.add(f("expires_at"), codeInvalidFormat, "Must be an RFC 3339 timestamp.")
		case !t.After(a.clk.Now()):
			v.add(f("expires_at"), codeExpiresInPast, "expires_at must be in the future.")
		default:
			expires, hasExpiry = t, true
		}
	}
	if len(v.errs) > before {
		return nil, nil
	}

	t, code, detail := a.selectChannels(rv, channelID, req.Tags)
	if code != "" {
		field := pointer
		switch {
		case channelID != "":
			field = f("channel_id")
		case len(req.Tags) > 0:
			field = f("tags")
		}
		v.add(field, code, "%s", detail)
		return nil, nil
	}
	res, err := sms.Plan(*req.Body, t.caps, t.opts)
	if err != nil {
		code := sms.ErrorCode(err)
		switch {
		case code == "":
			return nil, fmt.Errorf("plan message: %w", err)
		case errors.Is(err, sms.ErrUnsupportedEncoding):
			v.add(f("body"), code, "%s", unsupportedDetail(*req.Body))
		default:
			v.add(f("body"), code, "%s", planDetail(err))
		}
		return nil, nil
	}

	now := a.clk.Now()
	msgs := make([]domain.Message, len(res.Parts))
	for i, part := range res.Parts {
		m := domain.Message{
			ID: a.ids.NewID(), Direction: domain.DirectionOut,
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
	return msgs, nil
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

// sendMessage implements POST /messages.
func (a *API) sendMessage(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	key, err := idempotencyKey(r)
	if err != nil {
		return err
	}
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req sendRequest
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	return a.enqueue(w, r, p, enqueueArgs{op: opSendMessage, raw: raw, key: key, reqs: []sendRequest{req}})
}

// sendBatch implements POST /messages/batch.
func (a *API) sendBatch(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	key, err := idempotencyKey(r)
	if err != nil {
		return err
	}
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req batchRequest
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	switch n := len(req.Messages); {
	case n == 0:
		var v validation
		v.add("/messages", codeRequired, "A batch has 1 to %d messages.", MaxBatchSize)
		return v.err()
	case n > MaxBatchSize:
		return newProblem(http.StatusRequestEntityTooLarge, problemBatchTooLarge,
			fmt.Sprintf("Batch contains %d messages; the maximum is %d.", n, MaxBatchSize))
	}
	return a.enqueue(w, r, p, enqueueArgs{op: opSendBatch, raw: raw, key: key, reqs: req.Messages, batch: true})
}

type enqueueArgs struct {
	op    string
	raw   []byte
	key   string
	reqs  []sendRequest
	batch bool
}

// enqueue validates and plans every request, stores all resulting messages in
// one transaction (with the idempotency record) and answers 202.
//
// A request with a known Idempotency-Key is answered from the record before
// it is validated: a replay returns the original messages (in their current
// state) even if the request would be rejected now, for example because its
// channel was disabled meanwhile.
func (a *API) enqueue(w http.ResponseWriter, r *http.Request, p auth.Principal, in enqueueArgs) error {
	ctx := r.Context()
	actor := actorOf(p)
	hash, err := requestHash(in.op, in.raw)
	if err != nil {
		return malformed("Request body is not valid JSON.")
	}
	idem := store.Idempotency{Key: in.key, RequestHash: hash, Since: a.clk.Now().Add(-a.opts.IdempotencyTTL)}
	if in.key != "" {
		if !a.inflight.acquire(actor, in.key) {
			return newProblem(http.StatusConflict, problemIdemInUse,
				"A request with this Idempotency-Key is still being processed; retry later.")
		}
		defer a.inflight.release(actor, in.key)
		res, ok, err := a.st.Messages().Replay(ctx, actor, idem)
		if err != nil {
			return enqueueError(err)
		}
		if ok {
			return a.writeEnqueued(w, res, in.batch)
		}
	}

	rv, err := a.loadRouting(ctx)
	if err != nil {
		return err
	}
	var v validation
	var msgs []domain.Message
	groups := make([]int, 0, len(in.reqs))
	invalid := 0
	for i, req := range in.reqs {
		pointer := ""
		if in.batch {
			pointer = fmt.Sprintf("/messages/%d", i)
		}
		before := len(v.errs)
		parts, err := a.planOne(rv, req, pointer, actor, &v)
		if err != nil {
			return err
		}
		if len(v.errs) > before {
			invalid++
		}
		msgs = append(msgs, parts...)
		groups = append(groups, len(parts))
	}
	if !v.ok() {
		if in.batch {
			return validationFailed(fmt.Sprintf("Batch rejected: %d of %d messages are invalid.", invalid, len(in.reqs)), v.errs...)
		}
		return v.err()
	}

	if in.batch {
		idem.Groups = groups
	}
	res, err := a.st.Messages().Enqueue(ctx, msgs, idem)
	if err != nil {
		return enqueueError(err)
	}
	if !res.Replayed {
		a.disp.Notify()
	}
	return a.writeEnqueued(w, res, in.batch)
}

// enqueueError maps errors of Enqueue and Replay.
func enqueueError(err error) error {
	if errors.Is(err, domain.ErrIdempotencyMismatch) {
		return newProblem(http.StatusUnprocessableEntity, problemIdemMismatch,
			"Idempotency-Key was already used with a different request body.")
	}
	return fmt.Errorf("enqueue: %w", err)
}

// sendResponse is schema SendMessageResponse: the messages created for one
// SendMessageRequest (several when the text was split, SPEC §12.3).
type sendResponse struct {
	Messages []messageDTO `json:"messages"`
}

// writeEnqueued answers 202. POST /messages returns one SendMessageResponse
// with Location pointing at its first message; POST /messages/batch returns
// one SendMessageResponse per batch item, in request order.
func (a *API) writeEnqueued(w http.ResponseWriter, res store.EnqueueResult, batch bool) error {
	if res.Replayed {
		w.Header().Set(replayedHeader, "true")
	}
	all := make([]messageDTO, len(res.Messages))
	for i, m := range res.Messages {
		all[i] = toMessageDTO(m)
	}
	if !batch {
		w.Header().Set("Location", Prefix+"/messages/"+all[0].ID)
		writeJSON(w, http.StatusAccepted, sendResponse{Messages: all})
		return nil
	}
	groups := res.Groups
	if groups == nil { // records written without groups: one message per item
		groups = make([]int, len(all))
		for i := range groups {
			groups[i] = 1
		}
	}
	items := make([]sendResponse, 0, len(groups))
	off := 0
	for _, n := range groups {
		if n < 1 || off+n > len(all) {
			return fmt.Errorf("enqueue: %d messages do not match group sizes %v", len(all), groups)
		}
		items = append(items, sendResponse{Messages: all[off : off+n]})
		off += n
	}
	if off != len(all) {
		return fmt.Errorf("enqueue: %d messages do not match group sizes %v", len(all), groups)
	}
	writeJSON(w, http.StatusAccepted, struct {
		Items []sendResponse `json:"items"`
	}{Items: items})
	return nil
}

// listMessages implements GET /messages.
func (a *API) listMessages(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := newQuery(r, "direction", "status", "peer", "channel_id", "since", "until", "cursor", "limit")
	var f store.MessageFilter
	if s := q.str("direction"); s != "" {
		d, err := domain.ParseDirection(s)
		if err != nil {
			q.fail("direction", codeInvalidFormat, "Must be \"out\" or \"in\".")
		}
		f.Direction = d
	}
	seen := map[domain.MessageStatus]bool{}
	for _, s := range q.all("status") {
		st, err := domain.ParseMessageStatus(s)
		if err != nil {
			q.fail("status", codeInvalidFormat, "Unknown message status.")
			continue
		}
		if !seen[st] {
			seen[st] = true
			f.Statuses = append(f.Statuses, st)
		}
	}
	if peer := q.str("peer"); peer != "" {
		if runeLen(peer) > maxToLen {
			q.fail("peer", codeOutOfRange, "At most 32 characters.")
		}
		// Same normalisation as "to"; inbound peers can be alphanumeric
		// senders that never normalise, so fall back to the raw text.
		f.Peer = peer
		if n, err := phone.Normalize(peer, phone.Options{DefaultRegion: a.opts.DefaultRegion, AllowShort: a.opts.AllowShortNumbers}); err == nil {
			f.Peer = n.Value
		}
	}
	f.ChannelID = q.id("channel_id")
	f.Since, f.Until = q.timestamp("since"), q.timestamp("until")
	after := q.cursor()
	limit := q.limit()
	if err := q.err(); err != nil {
		return err
	}

	page, err := a.st.Messages().List(r.Context(), f, store.PageRequest{Cursor: string(after), Limit: limit})
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}
	items := make([]messageDTO, len(page.Items))
	for i, m := range page.Items {
		items[i] = toMessageDTO(m)
	}
	next := ""
	if page.NextCursor != "" {
		next = encodeCursor(domain.ID(page.NextCursor))
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

// messageID reads the {id} path parameter of a message (a ULID).
func messageID(r *http.Request) (domain.ID, error) {
	id, err := domain.ParseID(r.PathValue("id"))
	if err != nil {
		return "", malformed("Invalid message ID.", fieldError{Field: "id", Code: codeInvalidFormat, Detail: "Not a ULID."})
	}
	return id, nil
}

// getMessage implements GET /messages/{id}.
func (a *API) getMessage(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := messageID(r)
	if err != nil {
		return err
	}
	m, err := a.st.Messages().Get(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		return notFound("Message")
	}
	if err != nil {
		return fmt.Errorf("get message: %w", err)
	}
	writeJSON(w, http.StatusOK, toMessageDTO(m))
	return nil
}

// cancelMessage implements DELETE /messages/{id}.
func (a *API) cancelMessage(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := messageID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var m domain.Message
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
		var err error
		if m, err = tx.Messages().Cancel(ctx, id); err != nil {
			return err
		}
		return a.audit(ctx, tx, actorOf(p), domain.AuditMessageCancel, "message:"+string(id), nil)
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return notFound("Message")
	case errors.Is(err, domain.ErrInvalidTransition):
		return conflict(fmt.Sprintf("Message %s is in status %q; only \"queued\" messages can be cancelled.", id, string(m.Status)))
	case err != nil:
		return fmt.Errorf("cancel message: %w", err)
	}
	writeJSON(w, http.StatusOK, toMessageDTO(m))
	return nil
}

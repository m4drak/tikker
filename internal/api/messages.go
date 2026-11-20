// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
)

// Limits of schema SendMessageRequest and Tag.
const (
	maxToLen       = sending.MaxToLen
	maxIDLen       = sending.MaxIDLen
	replayedHeader = "Idempotent-Replayed"
	opSendMessage  = "POST /messages"
	opSendBatch    = "POST /messages/batch"
)

// sendRequest is schema SendMessageRequest (the JSON form of
// sending.Request). Fields are pointers so that a missing member can be told
// from an empty one.
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

// validateTags checks Tags (schema Tags and Tag).
func validateTags(v *validation, field string, tags []string) {
	for _, e := range sending.ValidateTags(field, tags) {
		v.add(e.Field, e.Code, "%s", e.Detail)
	}
}

// planOne validates one request, selects channels, plans the text and returns
// the messages (one, or several parts) to enqueue (package sending). pointer
// is the JSON pointer of the request inside the body ("" for POST /messages,
// "/messages/3" in a batch). Problems are added to v; the result is nil then.
func (a *API) planOne(rt *sending.Routing, req sendRequest, pointer string, actor domain.Actor, v *validation) ([]domain.Message, error) {
	msgs, errs, err := a.send.Plan(rt, sending.Request(req), pointer, actor)
	if err != nil {
		return nil, err //nolint:wrapcheck // already has context
	}
	for _, e := range errs {
		v.add(e.Field, e.Code, "%s", e.Detail)
	}
	return msgs, nil
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

	rt, err := a.send.LoadRouting(ctx)
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
		parts, err := a.planOne(rt, req, pointer, actor, &v)
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
	res, err := a.send.Enqueue(ctx, msgs, idem)
	if err != nil {
		return enqueueError(err)
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
		f.Peer = a.send.NormalizePeer(peer)
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
	m, err := a.send.Cancel(r.Context(), actorOf(p), id)
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

// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package client is a small typed client for the tikker REST API
// (api/openapi.yaml). It covers the message operations used by the
// `tikker sms` command line: send, get, list and cancel.
//
// The client never logs or prints the bearer token and never includes it in
// returned errors.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxResponseBytes bounds the response body read from the server.
const maxResponseBytes = 8 << 20

// Message statuses (MessageStatus in the contract).
const (
	StatusQueued    = "queued"
	StatusSending   = "sending"
	StatusSent      = "sent"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusReceived  = "received"
)

// Message directions (Direction in the contract).
const (
	DirectionOut = "out"
	DirectionIn  = "in"
)

// Statuses lists every valid MessageStatus.
var Statuses = []string{StatusQueued, StatusSending, StatusSent, StatusFailed, StatusCancelled, StatusReceived}

// IsTerminal reports whether an outgoing message in this status will not
// change any more.
func IsTerminal(status string) bool {
	switch status {
	case StatusSent, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// SendRequest is SendMessageRequest.
type SendRequest struct {
	To        string   `json:"to"`
	Body      string   `json:"body"`
	ChannelID string   `json:"channel_id,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	ClientRef string   `json:"client_ref,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

// Message is the Message schema.
type Message struct {
	ID                string   `json:"id"`
	Direction         string   `json:"direction"`
	ChannelID         *string  `json:"channel_id"`
	Peer              string   `json:"peer"`
	Body              *string  `json:"body"`
	Encoding          string   `json:"encoding"`
	Segments          int      `json:"segments"`
	Status            string   `json:"status"`
	Attempts          int      `json:"attempts"`
	NextAttemptAt     *string  `json:"next_attempt_at"`
	LastError         *string  `json:"last_error"`
	PossibleDuplicate bool     `json:"possible_duplicate"`
	ClientRef         *string  `json:"client_ref"`
	Tags              []string `json:"tags"`
	ExpiresAt         *string  `json:"expires_at"`
	CreatedBy         *string  `json:"created_by"`
	CreatedAt         string   `json:"created_at"`
	SentAt            *string  `json:"sent_at"`
	ReceivedAt        *string  `json:"received_at"`
}

// SendResponse is SendMessageResponse.
type SendResponse struct {
	Messages []Message `json:"messages"`
}

// MessagePage is MessagePage: one page of messages and the cursor of the next.
type MessagePage struct {
	Items      []Message `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// ListParams are the query parameters of GET /messages used by the CLI.
type ListParams struct {
	Direction string
	Statuses  []string
	Limit     int
	Cursor    string
}

// FieldError is FieldError.
type FieldError struct {
	Field  string `json:"field"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Problem is the RFC 9457 problem document (Problem).
type Problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	Errors   []FieldError `json:"errors,omitempty"`
}

// Code returns the short problem code: the part of Type after
// "urn:tikker:problem:", for example "validation".
func (p Problem) Code() string {
	return strings.TrimPrefix(p.Type, "urn:tikker:problem:")
}

// APIError is returned for every non-2xx response. Problem is empty when the
// server did not answer with a problem document.
type APIError struct {
	HTTPStatus int
	Problem    Problem
	// Body is a short excerpt of a non-problem response body.
	Body string
}

// Error renders the problem as a readable message: one summary line, then one
// line per field error.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "API error %d", e.HTTPStatus)
	if code := e.Problem.Code(); code != "" {
		fmt.Fprintf(&b, " (%s)", code)
	}
	if e.Problem.Title != "" {
		b.WriteString(": " + e.Problem.Title)
	}
	if e.Problem.Detail != "" {
		b.WriteString(": " + e.Problem.Detail)
	}
	if e.Problem.Title == "" && e.Problem.Detail == "" && e.Body != "" {
		b.WriteString(": " + e.Body)
	}
	for _, fe := range e.Problem.Errors {
		fmt.Fprintf(&b, "\n  - %s: %s: %s", fe.Field, fe.Code, fe.Detail)
	}
	return b.String()
}

// Client talks to one tikker instance.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New creates a client for the instance at baseURL (http or https, without
// the /api/v1 suffix, which is tolerated). token is sent as a bearer token; hc may be nil.
func New(baseURL, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, errors.New("invalid URL: " + urlErrReason(err))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid URL: want http:// or https:// followed by a host")
	}
	if token == "" {
		return nil, errors.New("empty token")
	}
	if hc == nil {
		hc = &http.Client{}
	}
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	// Tolerate an address copied together with the API prefix.
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v1")
	return &Client{base: u, token: token, http: hc}, nil
}

func urlErrReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// Send posts a message. idempotencyKey is sent as Idempotency-Key when not
// empty. It returns the decoded response and the raw JSON body.
func (c *Client) Send(ctx context.Context, req SendRequest, idempotencyKey string) (SendResponse, json.RawMessage, error) {
	var out SendResponse
	h := http.Header{}
	if idempotencyKey != "" {
		h.Set("Idempotency-Key", idempotencyKey)
	}
	raw, err := c.do(ctx, http.MethodPost, "/messages", nil, h, req, &out)
	return out, raw, err
}

// Get fetches one message.
func (c *Client) Get(ctx context.Context, id string) (Message, json.RawMessage, error) {
	var out Message
	raw, err := c.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(id), nil, nil, nil, &out)
	return out, raw, err
}

// Cancel cancels a queued message and returns it in its new state.
func (c *Client) Cancel(ctx context.Context, id string) (Message, json.RawMessage, error) {
	var out Message
	raw, err := c.do(ctx, http.MethodDelete, "/messages/"+url.PathEscape(id), nil, nil, nil, &out)
	return out, raw, err
}

// List fetches one page of messages.
func (c *Client) List(ctx context.Context, p ListParams) (MessagePage, json.RawMessage, error) {
	q := url.Values{}
	if p.Direction != "" {
		q.Set("direction", p.Direction)
	}
	for _, s := range p.Statuses {
		q.Add("status", s)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	var out MessagePage
	raw, err := c.do(ctx, http.MethodGet, "/messages", q, nil, nil, &out)
	return out, raw, err
}

// do performs one request. On 2xx it decodes the body into out; otherwise it
// returns an *APIError.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, header http.Header, body, out any) (json.RawMessage, error) {
	u := *c.base
	// path is already escaped; set Path and RawPath so that %2F in an id is
	// sent as-is instead of being escaped a second time.
	escaped := u.EscapedPath() + "/api/v1" + path
	if dec, err := url.PathUnescape(escaped); err == nil {
		u.Path, u.RawPath = dec, escaped
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		// *url.Error carries the method and URL only, never the headers.
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("response too large")
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, newAPIError(res, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %w", res.StatusCode, err)
	}
	return json.RawMessage(data), nil
}

func newAPIError(res *http.Response, data []byte) *APIError {
	e := &APIError{HTTPStatus: res.StatusCode}
	mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if mt == "application/problem+json" || mt == "application/json" {
		var p Problem
		if json.Unmarshal(data, &p) == nil && (p.Type != "" || p.Title != "") {
			e.Problem = p
			return e
		}
	}
	e.Body = excerpt(data)
	return e
}

// excerpt returns a short single-line, printable excerpt of a body.
func excerpt(data []byte) string {
	s := strings.Join(strings.Fields(string(bytes.ToValidUTF8(data, nil))), " ")
	const limit = 200
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "..."
	}
	return s
}

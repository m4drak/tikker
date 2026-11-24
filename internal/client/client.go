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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	// Location is the Location header of a redirect response.
	Location string
	// RetryAfter is the parsed Retry-After header (zero when absent).
	RetryAfter time.Duration
}

// Error renders the problem as a readable message: one summary line, then one
// line per field error.
func (e *APIError) Error() string {
	// Every server-supplied string goes through Sanitize so that it cannot
	// inject terminal escapes or extra lines.
	var b strings.Builder
	fmt.Fprintf(&b, "API error %d", e.HTTPStatus)
	if code := Sanitize(e.Problem.Code(), 128); code != "" {
		fmt.Fprintf(&b, " (%s)", code)
	}
	if e.Location != "" {
		fmt.Fprintf(&b, ": redirect to %s (redirects are not followed; check --url, use https:// if the server redirects to it)", Sanitize(e.Location, 256))
	}
	if t := Sanitize(e.Problem.Title, 256); t != "" {
		b.WriteString(": " + t)
	}
	if d := Sanitize(e.Problem.Detail, 1024); d != "" {
		b.WriteString(": " + d)
	}
	if e.Problem.Title == "" && e.Problem.Detail == "" && e.Body != "" {
		b.WriteString(": " + Sanitize(e.Body, 200))
	}
	for _, fe := range e.Problem.Errors {
		fmt.Fprintf(&b, "\n  - %s: %s: %s", Sanitize(fe.Field, 128), Sanitize(fe.Code, 64), Sanitize(fe.Detail, 512))
	}
	return b.String()
}

// ErrInsecureTransport is returned by New for a plain http:// URL whose host
// is not a loopback address, unless the caller allowed it explicitly.
var ErrInsecureTransport = errors.New("refusing to send the API token over plain http:// to a non-loopback host (use https://, or --insecure-http to override)")

// Client talks to one tikker instance.
type Client struct {
	base     *url.URL
	token    string
	http     *http.Client
	insecure bool
}

// Insecure reports whether the client sends the token over plain http:// to
// a non-loopback host (allowed explicitly with allowInsecure).
func (c *Client) Insecure() bool { return c.insecure }

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Sanitize replaces control characters (including terminal escapes and
// newlines) with spaces, collapses whitespace and truncates s to limit runes.
// Use it for every server-supplied string that is printed as text.
func Sanitize(s string, limit int) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); limit > 3 && len(r) > limit {
		s = string(r[:limit-3]) + "..."
	}
	return s
}

// New creates a client for the instance at baseURL (http or https, without
// the /api/v1 suffix, which is tolerated). token is sent as a bearer token; hc may be nil.
// Plain http:// is accepted only for loopback hosts unless allowInsecure is set.
// Redirects are never followed (a copy of hc is used).
func New(baseURL, token string, hc *http.Client, allowInsecure bool) (*Client, error) {
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
	insecure := u.Scheme == "http" && !isLoopbackHost(u.Hostname())
	if insecure && !allowInsecure {
		return nil, ErrInsecureTransport
	}
	if hc == nil {
		hc = &http.Client{}
	}
	hcCopy := *hc
	hcCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc = &hcCopy
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	// Tolerate an address copied together with the API prefix.
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v1")
	return &Client{base: u, token: token, http: hc, insecure: insecure}, nil
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
	raw, err := c.do(ctx, http.MethodPost, "/messages", http.StatusAccepted, nil, h, req, &out)
	return out, raw, err
}

// Get fetches one message.
func (c *Client) Get(ctx context.Context, id string) (Message, json.RawMessage, error) {
	var out Message
	raw, err := c.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(id), http.StatusOK, nil, nil, nil, &out)
	return out, raw, err
}

// Cancel cancels a queued message and returns it in its new state.
func (c *Client) Cancel(ctx context.Context, id string) (Message, json.RawMessage, error) {
	var out Message
	raw, err := c.do(ctx, http.MethodDelete, "/messages/"+url.PathEscape(id), http.StatusOK, nil, nil, nil, &out)
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
	raw, err := c.do(ctx, http.MethodGet, "/messages", http.StatusOK, q, nil, nil, &out)
	return out, raw, err
}

// do performs one request. It decodes the body into out when the status is
// exactly wantStatus (the code the contract documents); any other status
// returns an *APIError (or an error for an undocumented 2xx).
func (c *Client) do(ctx context.Context, method, path string, wantStatus int, query url.Values, header http.Header, body, out any) (json.RawMessage, error) {
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

	if res.StatusCode != wantStatus {
		if res.StatusCode >= 200 && res.StatusCode <= 299 {
			return nil, fmt.Errorf("unexpected HTTP status %d (want %d)", res.StatusCode, wantStatus)
		}
		return nil, newAPIError(res, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %w", res.StatusCode, err)
	}
	return json.RawMessage(data), nil
}

func newAPIError(res *http.Response, data []byte) *APIError {
	e := &APIError{HTTPStatus: res.StatusCode, RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"))}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		e.Location = res.Header.Get("Location")
	}
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

// parseRetryAfter parses a Retry-After header (delta-seconds or HTTP date);
// it returns 0 when absent or invalid. The result is capped at one hour.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return max(0, min(d, time.Hour))
}

// excerpt returns a short single-line, printable excerpt of a body.
func excerpt(data []byte) string {
	return Sanitize(string(data), 200)
}

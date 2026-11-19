// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// call describes one REST request.
type call struct {
	op     string
	method string
	path   string // below /rest, e.g. "/tool/sms/send"
	body   any    // JSON-encoded when non-nil
	// timeout bounds the whole exchange; 0 means requestTimeout.
	timeout time.Duration
	// mutating marks commands whose repetition has an effect (sending an
	// SMS); only their failures after writing the request are ambiguous.
	mutating bool
}

// response is a completed HTTP exchange.
type response struct {
	status  int
	body    []byte
	tls     *tls.ConnectionState
	latency time.Duration // until the response headers arrived
}

func (r *response) ok() bool { return r != nil && r.status >= 200 && r.status < 300 }

// do performs cl. It returns the response whenever headers arrived, together
// with a classified error for non-2xx statuses or a failed body read. A nil
// response means a transport error.
func (c *Conn) do(ctx context.Context, cl call) (*response, error) {
	if c.closed.Load() {
		return nil, localError(domain.KindTransient, cl.op, ErrClosed)
	}
	timeout := cl.timeout
	if timeout <= 0 {
		timeout = c.requestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// RouterOS commands are always POSTed as JSON; GET with parameters does
	// not work (research W3). Content-Type is required (415 otherwise).
	payload := []byte{}
	if cl.body != nil {
		var err error
		if payload, err = json.Marshal(cl.body); err != nil {
			return nil, localError(domain.KindPermanent, cl.op, fmt.Errorf("encode request: %w", err))
		}
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, c.base+cl.path, bytes.NewReader(payload))
	if err != nil {
		return nil, localError(domain.KindPermanent, cl.op, fmt.Errorf("build request: %w", err))
	}
	if cl.body == nil {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	}
	req.SetBasicAuth(c.user, c.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// The request may have reached the router once a connection was obtained
	// and writing it did not fail. Waiting for WroteRequest alone is not
	// enough: a context deadline can win the race against the trace callback
	// after the bytes already left.
	var gotConn, writeFailed atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err != nil {
				writeFailed.Store(true)
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	maybeSent := func() bool { return gotConn.Load() && !writeFailed.Load() }

	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, classifyTransport(cl.op, cl.mutating, maybeSent(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := &response{status: resp.StatusCode, tls: resp.TLS, latency: time.Since(start)}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return out, classifyTransport(cl.op, cl.mutating, true, fmt.Errorf("%s %s: read response: %w", cl.method, cl.path, err))
	}
	if int64(len(body)) > c.maxBody {
		return out, &domain.ConnectorError{
			Kind: domain.KindTransient, Op: cl.op, Code: domain.CodeUnexpectedResponse,
			Err: fmt.Errorf("%s %s: %w (over %d bytes)", cl.method, cl.path, ErrResponseTooLarge, c.maxBody),
		}
	}
	out.body = body
	if !out.ok() {
		return out, classifyStatus(cl.op, cl.method, cl.path, resp.StatusCode, body)
	}
	return out, nil
}

// decode unmarshals a 2xx body into v.
func decode(op, path string, r *response, v any) error {
	if err := json.Unmarshal(r.body, v); err != nil {
		return &domain.ConnectorError{
			Kind: domain.KindTransient, Op: op, Code: domain.CodeUnexpectedResponse,
			Err: fmt.Errorf("%s: %w: %v", path, ErrUnexpectedResponse, err),
		}
	}
	return nil
}

// record is one RouterOS item. REST renders every value as a string, but
// numbers and booleans are tolerated.
type record map[string]any

func (r record) str(key string) string {
	switch v := r[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprintf("%g", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	return ""
}

func (r record) has(key string) bool { _, ok := r[key]; return ok }

// boolean reads RouterOS flags ("true"/"false", "yes"/"no").
func (r record) boolean(key string) bool {
	switch r.str(key) {
	case "true", "yes":
		return true
	}
	return false
}

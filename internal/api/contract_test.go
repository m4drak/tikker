// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

// contract validates exchanges against api/openapi.yaml, the source of truth.
// github.com/getkin/kin-openapi (MIT) is used only here, in tests: it loads
// OpenAPI 3.1 documents (type arrays such as [string, "null"]) and validates
// response bodies, status codes and content types against the operation.
type contract struct {
	doc *openapi3.T
	ops []contractOp
}

type contractOp struct {
	method   string
	template string
	re       *regexp.Regexp
	item     *openapi3.PathItem
	op       *openapi3.Operation
}

var (
	contractOnce sync.Once
	contractDoc  *contract
	contractErr  error
)

// loadContract loads the contract once per test binary.
func loadContract(t *testing.T) *contract {
	t.Helper()
	contractOnce.Do(func() {
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile(filepath.Join("..", "..", "api", "openapi.yaml"))
		if err != nil {
			contractErr = err
			return
		}
		if err := doc.Validate(context.Background()); err != nil {
			contractErr = err
			return
		}
		c := &contract{doc: doc}
		for tmpl, item := range doc.Paths.Map() {
			if tmpl == "/healthz" || tmpl == "/readyz" {
				continue // served by package probes
			}
			segs := strings.Split(tmpl, "/")
			for i, seg := range segs {
				if strings.HasPrefix(seg, "{") {
					segs[i] = "[^/]+"
				} else {
					segs[i] = regexp.QuoteMeta(seg)
				}
			}
			pattern := "^" + strings.Join(segs, "/") + "$"
			for method, op := range item.Operations() {
				c.ops = append(c.ops, contractOp{method: method, template: tmpl, re: regexp.MustCompile(pattern), item: item, op: op})
			}
		}
		contractDoc = c
	})
	if contractErr != nil {
		t.Fatalf("api/openapi.yaml: %v", contractErr)
	}
	return contractDoc
}

// find returns the operation for a request, or nil.
func (c *contract) find(method, path string) *contractOp {
	path = strings.TrimPrefix(path, Prefix)
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	for i := range c.ops {
		if c.ops[i].method == method && c.ops[i].re.MatchString(path) {
			return &c.ops[i]
		}
	}
	return nil
}

// check fails the test when res does not conform to the contract: the status
// must be declared for the operation, the content type and body must match the
// schema, and the headers the contract promises must be present.
// rateHeaders is false when the API runs without a request budget, which the
// contract allows to omit the RateLimit-* headers.
func (c *contract) check(t *testing.T, method, path string, res *result, rateHeaders bool) {
	t.Helper()
	op := c.find(method, path)
	if op == nil {
		c.checkCommon(t, method, path, res)
		return
	}
	req := httptest.NewRequest(method, path, nil)
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req,
			Route:   &routers.Route{Spec: c.doc, Path: op.template, PathItem: op.item, Method: method, Operation: op.op},
		},
		Status:  res.Status,
		Header:  res.Header,
		Body:    io.NopCloser(bytes.NewReader(res.Body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		t.Errorf("%s %s -> %d does not match the contract: %v\nbody: %s", method, path, res.Status, err, res.Body)
	}
	// Headers every authenticated response of the API carries; a 401 costs
	// nothing and carries none.
	for _, h := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"} {
		switch got := res.Header.Get(h); {
		case res.Status != http.StatusUnauthorized && rateHeaders && got == "":
			t.Errorf("%s %s -> %d: missing header %s", method, path, res.Status, h)
		case (res.Status == http.StatusUnauthorized || !rateHeaders) && got != "":
			t.Errorf("%s %s -> %d: unexpected header %s", method, path, res.Status, h)
		}
	}
	if res.Status == http.StatusTooManyRequests && res.Header.Get("Retry-After") == "" {
		t.Errorf("429 without Retry-After")
	}
	if res.Status == http.StatusUnauthorized && res.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("401 without WWW-Authenticate")
	}
	checkProblem(t, res)
}

// checkCommon validates the responses the contract describes for every path
// ("Odpowiedzi wspólne"): 404 for an unknown path, 405 with Allow for a known
// path and an unsupported method; both are answered before authentication.
func (c *contract) checkCommon(t *testing.T, method, path string, res *result) {
	t.Helper()
	var name string
	switch res.Status {
	case http.StatusNotFound:
		name = "NotFound"
	case http.StatusMethodNotAllowed:
		name = "MethodNotAllowed"
		if res.Header.Get("Allow") == "" {
			t.Errorf("%s %s -> 405 without Allow", method, path)
		}
	default:
		t.Errorf("%s %s -> %d: no operation and not a common response", method, path, res.Status)
		return
	}
	resp := c.doc.Components.Responses[name]
	if resp == nil || resp.Value == nil {
		t.Fatalf("components/responses/%s missing", name)
	}
	mt := resp.Value.Content.Get(res.Header.Get("Content-Type"))
	if mt == nil {
		t.Errorf("%s %s -> %d: content type %q not in components/responses/%s", method, path, res.Status, res.Header.Get("Content-Type"), name)
		return
	}
	var body any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Errorf("%s %s -> %d: body is not JSON: %s", method, path, res.Status, res.Body)
		return
	}
	if err := mt.Schema.Value.VisitJSON(body); err != nil {
		t.Errorf("%s %s -> %d does not match components/responses/%s: %v", method, path, res.Status, name, err)
	}
	if res.Header.Get("RateLimit-Limit") != "" {
		t.Errorf("%s %s -> %d: common responses carry no RateLimit headers", method, path, res.Status)
	}
	checkProblem(t, res)
}

// checkProblem checks that an error response is a problem of the contract.
func checkProblem(t *testing.T, res *result) {
	t.Helper()
	if res.Status >= 400 {
		var p struct {
			Type   string `json:"type"`
			Title  string `json:"title"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal(res.Body, &p); err != nil || p.Status != res.Status || !strings.HasPrefix(p.Type, "urn:tikker:problem:") {
			t.Errorf("error response is not a contract problem: %s", res.Body)
		}
		// title is constant per type.
		if want, ok := problemTitles[p.Type]; ok && p.Title != want {
			t.Errorf("problem %s has title %q, want %q", p.Type, p.Title, want)
		}
	}
}

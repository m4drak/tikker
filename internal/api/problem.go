// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/domain"
)

// Problem types of api/openapi.yaml (RFC 9457 "type").
const (
	problemMalformed       = "urn:tikker:problem:malformed-request"
	problemUnauthorized    = "urn:tikker:problem:unauthorized"
	problemForbidden       = "urn:tikker:problem:forbidden"
	problemNotFound        = "urn:tikker:problem:not-found"
	problemMethodNotAllow  = "urn:tikker:problem:method-not-allowed"
	problemConflict        = "urn:tikker:problem:conflict"
	problemIdemInUse       = "urn:tikker:problem:idempotency-key-in-use"
	problemPayloadTooLarge = "urn:tikker:problem:payload-too-large"
	problemBatchTooLarge   = "urn:tikker:problem:batch-too-large"
	problemValidation      = "urn:tikker:problem:validation"
	problemIdemMismatch    = "urn:tikker:problem:idempotency-key-mismatch"
	problemRateLimited     = "urn:tikker:problem:rate-limited"
	problemInternal        = "urn:tikker:problem:internal"
	requestInstancePrefix  = "urn:tikker:request:"
	contentTypeProblemJSON = "application/problem+json"
)

// problemTitles are the fixed titles of the problem types (Problem.title is
// constant per type; the examples of the contract use these).
var problemTitles = map[string]string{
	problemMalformed:       "Malformed request",
	problemUnauthorized:    "Unauthorized",
	problemForbidden:       "Forbidden",
	problemNotFound:        "Not Found",
	problemMethodNotAllow:  "Method Not Allowed",
	problemConflict:        "Conflict",
	problemIdemInUse:       "Idempotency key in use",
	problemPayloadTooLarge: "Payload too large",
	problemBatchTooLarge:   "Batch too large",
	problemValidation:      "Request validation failed",
	problemIdemMismatch:    "Idempotency key reused with different payload",
	problemRateLimited:     "Too Many Requests",
	problemInternal:        "Internal Server Error",
}

// Field error codes of api/openapi.yaml (errors[].code).
const (
	codeRequired           = "required"
	codeInvalidFormat      = "invalid_format"
	codeOutOfRange         = "out_of_range"
	codeInvalidPhone       = "invalid_phone_number"
	codeTooManySegments    = "too_many_segments"
	codeUnsupportedChars   = "unsupported_characters"
	codeUnknownChannel     = "unknown_channel"
	codeChannelDisabled    = "channel_disabled"
	codeNoMatchingChannel  = "no_matching_channel"
	codeExpiresInPast      = "expires_at_in_past"
	codeInsecureTransport  = "insecure_transport_not_allowed"
	codeUnknownDevice      = "unknown_device"
	maxReportedFieldErrors = 100
	maxProblemDetailRunes  = 500
)

// fieldError is one entry of Problem.errors (schema FieldError).
type fieldError struct {
	Field  string `json:"field"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// problem is the RFC 9457 document of the contract (schema Problem).
type problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	Errors   []fieldError `json:"errors,omitempty"`
}

// apiError is an error that is reported to the client as a problem. Handlers
// return it; everything else becomes a 500.
type apiError struct {
	status   int
	typ      string
	title    string
	detail   string
	instance string
	errors   []fieldError
	header   http.Header
}

func (e *apiError) Error() string {
	return fmt.Sprintf("api: %d %s: %s", e.status, e.typ, e.detail)
}

func newProblem(status int, typ, detail string, errs ...fieldError) *apiError {
	title, ok := problemTitles[typ]
	if !ok {
		title = http.StatusText(status)
	}
	return &apiError{status: status, typ: typ, title: title, detail: detail, errors: errs}
}

func malformed(detail string, errs ...fieldError) *apiError {
	return newProblem(http.StatusBadRequest, problemMalformed, detail, errs...)
}

func notFound(what string) *apiError {
	return newProblem(http.StatusNotFound, problemNotFound, what+" not found.")
}

func conflict(detail string) *apiError {
	return newProblem(http.StatusConflict, problemConflict, detail)
}

// validationFailed is the 422 "validation" problem.
func validationFailed(detail string, errs ...fieldError) *apiError {
	return newProblem(http.StatusUnprocessableEntity, problemValidation, detail, errs...)
}

func (e *apiError) withHeader(k, v string) *apiError {
	if e.header == nil {
		e.header = http.Header{}
	}
	e.header.Set(k, v)
	return e
}

// validation collects field errors of one request. Its zero value is ready.
type validation struct {
	errs []fieldError
}

func (v *validation) add(field, code, format string, args ...any) {
	if len(v.errs) >= maxReportedFieldErrors {
		return
	}
	v.errs = append(v.errs, fieldError{Field: field, Code: code, Detail: fmt.Sprintf(format, args...)})
}

func (v *validation) ok() bool { return len(v.errs) == 0 }

// err returns the 422 problem, or nil when nothing was collected.
func (v *validation) err() error {
	if v.ok() {
		return nil
	}
	detail := "The request is invalid."
	if len(v.errs) == 1 {
		detail = v.errs[0].Detail
	}
	return validationFailed(detail, v.errs...)
}

// writeProblem writes e as application/problem+json. instance is the request
// path unless e carries its own.
func writeProblem(w http.ResponseWriter, r *http.Request, e *apiError) {
	for k, vs := range e.header {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", contentTypeProblemJSON)
	w.Header().Set("Cache-Control", "no-store")
	instance := e.instance
	if instance == "" {
		instance = r.URL.Path
	}
	title := e.title
	if title == "" {
		title = http.StatusText(e.status)
	}
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(problem{
		Type: e.typ, Title: title, Status: e.status, Detail: truncateRunes(e.detail, maxProblemDetailRunes),
		Instance: instance, Errors: e.errors,
	})
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// mapStoreError turns repository sentinels into problems; nil for anything
// else. what names the resource for "not found" ("Message", "Device", ...);
// conflictDetail describes a domain.ErrConflict for this operation.
func mapStoreError(err error, what, conflictDetail string) *apiError {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return notFound(what)
	case errors.Is(err, domain.ErrConflict):
		return conflict(conflictDetail)
	}
	return nil
}

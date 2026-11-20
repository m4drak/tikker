// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// readBody reads the whole request body, at most max bytes. A longer body is
// the 413 problem of the contract.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, newProblem(http.StatusRequestEntityTooLarge, problemPayloadTooLarge,
				fmt.Sprintf("Request body exceeds the limit of %d bytes.", mbe.Limit))
		}
		return nil, malformed("Request body could not be read.")
	}
	return b, nil
}

// decodeStrict decodes body into v (a pointer to a struct) like a schema with
// additionalProperties: false: unknown fields, wrong types, trailing data and
// an empty body are 400 problems.
func decodeStrict(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return malformed("Request body is empty; a JSON object is required.")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return decodeProblem(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return malformed("Request body must contain a single JSON value.")
	}
	return nil
}

// decodeProblem describes a JSON decoding error without leaking Go type names.
func decodeProblem(err error) *apiError {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return malformed(fmt.Sprintf("Request body is not valid JSON: %s at offset %d.", syntaxMessage(syn), syn.Offset))
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return malformed("Request body is not valid JSON: unexpected end of input.")
	case errors.As(err, &typ):
		field := "/" + strings.ReplaceAll(typ.Field, ".", "/")
		return malformed(fmt.Sprintf("Field %q has the wrong type: expected %s, got %s.", typ.Field, jsonKind(typ.Type.String()), typ.Value),
			fieldError{Field: field, Code: codeInvalidFormat, Detail: "Expected " + jsonKind(typ.Type.String()) + "."})
	}
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		name = strings.Trim(name, `"`)
		return malformed(fmt.Sprintf("Unknown field %q.", name),
			fieldError{Field: "/" + name, Code: codeInvalidFormat, Detail: "Unknown field."})
	}
	return malformed("Request body is not valid.")
}

func syntaxMessage(e *json.SyntaxError) string {
	m := e.Error()
	if len(m) > 80 {
		m = m[:80]
	}
	return m
}

// jsonKind maps a Go type name from encoding/json to a JSON type name.
func jsonKind(goType string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		return "array"
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "uint"), strings.HasPrefix(goType, "float"):
		return "number"
	case goType == "string" || goType == "secrets.Password":
		return "string"
	case goType == "bool":
		return "boolean"
	}
	return "value of another type"
}

// writeJSON writes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// JSON Merge Patch (RFC 7396)

// mergePatch is a decoded merge-patch document: one raw value per member.
type mergePatch map[string]json.RawMessage

// decodePatch parses body as a JSON object and rejects members outside
// allowed (the contract uses additionalProperties: false). An empty object is
// left to the caller (minProperties: 1).
func decodePatch(body []byte, allowed ...string) (mergePatch, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, malformed("Request body is empty; a JSON object is required.")
	}
	var p mergePatch
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&p); err != nil {
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) {
			return nil, malformed("Request body must be a JSON object.")
		}
		return nil, decodeProblem(err)
	}
	if p == nil {
		return nil, malformed("Request body must be a JSON object.")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, malformed("Request body must contain a single JSON value.")
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return nil, malformed(fmt.Sprintf("Unknown field %q.", k),
				fieldError{Field: "/" + k, Code: codeInvalidFormat, Detail: "Unknown field."})
		}
	}
	return p, nil
}

func (p mergePatch) has(k string) bool { _, ok := p[k]; return ok }

// isNull reports whether member k is present with the value null.
func (p mergePatch) isNull(k string) bool {
	raw, ok := p[k]
	return ok && string(bytes.TrimSpace(raw)) == "null"
}

// get decodes member k into v. A wrong type is a 400 problem.
func (p mergePatch) get(k string, v any) error {
	dec := json.NewDecoder(bytes.NewReader(p[k]))
	if err := dec.Decode(v); err != nil {
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) || errors.Is(err, io.ErrUnexpectedEOF) {
			return malformed(fmt.Sprintf("Field %q has the wrong type.", k),
				fieldError{Field: "/" + k, Code: codeInvalidFormat, Detail: "Wrong JSON type."})
		}
		return decodeProblem(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Values

// formatTime renders t as RFC 3339 in UTC without fractions, as the examples
// of the contract do.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// nullableTime is the JSON form of an optional timestamp.
func nullableTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}

// nullableString is "" -> null.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseTimestamp parses an RFC 3339 timestamp.
func parseTimestamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// runeLen counts characters like JSON Schema's minLength/maxLength.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

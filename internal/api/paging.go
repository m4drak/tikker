// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Page limits of the contract (parameter Limit).
const (
	defaultLimit = 50
	maxLimit     = 200
	maxCursorLen = 512
)

// cursorBody is the (opaque to clients) content of a cursor: the ID of the
// last item of the previous page.
type cursorBody struct {
	ID string `json:"id"`
}

func encodeCursor(id domain.ID) string {
	b, _ := json.Marshal(cursorBody{ID: string(id)})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor returns the ID in cursor, "" for no cursor. A cursor that this
// server did not issue is a 400 problem.
func decodeCursor(c string) (domain.ID, error) {
	if c == "" {
		return "", nil
	}
	bad := malformed("Invalid query parameters.", fieldError{Field: "cursor", Code: codeInvalidFormat, Detail: "Not a cursor issued by this server."})
	if len(c) > maxCursorLen {
		return "", bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", bad
	}
	var body cursorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", bad
	}
	id, err := domain.ParseID(body.ID)
	if err != nil {
		return "", bad
	}
	return id, nil
}

// query wraps URL query parameters and collects the errors found while
// reading them, so a request reports all of them at once (400, schema
// FieldError with the parameter name as field).
type query struct {
	v    url.Values
	errs []fieldError
}

// newQuery checks r's query string against the parameters an operation
// declares; unknown parameters are errors (additionalProperties: false in
// spirit, "nieznany parametr" in the contract).
func newQuery(r *http.Request, known ...string) *query {
	q := &query{v: r.URL.Query()}
	keys := make([]string, 0, len(q.v))
	for k := range q.v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(known, k) {
			q.fail(k, codeInvalidFormat, "Unknown query parameter.")
		}
	}
	return q
}

func (q *query) fail(field, code, detail string) {
	q.errs = append(q.errs, fieldError{Field: field, Code: code, Detail: detail})
}

// str returns the single value of name; several values are an error.
func (q *query) str(name string) string {
	vs := q.v[name]
	switch len(vs) {
	case 0:
		return ""
	case 1:
		return vs[0]
	}
	q.fail(name, codeInvalidFormat, "Must be given at most once.")
	return ""
}

func (q *query) all(name string) []string { return q.v[name] }

// id reads an Id parameter (1..64 characters).
func (q *query) id(name string) domain.ID {
	s := q.str(name)
	if s != "" && len(s) > 64 {
		q.fail(name, codeOutOfRange, "Must be at most 64 characters.")
		return ""
	}
	return domain.ID(s)
}

// limit reads "limit" (1..200, default 50).
func (q *query) limit() int {
	s := q.str("limit")
	if s == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		q.fail("limit", codeInvalidFormat, "Must be an integer.")
		return defaultLimit
	}
	if n < 1 || n > maxLimit {
		q.fail("limit", codeOutOfRange, fmt.Sprintf("Must be between 1 and %d.", maxLimit))
		return defaultLimit
	}
	return n
}

// cursor reads and decodes "cursor".
func (q *query) cursor() domain.ID {
	id, err := decodeCursor(q.str("cursor"))
	if err != nil {
		q.fail("cursor", codeInvalidFormat, "Not a cursor issued by this server.")
		return ""
	}
	return id
}

// timestamp reads an RFC 3339 parameter; the zero time means absent.
func (q *query) timestamp(name string) (t time.Time) {
	s := q.str(name)
	if s == "" {
		return t
	}
	v, ok := parseTimestamp(s)
	if !ok {
		q.fail(name, codeInvalidFormat, "Must be an RFC 3339 timestamp.")
		return t
	}
	return v
}

// err returns the 400 problem for the collected errors, or nil.
func (q *query) err() error {
	if len(q.errs) == 0 {
		return nil
	}
	return malformed("Invalid query parameters.", q.errs...)
}

// pageOf cuts one page out of items, which must be sorted by ID (ascending,
// or descending when desc). Used for resources the store returns whole
// (devices, channels, users, tokens); messages are paged by the store.
func pageOf[T any](items []T, id func(T) domain.ID, after domain.ID, limit int, desc bool) (page []T, next string) {
	slices.SortFunc(items, func(a, b T) int {
		c := compareIDs(id(a), id(b))
		if desc {
			return -c
		}
		return c
	})
	start := 0
	if after != "" {
		start = sort.Search(len(items), func(i int) bool {
			c := compareIDs(id(items[i]), after)
			if desc {
				return c < 0
			}
			return c > 0
		})
	}
	end := min(start+limit, len(items))
	page = items[start:end]
	if end < len(items) && len(page) > 0 {
		next = encodeCursor(id(page[len(page)-1]))
	}
	return page, next
}

func compareIDs(a, b domain.ID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// pageResponse is the shared page envelope (schema PageMeta + items).
type pageResponse[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func newPage[T any](items []T, next string) pageResponse[T] {
	if items == nil {
		items = []T{}
	}
	p := pageResponse[T]{Items: items}
	if next != "" {
		p.NextCursor = &next
	}
	return p
}

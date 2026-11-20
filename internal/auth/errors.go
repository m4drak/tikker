// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Errors returned by the service. Test them with errors.Is.
var (
	// ErrInvalidCredentials: wrong username or password, or a disabled
	// account. The three cases are deliberately indistinguishable.
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrUnauthenticated: the request carries no credentials.
	ErrUnauthenticated = errors.New("auth: no credentials")
	// ErrInvalidToken: the API token is malformed, unknown, expired,
	// revoked, or its owner is disabled (API 401).
	ErrInvalidToken = errors.New("auth: invalid, expired or revoked token")
	// ErrSetupCompleted: the first-run wizard was used although users
	// already exist. It wraps domain.ErrConflict.
	ErrSetupCompleted = fmt.Errorf("auth: first-run setup already completed: %w", domain.ErrConflict)
	// ErrMalformedHash: a stored password hash is not a supported argon2id
	// PHC string.
	ErrMalformedHash = errors.New("auth: malformed password hash")
	// ErrValidation matches every *ValidationError.
	ErrValidation = errors.New("auth: validation failed")
)

// RateLimitError is returned by Login when the per-IP or per-username
// attempt budget is exhausted (HTTP 429 with Retry-After).
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("auth: too many login attempts, retry in %s", e.RetryAfter)
}

// RetryAfterSeconds is the value of the Retry-After header: whole seconds,
// rounded up, at least 1.
func (e *RateLimitError) RetryAfterSeconds() int {
	s := int((e.RetryAfter + time.Second - 1) / time.Second)
	return max(s, 1)
}

// Field error codes, as listed in api/openapi.yaml.
const (
	CodeRequired         = "required"
	CodeInvalidFormat    = "invalid_format"
	CodeOutOfRange       = "out_of_range"
	CodeScopeExceedsRole = "scope_exceeds_role"
	CodeUnknownUser      = "unknown_user"
	CodeExpiresAtInPast  = "expires_at_in_past"
)

// FieldError describes one invalid input field. Field is a JSON pointer
// into the request body (e.g. "/scopes/1"), matching the API contract.
type FieldError struct {
	Field  string
	Code   string
	Detail string
}

// ValidationError lists invalid input fields (API 422, problem type
// validation). It never contains the rejected values of secrets.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, f := range e.Errors {
		parts = append(parts, f.Field+": "+f.Detail)
	}
	return "auth: validation failed: " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrValidation) true.
func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

// validation collects field errors.
type validation []FieldError

func (v *validation) add(field, code, format string, args ...any) {
	*v = append(*v, FieldError{Field: field, Code: code, Detail: fmt.Sprintf(format, args...)})
}

func (v validation) err() error {
	if len(v) == 0 {
		return nil
	}
	return &ValidationError{Errors: v}
}

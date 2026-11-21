// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/domain"
)

// ChangeOwnPassword changes the password of the signed-in user p after
// checking their current password (so a stolen session cannot lock the owner
// out). Wrong current passwords count against the login attempt limits and
// yield [ErrInvalidCredentials], exhausted limits a *[RateLimitError]; a new
// password that breaks the policy yields a *[ValidationError]. The change
// ends all sessions of the user; a fresh one is started for this request and
// its principal returned.
func (s *Service) ChangeOwnPassword(w http.ResponseWriter, r *http.Request, p Principal, current, next string) (Principal, error) {
	ctx := r.Context()
	ip := s.ClientIP(r)
	ukey := userKey(p.User.Username)
	if wait, ok := s.limiter.allow(limiterIP(ip), ukey, s.clk.Now()); !ok {
		return Principal{}, &RateLimitError{RetryAfter: wait}
	}
	if err := ValidatePassword("/password", next); err != nil {
		return Principal{}, err
	}
	u, reason, err := s.verify(ctx, p.User.Username, current)
	if err != nil {
		return Principal{}, err
	}
	if reason != "" || u.ID != p.User.ID {
		return Principal{}, ErrInvalidCredentials
	}
	s.limiter.succeeded(limiterIP(ip), ukey, s.clk.Now())
	if _, err := s.UpdateUser(ctx, domain.UserActor(u.ID), u.ID, UserChanges{Password: &next}); err != nil {
		return Principal{}, err
	}
	np, err := s.StartSession(w, r, u)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: change password: %w", err)
	}
	return np, nil
}

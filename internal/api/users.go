// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
)

const (
	conflictUserDup   = "A user with this username already exists."
	conflictLastAdmin = "The change would leave no enabled administrator."
)

// userCreate is schema UserCreate. The password type keeps the plaintext out
// of every log and format verb.
type userCreate struct {
	Username *string           `json:"username"`
	Password *secrets.Password `json:"password"`
	Role     *string           `json:"role"`
	Disabled *bool             `json:"disabled"`
}

// listUsers implements GET /users.
func (a *API) listUsers(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := newQuery(r, "cursor", "limit")
	after, limit := q.cursor(), q.limit()
	if err := q.err(); err != nil {
		return err
	}
	users, err := a.st.Users().List(r.Context())
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	page, next := pageOf(users, func(u domain.User) domain.ID { return u.ID }, after, limit, false)
	items := make([]userDTO, len(page))
	for i, u := range page {
		items[i] = toUserDTO(u)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

// getUser implements GET /users/{id}.
func (a *API) getUser(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	u, err := a.st.Users().Get(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		return notFound("User")
	}
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	writeJSON(w, http.StatusOK, toUserDTO(u))
	return nil
}

// createUser implements POST /users.
func (a *API) createUser(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req userCreate
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	var v validation
	in := auth.NewUser{}
	if req.Username == nil {
		v.add("/username", codeRequired, "A username is required.")
	} else {
		in.Username = *req.Username
	}
	if req.Password == nil {
		v.add("/password", codeRequired, "A password is required.")
	} else {
		in.Password = req.Password.Reveal()
	}
	if req.Role == nil {
		v.add("/role", codeRequired, "A role is required.")
	} else {
		in.Role = domain.Role(*req.Role)
	}
	if err := v.err(); err != nil {
		return err
	}
	in.Disabled = req.Disabled != nil && *req.Disabled

	u, err := a.auth.CreateUser(r.Context(), actorOf(p), in)
	if e := authProblem(err, "User", conflictUserDup); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	w.Header().Set("Location", Prefix+"/users/"+string(u.ID))
	writeJSON(w, http.StatusCreated, toUserDTO(u))
	return nil
}

var userPatchFields = []string{"password", "role", "disabled"}

// updateUser implements PATCH /users/{id} (JSON Merge Patch).
func (a *API) updateUser(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	patch, err := decodePatch(raw, userPatchFields...)
	if err != nil {
		return err
	}
	var v validation
	if len(patch) == 0 {
		v.add("", codeRequired, "At least one property is required.")
		return v.err()
	}
	password, hasPass, err := patchValue[secrets.Password](patch, "password", &v)
	if err != nil {
		return err
	}
	role, hasRole, err := patchValue[string](patch, "role", &v)
	if err != nil {
		return err
	}
	disabled, hasDisabled, err := patchValue[bool](patch, "disabled", &v)
	if err != nil {
		return err
	}
	if err := v.err(); err != nil {
		return err
	}
	var ch auth.UserChanges
	if hasPass {
		s := password.Reveal()
		ch.Password = &s
	}
	if hasRole {
		rl := domain.Role(role)
		ch.Role = &rl
	}
	if hasDisabled {
		ch.Disabled = &disabled
	}
	u, err := a.auth.UpdateUser(r.Context(), actorOf(p), id, ch)
	if e := authProblem(err, "User", conflictLastAdmin); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	writeJSON(w, http.StatusOK, toUserDTO(u))
	return nil
}

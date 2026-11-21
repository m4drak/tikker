// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// ---------------------------------------------------------------------------
// Users

type userRepo struct{ h *handle }

var _ store.UserRepo = (*userRepo)(nil)

const userCols = `id, username, password_hash, role, disabled, created_at`

func scanUser(sc scanner) (domain.User, error) {
	var u domain.User
	var id, role string
	var disabled int
	var created int64
	if err := sc.Scan(&id, &u.Username, &u.PasswordHash, &role, &disabled, &created); err != nil {
		return domain.User{}, err
	}
	u.ID = domain.ID(id)
	u.Role = domain.Role(role)
	u.Disabled = disabled != 0
	u.CreatedAt = fromMS(created)
	return u, nil
}

// Create implements [store.UserRepo].
func (r *userRepo) Create(ctx context.Context, u domain.User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = r.h.s.clk.Now()
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
			string(u.ID), u.Username, u.PasswordHash, string(u.Role), boolInt(u.Disabled), toMS(u.CreatedAt))
		if err != nil {
			return fmt.Errorf("sqlite: create user: %w", mapErr(err))
		}
		return nil
	})
}

func (r *userRepo) one(ctx context.Context, where string, arg any) (domain.User, error) {
	u, err := scanUser(r.h.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, notFound("user", arg)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("sqlite: get user: %w", err)
	}
	return u, nil
}

// Get implements [store.UserRepo].
func (r *userRepo) Get(ctx context.Context, id domain.ID) (domain.User, error) {
	return r.one(ctx, "id = ?", string(id))
}

// GetByUsername implements [store.UserRepo]. The column is declared
// COLLATE NOCASE, so the comparison ignores ASCII case.
func (r *userRepo) GetByUsername(ctx context.Context, username string) (domain.User, error) {
	return r.one(ctx, "username = ?", username)
}

// List implements [store.UserRepo].
func (r *userRepo) List(ctx context.Context) ([]domain.User, error) {
	rows, err := r.h.r.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list users: %w", err)
	}
	return out, nil
}

func countActiveAdmins(ctx context.Context, q execer) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count admins: %w", err)
	}
	return n, nil
}

// Update implements [store.UserRepo]. Demoting or disabling the last enabled
// admin yields domain.ErrConflict.
func (r *userRepo) Update(ctx context.Context, u domain.User) error {
	return r.h.write(ctx, func(q execer) error {
		var role string
		var disabled int
		err := q.QueryRowContext(ctx, `SELECT role, disabled FROM users WHERE id = ?`, string(u.ID)).Scan(&role, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("user", u.ID)
		}
		if err != nil {
			return fmt.Errorf("sqlite: update user: %w", err)
		}
		wasActiveAdmin := role == string(domain.RoleAdmin) && disabled == 0
		staysActiveAdmin := u.Role == domain.RoleAdmin && !u.Disabled
		if wasActiveAdmin && !staysActiveAdmin {
			n, err := countActiveAdmins(ctx, q)
			if err != nil {
				return err
			}
			if n <= 1 {
				return fmt.Errorf("%w: cannot demote or disable the last active admin", domain.ErrConflict)
			}
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE users SET username = ?, password_hash = ?, role = ?, disabled = ? WHERE id = ?`,
			u.Username, u.PasswordHash, string(u.Role), boolInt(u.Disabled), string(u.ID)); err != nil {
			return fmt.Errorf("sqlite: update user: %w", mapErr(err))
		}
		return nil
	})
}

// Count implements [store.UserRepo].
func (r *userRepo) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.h.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count users: %w", err)
	}
	return n, nil
}

// CountActiveAdmins implements [store.UserRepo].
func (r *userRepo) CountActiveAdmins(ctx context.Context) (int, error) {
	return countActiveAdmins(ctx, r.h.r)
}

// ---------------------------------------------------------------------------
// Tokens

type tokenRepo struct{ h *handle }

var _ store.TokenRepo = (*tokenRepo)(nil)

const tokenCols = `id, user_id, name, prefix, hash, scopes, expires_at, last_used_at, created_at` //nolint:gosec // column list, not a credential

func scanToken(sc scanner) (domain.APIToken, error) {
	var t domain.APIToken
	var id, user, scopes string
	var expires, used, created int64
	if err := sc.Scan(&id, &user, &t.Name, &t.Prefix, &t.Hash, &scopes, &expires, &used, &created); err != nil {
		return domain.APIToken{}, err
	}
	t.ID = domain.ID(id)
	t.UserID = domain.ID(user)
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return domain.APIToken{}, fmt.Errorf("sqlite: corrupt scopes %q: %w", scopes, err)
	}
	t.ExpiresAt = fromMS(expires)
	t.LastUsedAt = fromMS(used)
	t.CreatedAt = fromMS(created)
	return t, nil
}

// Create implements [store.TokenRepo].
func (r *tokenRepo) Create(ctx context.Context, t domain.APIToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = r.h.s.clk.Now()
	}
	scopes := t.Scopes
	if scopes == nil {
		scopes = domain.Scopes{}
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO api_tokens (`+tokenCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(t.ID), string(t.UserID), t.Name, t.Prefix, t.Hash, marshalJSON(scopes),
			toMS(t.ExpiresAt), toMS(t.LastUsedAt), toMS(t.CreatedAt))
		if err != nil {
			return fmt.Errorf("sqlite: create token: %w", mapErr(err))
		}
		return nil
	})
}

// GetByPrefix implements [store.TokenRepo].
func (r *tokenRepo) GetByPrefix(ctx context.Context, prefix string) (domain.APIToken, error) {
	t, err := scanToken(r.h.r.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE prefix = ?`, prefix))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.APIToken{}, notFound("token", prefix)
	}
	if err != nil {
		return domain.APIToken{}, fmt.Errorf("sqlite: get token: %w", err)
	}
	return t, nil
}

// List implements [store.TokenRepo].
func (r *tokenRepo) List(ctx context.Context, userID domain.ID) ([]domain.APIToken, error) {
	query := `SELECT ` + tokenCols + ` FROM api_tokens`
	var args []any
	if !userID.IsZero() {
		query += ` WHERE user_id = ?`
		args = append(args, string(userID))
	}
	query += ` ORDER BY id DESC`
	rows, err := r.h.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list tokens: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list tokens: %w", err)
	}
	return out, nil
}

// Delete implements [store.TokenRepo].
func (r *tokenRepo) Delete(ctx context.Context, id domain.ID) error {
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, string(id))
		if err != nil {
			return fmt.Errorf("sqlite: delete token: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("token", id)
		}
		return nil
	})
}

// TouchLastUsed implements [store.TokenRepo]. Writes are coalesced: the
// stored value moves only when at is at least Options.TouchInterval later.
func (r *tokenRepo) TouchLastUsed(ctx context.Context, id domain.ID, at time.Time) error {
	interval := r.h.s.opts.TouchInterval
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE api_tokens SET last_used_at = ? WHERE id = ? AND last_used_at <= ?`,
			toMS(at), string(id), toMS(at.Add(-interval)))
		if err != nil {
			return fmt.Errorf("sqlite: touch token: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil || n > 0 {
			return err
		}
		var one int
		err = q.QueryRowContext(ctx, `SELECT 1 FROM api_tokens WHERE id = ?`, string(id)).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("token", id)
		}
		return err
	})
}

// ---------------------------------------------------------------------------
// Audit

type auditRepo struct{ h *handle }

var _ store.AuditRepo = (*auditRepo)(nil)

// Append implements [store.AuditRepo].
func (r *auditRepo) Append(ctx context.Context, e domain.AuditLog) error {
	if e.At.IsZero() {
		e.At = r.h.s.clk.Now()
	}
	meta := e.Meta
	if meta == nil {
		meta = map[string]string{}
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO audit_log (id, actor, action, target, meta, at) VALUES (?, ?, ?, ?, ?, ?)`,
			string(e.ID), actorString(e.Actor), string(e.Action), e.Target, marshalJSON(meta), toMS(e.At))
		if err != nil {
			return fmt.Errorf("sqlite: append audit: %w", mapErr(err))
		}
		return nil
	})
}

// List implements [store.AuditRepo].
func (r *auditRepo) List(ctx context.Context, f store.AuditFilter, page store.PageRequest) (store.Page[domain.AuditLog], error) {
	page = page.Normalized()
	cursor, err := pageCursor(page.Cursor)
	if err != nil {
		return store.Page[domain.AuditLog]{}, err
	}
	conds := []string{"1 = 1"}
	var args []any
	if !f.Actor.IsZero() {
		conds = append(conds, "actor = ?")
		args = append(args, f.Actor.String())
	}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, string(f.Action))
	}
	if !f.Since.IsZero() {
		conds = append(conds, "at >= ?")
		args = append(args, toMS(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "at < ?")
		args = append(args, toMS(f.Until))
	}
	if cursor != "" {
		conds = append(conds, "id < ?")
		args = append(args, cursor)
	}
	args = append(args, page.Limit+1)
	query := `SELECT id, actor, action, target, meta, at FROM audit_log WHERE `
	for i, c := range conds {
		if i > 0 {
			query += " AND "
		}
		query += c
	}
	query += ` ORDER BY id DESC LIMIT ?`
	rows, err := r.h.r.QueryContext(ctx, query, args...)
	if err != nil {
		return store.Page[domain.AuditLog]{}, fmt.Errorf("sqlite: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []domain.AuditLog
	for rows.Next() {
		var e domain.AuditLog
		var id, actor, action, meta string
		var at int64
		if err := rows.Scan(&id, &actor, &action, &e.Target, &meta, &at); err != nil {
			return store.Page[domain.AuditLog]{}, fmt.Errorf("sqlite: list audit: %w", err)
		}
		e.ID = domain.ID(id)
		e.Action = domain.AuditAction(action)
		e.At = fromMS(at)
		if e.Actor, err = parseActor(actor); err != nil {
			return store.Page[domain.AuditLog]{}, err
		}
		if err := json.Unmarshal([]byte(meta), &e.Meta); err != nil {
			return store.Page[domain.AuditLog]{}, fmt.Errorf("sqlite: corrupt audit meta: %w", err)
		}
		if len(e.Meta) == 0 {
			e.Meta = nil
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return store.Page[domain.AuditLog]{}, fmt.Errorf("sqlite: list audit: %w", err)
	}
	return paginate(items, page.Limit, func(e domain.AuditLog) domain.ID { return e.ID }), nil
}

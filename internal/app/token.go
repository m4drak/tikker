// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

// TokenRequest describes an API token created from the command line
// (`tikker token create`), the only way to obtain the first token while there
// is no web panel.
type TokenRequest struct {
	Username string
	Name     string
	Scopes   domain.Scopes
	// TTL is the token's lifetime; zero means it never expires.
	TTL time.Duration
}

// CreateToken opens the database in cfg.DataDir and creates an API token for
// an existing user. It returns the full token, which is shown only once.
// It works while the server is running (SQLite WAL).
func CreateToken(ctx context.Context, cfg config.Config, log *slog.Logger, req TokenRequest) (domain.APIToken, string, error) {
	dbPath := filepath.Join(cfg.DataDir, DatabaseFile)
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.APIToken{}, "", fmt.Errorf("no database at %s: start `tikker serve` first", dbPath)
		}
		return domain.APIToken{}, "", fmt.Errorf("database: %w", err)
	}
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath, Synchronous: cfg.SQLiteSynchronous})
	if err != nil {
		return domain.APIToken{}, "", fmt.Errorf("database %s: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	svc, err := auth.New(auth.Config{Store: st, Logger: log})
	if err != nil {
		return domain.APIToken{}, "", fmt.Errorf("auth: %w", err)
	}
	u, err := st.Users().GetByUsername(ctx, req.Username)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.APIToken{}, "", fmt.Errorf("user %q does not exist", req.Username)
	}
	if err != nil {
		return domain.APIToken{}, "", fmt.Errorf("find user: %w", err)
	}
	in := auth.NewToken{UserID: u.ID, Name: req.Name, Scopes: req.Scopes}
	if req.TTL > 0 {
		in.ExpiresAt = time.Now().Add(req.TTL)
	}
	tok, full, err := svc.CreateToken(ctx, domain.SystemActor, in)
	if err != nil {
		return domain.APIToken{}, "", fmt.Errorf("create token: %w", err)
	}
	return tok, full, nil
}

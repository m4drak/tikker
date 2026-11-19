-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- Server-side sessions of the web panel (T20). id is the hex SHA-256 of the
-- random token stored in the browser cookie; the token itself is never
-- stored. expires_at is the absolute expiry; the idle timeout is evaluated
-- against last_seen_at, which internal/auth updates at most once a minute
-- per session to limit flash writes. user_agent and ip are truncated and
-- only informational (list of active sessions).
CREATE TABLE sessions (
    id           TEXT    NOT NULL PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    user_agent   TEXT    NOT NULL DEFAULT '',
    ip           TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);
CREATE INDEX idx_sessions_last_seen ON sessions (last_seen_at);

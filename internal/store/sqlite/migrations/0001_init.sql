-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- Initial schema. Conventions:
--   * all times are INTEGER unix milliseconds (UTC); 0 means "not set";
--   * IDs are 26-character ULIDs stored as TEXT; '' means "no reference";
--   * booleans are INTEGER 0/1;
--   * tag lists are JSON arrays of strings (matched with json_each).

CREATE TABLE users (
    id            TEXT    NOT NULL PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    disabled      INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE api_tokens (
    id           TEXT    NOT NULL PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    prefix       TEXT    NOT NULL UNIQUE,
    hash         BLOB    NOT NULL,
    scopes       TEXT    NOT NULL DEFAULT '[]',
    expires_at   INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_api_tokens_user ON api_tokens (user_id);

CREATE TABLE devices (
    id           TEXT    NOT NULL PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    connector    TEXT    NOT NULL,
    base_url     TEXT    NOT NULL,
    username     TEXT    NOT NULL,
    password_enc BLOB    NOT NULL,
    tls_mode     TEXT    NOT NULL,
    ca_pem       TEXT    NOT NULL DEFAULT '',
    fingerprint  TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE channels (
    id             TEXT    NOT NULL PRIMARY KEY,
    device_id      TEXT    NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    port           TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    tags           TEXT    NOT NULL DEFAULT '[]',
    rate_per_min   INTEGER NOT NULL,
    max_segments   INTEGER NOT NULL,
    transliterate  INTEGER NOT NULL DEFAULT 0 CHECK (transliterate IN (0, 1)),
    enabled        INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    state          TEXT    NOT NULL,
    state_reason   TEXT    NOT NULL DEFAULT '',
    last_health_at INTEGER NOT NULL DEFAULT 0,
    signal_rssi    INTEGER,
    signal_rsrp    INTEGER,
    signal_rsrq    INTEGER,
    signal_sinr    INTEGER,
    operator       TEXT    NOT NULL DEFAULT '',
    UNIQUE (device_id, port)
) STRICT;

-- Outbound and inbound messages. channel_id and route_channel_id carry no
-- foreign key on purpose: messages keep the channel ID for history after the
-- channel is deleted.
CREATE TABLE messages (
    id                 TEXT    NOT NULL PRIMARY KEY,
    direction          TEXT    NOT NULL CHECK (direction IN ('out', 'in')),
    channel_id         TEXT    NOT NULL DEFAULT '',
    route_channel_id   TEXT    NOT NULL DEFAULT '',
    route_tags         TEXT    NOT NULL DEFAULT '[]',
    peer               TEXT    NOT NULL,
    body               TEXT    NOT NULL DEFAULT '',
    encoding           TEXT    NOT NULL,
    segments           INTEGER NOT NULL DEFAULT 1,
    status             TEXT    NOT NULL
        CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'cancelled', 'received')),
    attempts           INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    INTEGER NOT NULL DEFAULT 0,
    lease_until        INTEGER NOT NULL DEFAULT 0,
    last_error         TEXT    NOT NULL DEFAULT '',
    expires_at         INTEGER NOT NULL DEFAULT 0,
    possible_duplicate INTEGER NOT NULL DEFAULT 0 CHECK (possible_duplicate IN (0, 1)),
    idempotency_key    TEXT    NOT NULL DEFAULT '',
    client_ref         TEXT    NOT NULL DEFAULT '',
    created_by         TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    sent_at            INTEGER NOT NULL DEFAULT 0,
    received_at        INTEGER NOT NULL DEFAULT 0,
    -- NULL for outbound messages; UNIQUE ignores NULLs.
    dedup_hash         TEXT    UNIQUE
) STRICT;

-- Queue: AcquireNext, NextDue, ExpireQueued and RecoverExpiredLeases.
CREATE INDEX idx_messages_queue   ON messages (status, next_attempt_at);
CREATE INDEX idx_messages_channel ON messages (channel_id);
CREATE INDEX idx_messages_peer    ON messages (peer, id);
CREATE INDEX idx_messages_created ON messages (created_at);

-- One record per Idempotency-Key request (SPEC §4.2); purged after 24 h.
CREATE TABLE idempotency_keys (
    created_by   TEXT    NOT NULL,
    key          TEXT    NOT NULL,
    request_hash TEXT    NOT NULL,
    message_ids  TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (created_by, key)
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_idempotency_created ON idempotency_keys (created_at);

CREATE TABLE audit_log (
    id     TEXT    NOT NULL PRIMARY KEY,
    actor  TEXT    NOT NULL,
    action TEXT    NOT NULL,
    target TEXT    NOT NULL DEFAULT '',
    meta   TEXT    NOT NULL DEFAULT '{}',
    at     INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_audit_at ON audit_log (at);

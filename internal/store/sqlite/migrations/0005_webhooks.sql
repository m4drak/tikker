-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- Webhooks (PLAN §5 #2): subscriptions and a durable delivery queue.
-- Conventions as in 0001_init.sql.

CREATE TABLE webhooks (
    id         TEXT    NOT NULL PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    url        TEXT    NOT NULL,
    secret_enc BLOB    NOT NULL,
    events     TEXT    NOT NULL DEFAULT '[]', -- JSON array of event names
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    -- Bumped when the destination changes or the webhook is disabled: queued
    -- deliveries of an older version are cancelled, never sent elsewhere.
    version    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE webhook_deliveries (
    id              TEXT    NOT NULL PRIMARY KEY,
    webhook_id      TEXT    NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event           TEXT    NOT NULL,
    payload         BLOB    NOT NULL, -- emptied once the delivery is terminal
    webhook_version INTEGER NOT NULL DEFAULT 1,
    status          TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    lease_until     INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    last_http_code  INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    delivered_at    INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Acquire and NextDeliveryDue scan pending rows only.
CREATE INDEX idx_webhook_deliveries_due ON webhook_deliveries (next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX idx_webhook_deliveries_hook ON webhook_deliveries (webhook_id, id);

-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- E-mail notifications of inbound SMS (PLAN §5 #5, ADR 0004): a durable
-- queue like webhook_deliveries. A row references the message and holds no
-- copy of its text. Conventions as in 0001_init.sql.

CREATE TABLE mail_outbox (
    id              TEXT    NOT NULL PRIMARY KEY,
    message_id      TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    lease_until     INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    sent_at         INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Acquire and NextDue scan pending rows only.
CREATE INDEX idx_mail_outbox_due ON mail_outbox (next_attempt_at, id) WHERE status = 'pending';

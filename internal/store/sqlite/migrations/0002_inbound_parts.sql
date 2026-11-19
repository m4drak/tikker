-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- Parts of inbound concatenated SMS (T16). The inbox poller stores every part
-- here in the same transaction that commits complete messages, and only then
-- deletes the entries from the router, so a part never lives only on the
-- router. Rows with message_id = '' are pending (waiting for the other parts).
-- When the message is assembled (or expired as incomplete) the rows are kept
-- as tombstones, with pdu and body emptied, so that a part the router
-- redelivers after a failed deletion is recognised by dedup_hash instead of
-- being stored as the first part of a new message. Tombstones are purged by
-- consumed_at.
CREATE TABLE inbound_parts (
    id            TEXT    NOT NULL PRIMARY KEY,
    channel_id    TEXT    NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    dedup_hash    TEXT    NOT NULL UNIQUE,
    phone         TEXT    NOT NULL,
    raw_timestamp TEXT    NOT NULL DEFAULT '',
    received_at   INTEGER NOT NULL DEFAULT 0,
    body          TEXT    NOT NULL DEFAULT '',
    pdu           TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    message_id    TEXT    NOT NULL DEFAULT '',
    consumed_at   INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX idx_inbound_parts_pending  ON inbound_parts (channel_id, created_at) WHERE message_id = '';
CREATE INDEX idx_inbound_parts_consumed ON inbound_parts (consumed_at) WHERE message_id <> '';

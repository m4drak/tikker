-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- Group sizes of an idempotent request (T21b): POST /messages/batch answers
-- with one group of messages per batch item, and a replay must return the
-- same grouping even if splitting the texts again would give other parts.
-- JSON array of positive integers; '' for records without groups.
ALTER TABLE idempotency_keys ADD COLUMN group_sizes TEXT NOT NULL DEFAULT '';

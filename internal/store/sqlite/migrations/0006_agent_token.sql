-- SPDX-FileCopyrightText: 2026 The tikker Authors
-- SPDX-License-Identifier: GPL-3.0-or-later

-- Agent connector (PLAN §5 #1): the sealed bearer token a router script
-- presents to /agent/v1. NULL for devices without one (routeros-rest).
ALTER TABLE devices ADD COLUMN agent_token_enc BLOB;

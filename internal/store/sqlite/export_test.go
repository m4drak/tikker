// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import "database/sql"

// WriteDB exposes the write pool to tests.
func (s *Store) WriteDB() *sql.DB { return s.writeDB }

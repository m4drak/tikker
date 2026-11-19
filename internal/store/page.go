// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package store

// Page size limits for cursor pagination (SPEC §5).
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 500
)

// PageRequest asks for one page of a list.
//
// Cursor is opaque to callers: an empty cursor starts from the newest item;
// otherwise it is the NextCursor of the previous page. Implementations key
// pages by ID (ULIDs sort by creation time), so pages stay stable while new
// rows are inserted.
type PageRequest struct {
	Cursor string
	Limit  int
}

// Normalized returns p with Limit clamped to [1, MaxPageLimit]; a
// non-positive Limit becomes DefaultPageLimit.
func (p PageRequest) Normalized() PageRequest {
	switch {
	case p.Limit <= 0:
		p.Limit = DefaultPageLimit
	case p.Limit > MaxPageLimit:
		p.Limit = MaxPageLimit
	}
	return p
}

// Page is one page of results. NextCursor is empty on the last page.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

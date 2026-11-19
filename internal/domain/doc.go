// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package domain holds the tikker domain model: entities, enumerations, the
// outbound message state machine, the retry policy and error classification.
//
// The package is a leaf: it depends only on the standard library,
// internal/clock and the ULID library. Every other internal package may import
// it, and it must never import them. That rule keeps the dependency graph
// acyclic:
//
//	domain  <-  connector  <-  connector/routeros, connector/fake
//	domain  <-  store      <-  store/sqlite
//	domain, connector, store  <-  queue, inbox, health, api, web
//
// Entities deliberately carry no JSON tags. Several of them hold secrets
// (password hashes, token hashes, encrypted device passwords), so the HTTP
// layer maps them to explicit DTOs that follow api/openapi.yaml. Enumerations
// implement encoding.TextMarshaler, so DTOs can embed them directly.
//
// Optional timestamps use the zero time.Time to mean "not set"; check them
// with IsZero. Optional references use the empty [ID].
package domain

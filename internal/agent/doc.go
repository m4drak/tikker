// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package agent implements the "agent" connector (PLAN §5 #1): tikker never
// calls the router; a RouterOS script (deploy/routeros/agent.rsc) polls
// tikker over HTTPS instead, so a router behind CGNAT works without a VPN.
//
// A [Hub] keeps one in-memory mailbox per agent device. Its two faces share
// the mailboxes:
//
//   - [Hub.Factory] builds the connector.Connector of a device ([Conn]).
//     Queue, inbox poller and health checker use it like any other
//     connector (see the package connector documentation);
//   - [Hub.Handler] is the HTTP API the script talks to, mounted under
//     [Prefix] ("/agent/v1/"), authenticated by the device's agent token.
//
// Wiring (cmd/tikker via internal/app):
//
//	hub := agent.NewHub(agent.Options{Logger: log, Clock: clk})
//	err := hub.Register(reg)                // connector.Registry
//	mux.Handle(agent.Prefix+"/", hub.Handler())
//
// # Flow
//
//	Send:     Conn.Send queues a command and blocks
//	          -> POST /agent/v1/commands returns it to the script
//	          -> the script sends the SMS and POSTs .../commands/{id}/result
//	          -> Send returns (accepted, or the reported error)
//	Inbound:  the script POSTs /agent/v1/inbox -> Conn.FetchInbox returns it
//	          -> the poller stores it -> Conn.DeleteInbox -> the next
//	          response lists it in "inbox_delete" -> the script removes it on
//	          the router -> POST /agent/v1/inbox/ack
//	Health:   the script POSTs /agent/v1/checkin (version, board, ports with
//	          the modem state) -> Conn.Health / Conn.Probe
//
// # Delivery guarantees (ADR 0002)
//
// A Send whose command the script never fetched fails as a plain Transient
// error: nothing reached the router. A fetched command without a result is
// TransientAmbiguous. The message ID is the idempotency key on both sides:
// the script remembers the IDs it has executed and answers a redelivered
// command with the earlier result, and tikker remembers final results
// (accepted, permanent) for Options.ResultTTL, so a retried Send of an
// already accepted message completes without a second SMS. Mailboxes are
// not persisted: after a restart of tikker, the dispatcher recovers the
// leases and redelivers, and the script's deduplication answers with the
// earlier result.
//
// The protocol is documented for administrators in docs/agent.md. What has
// not been verified on a real router is marked [WERYFIKACJA] there and in
// the script.
package agent

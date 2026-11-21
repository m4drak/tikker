// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

// registerPages registers the panel pages behind a session; each page file
// has its own register function.
func (s *Server) registerPages() {
	s.registerDashboard()
	s.registerSend()
	s.registerMessages()
	s.registerDevices()
	s.registerChannels()
	s.registerTokens()
	s.registerUsers()
	s.registerAudit()
}

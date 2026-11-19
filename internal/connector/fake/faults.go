// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"net/http"
	"time"
)

// Endpoint names one emulated REST call. The values are also used in the
// /_fake control API.
type Endpoint string

// Emulated endpoints. EndpointAny matches every endpoint when used with
// FailNext.
const (
	EndpointAny         Endpoint = "*"
	EndpointSend        Endpoint = "send"         // POST /rest/tool/sms/send
	EndpointInbox       Endpoint = "inbox"        // GET /rest/tool/sms/inbox
	EndpointInboxDelete Endpoint = "inbox.delete" // DELETE /rest/tool/sms/inbox/{id}
	EndpointInboxRemove Endpoint = "inbox.remove" // POST /rest/tool/sms/inbox/remove
	EndpointSettings    Endpoint = "settings"     // GET /rest/tool/sms
	EndpointLTE         Endpoint = "lte"          // GET /rest/interface/lte
	EndpointLTEMonitor  Endpoint = "lte.monitor"  // POST /rest/interface/lte/monitor
	EndpointResource    Endpoint = "resource"     // GET /rest/system/resource
	EndpointRouterboard Endpoint = "routerboard"  // GET /rest/system/routerboard
	EndpointUnknown     Endpoint = "unknown"      // anything else under /rest
)

// FaultKind is how a Fault misbehaves.
type FaultKind int

const (
	// FaultHTTP answers with an RouterOS-style JSON error.
	FaultHTTP FaultKind = iota
	// FaultDrop closes the connection without a response.
	FaultDrop
	// FaultHang never answers; after Options.SessionTimeout the call ends
	// with 400 "Session closed" like RouterOS REST does after 60 s.
	FaultHang
)

// Fault describes a forced failure for FailNext.
type Fault struct {
	Kind FaultKind
	// Status, Message and Detail shape a FaultHTTP body:
	// {"error":Status,"message":Message,"detail":Detail}. Message defaults to
	// the HTTP status text, an empty Detail is omitted.
	Status  int
	Message string
	Detail  string
	// Accepted (EndpointSend only) makes the router take the SMS first: it
	// is recorded in Sent() with Ambiguous set and only then does the fault
	// strike. This is the "timeout after accepting" case a client cannot
	// distinguish from a failure.
	Accepted bool
	// Delay is waited before the fault strikes.
	Delay time.Duration
}

// HTTPError answers with the given status and detail.
func HTTPError(status int, detail string) Fault {
	return Fault{Kind: FaultHTTP, Status: status, Detail: detail}
}

// Unauthorized answers 401, as for bad credentials or a missing rest-api policy.
func Unauthorized() Fault { return HTTPError(http.StatusUnauthorized, "") }

// NotAllowed answers 500 "not allowed (9)", as for a user without the api policy.
func NotAllowed() Fault { return HTTPError(http.StatusInternalServerError, "not allowed (9)") }

// SessionClosed answers 400 "Session closed" (RouterOS REST 60 s limit).
func SessionClosed() Fault { return HTTPError(http.StatusBadRequest, "Session closed") }

// DropConnection closes the connection without answering.
func DropConnection() Fault { return Fault{Kind: FaultDrop} }

// Hang never answers (until the session timeout or the client gives up).
func Hang() Fault { return Fault{Kind: FaultHang} }

// AfterAccept returns f marked Accepted (see Fault.Accepted).
func (f Fault) AfterAccept() Fault { f.Accepted = true; return f }

// After returns f delayed by d.
func (f Fault) After(d time.Duration) Fault { f.Delay = d; return f }

type faultEntry struct {
	fault     Fault
	remaining int // < 0: forever
}

// FailNext makes the next n calls to ep fail with f (n < 0: until
// ClearFaults). Faults for one endpoint are used in the order added; a fault
// registered for EndpointAny is used only when ep has none queued. The fault
// strikes after authentication, so the call is still counted by Requests.
func (s *Server) FailNext(ep Endpoint, f Fault, n int) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[ep] = append(s.faults[ep], &faultEntry{fault: f, remaining: n})
}

// ClearFaults removes all queued faults.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = map[Endpoint][]*faultEntry{}
}

// popFault returns the fault to apply to a call of ep, if any. s.mu must not
// be held.
func (s *Server) popFault(ep Endpoint) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range []Endpoint{ep, EndpointAny} {
		q := s.faults[key]
		if len(q) == 0 {
			continue
		}
		e := q[0]
		f := e.fault
		if e.remaining > 0 {
			e.remaining--
			if e.remaining == 0 {
				s.faults[key] = q[1:]
			}
		}
		return &f
	}
	return nil
}

// act carries out a fault that is not deferred to the send handler.
func (s *Server) act(w http.ResponseWriter, r *http.Request, f *Fault) {
	if f.Delay > 0 && !s.sleep(f.Delay) {
		return
	}
	switch f.Kind {
	case FaultDrop:
		panic(http.ErrAbortHandler)
	case FaultHang:
		s.mu.Lock()
		timeout := s.sessionTimeout
		s.mu.Unlock()
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-t.C:
			writeErr(w, http.StatusBadRequest, "Session closed")
		case <-r.Context().Done():
		case <-s.done:
		}
	default:
		status := f.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		body := errBody{Error: status, Message: f.Message, Detail: f.Detail}
		if body.Message == "" {
			body.Message = http.StatusText(status)
		}
		writeJSON(w, status, body)
	}
}

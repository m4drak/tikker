// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

// Inbox field names (research W8). ASSUMPTION(T00): "port" exists only on
// some versions (if at all); without it the inbox belongs to the port set in
// /tool sms.
const (
	fieldID        = ".id"
	fieldPhone     = "phone"
	fieldTimestamp = "timestamp"
	fieldType      = "type"
	fieldMessage   = "message"
	fieldPDU       = "pdu"
	fieldPort      = "port"
)

// timestampLayouts are tried in order to parse inbox timestamps.
// ASSUMPTION(T00): the real format is unknown (W8); ISO and the console's
// "nov/19/2026 12:00:00" form are accepted (month names parse
// case-insensitively).
var timestampLayouts = []string{
	"2006-01-02 15:04:05",
	"Jan/02/2006 15:04:05",
	time.RFC3339,
}

func (c *Conn) parseTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range timestampLayouts {
		if t, err := time.ParseInLocation(layout, s, c.loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

// FetchInbox implements connector.Connector. Entries are returned in the
// router's order (ASSUMPTION(T00): oldest first) with the raw "pdu" for
// internal/sms to decode.
func (c *Conn) FetchInbox(ctx context.Context, ch connector.ChannelRef) ([]connector.InboundSMS, error) {
	resp, err := c.do(ctx, call{op: opFetchInbox, method: http.MethodGet, path: "/tool/sms/inbox"})
	if err != nil {
		c.log.Warn("inbox fetch failed", slog.String("port", ch.Port), slog.Any("error", err))
		return nil, err
	}
	var items []record
	if err := decode(opFetchInbox, "/tool/sms/inbox", resp, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	withPort := false
	for _, it := range items {
		if it.has(fieldPort) {
			withPort = true
			break
		}
	}
	if !withPort {
		recv, err := c.receivePort(ctx)
		if err != nil {
			return nil, err
		}
		if recv != "" && recv != ch.Port {
			// The inbox belongs to another port of this device.
			return nil, nil
		}
	}

	out := make([]connector.InboundSMS, 0, len(items))
	for _, it := range items {
		if withPort && it.str(fieldPort) != ch.Port {
			continue
		}
		raw := it.str(fieldTimestamp)
		out = append(out, connector.InboundSMS{
			RemoteID:     it.str(fieldID),
			Phone:        it.str(fieldPhone),
			RawTimestamp: raw,
			Timestamp:    c.parseTimestamp(raw),
			Body:         it.str(fieldMessage),
			Type:         it.str(fieldType),
			PDU:          it.str(fieldPDU),
		})
	}
	c.log.Debug("inbox fetched", slog.String("port", ch.Port), slog.Int("count", len(out)))
	return out, nil
}

// receivePort returns the port /tool sms receives on.
func (c *Conn) receivePort(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, call{op: opFetchInbox, method: http.MethodGet, path: "/tool/sms"})
	if err != nil {
		return "", err
	}
	var settings record
	if err := decode(opFetchInbox, "/tool/sms", resp, &settings); err != nil {
		return "", err
	}
	return settings.str("port"), nil
}

// Delete variants, detected on first use and remembered per device.
const (
	deleteUnknown  int32 = iota
	deleteByPath         // DELETE /rest/tool/sms/inbox/{id}
	deleteByRemove       // POST /rest/tool/sms/inbox/remove {".id": id}
)

// DeleteInbox implements connector.Connector. It removes the entries one by
// one, so that one vanished entry does not fail the others; entries that no
// longer exist count as deleted.
func (c *Conn) DeleteInbox(ctx context.Context, ch connector.ChannelRef, remoteIDs []string) error {
	var errs []error
	deleted := 0
	for _, id := range remoteIDs {
		if id == "" {
			continue
		}
		if strings.ContainsAny(id, ",/?#") {
			errs = append(errs, localError(domain.KindPermanent, opDeleteInbox, fmt.Errorf("invalid inbox id %q", id)))
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, localError(domain.KindTransient, opDeleteInbox, err))
			break
		}
		if err := c.deleteOne(ctx, id); err != nil {
			errs = append(errs, err)
			if domain.KindOf(err) == domain.KindAuth {
				break
			}
			continue
		}
		deleted++
	}
	if len(errs) > 0 {
		err := errors.Join(errs...)
		c.log.Warn("inbox delete failed", slog.String("port", ch.Port), slog.Int("deleted", deleted), slog.Any("error", err))
		// Keep the first connector error's class visible through errors.As.
		return &joined{first: errs[0], all: err}
	}
	c.log.Debug("inbox entries deleted", slog.String("port", ch.Port), slog.Int("count", deleted))
	return nil
}

// joined reports all errors but unwraps to the first one, so that
// domain.KindOf classifies by it.
type joined struct{ first, all error }

func (j *joined) Error() string { return j.all.Error() }
func (j *joined) Unwrap() error { return j.first }

func (c *Conn) deleteOne(ctx context.Context, id string) error {
	if c.delVariant.Load() != deleteByRemove {
		resp, err := c.do(ctx, call{op: opDeleteInbox, method: http.MethodDelete, path: "/tool/sms/inbox/" + url.PathEscape(id)})
		switch {
		case err == nil:
			c.delVariant.CompareAndSwap(deleteUnknown, deleteByPath)
			return nil
		case resp != nil && isNoSuchItem(resp):
			// ASSUMPTION(T00): 404 "no such item" = entry already gone (W10).
			c.delVariant.CompareAndSwap(deleteUnknown, deleteByPath)
			return nil
		case resp != nil && pathUnsupported(resp):
			if c.delVariant.CompareAndSwap(deleteUnknown, deleteByRemove) {
				c.log.Info("inbox DELETE not supported, using POST /tool/sms/inbox/remove", slog.Int("status", resp.status))
			} else {
				c.delVariant.Store(deleteByRemove)
			}
		default:
			return err
		}
	}
	resp, err := c.do(ctx, call{op: opDeleteInbox, method: http.MethodPost, path: "/tool/sms/inbox/remove", body: map[string]string{".id": id}})
	if err != nil && resp != nil && isNoSuchItem(resp) {
		// ASSUMPTION(T00): 400 "no such item (4)" = already gone.
		return nil
	}
	return err
}

func isNoSuchItem(r *response) bool {
	return (r.status == http.StatusNotFound || r.status == http.StatusBadRequest) &&
		strings.Contains(strings.ToLower(parseRouterError(r.body)), "no such item")
}

// pathUnsupported: the DELETE-by-path form is not available.
// ASSUMPTION(T00): 405, 404 without "no such item", or 400 "no such command".
func pathUnsupported(r *response) bool {
	switch r.status {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusNotFound:
		return true // isNoSuchItem was checked first
	case http.StatusBadRequest:
		return strings.Contains(strings.ToLower(parseRouterError(r.body)), "no such command")
	}
	return false
}

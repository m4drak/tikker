// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

// /interface/lte monitor fields. ASSUMPTION(T00): names and value strings
// follow the RouterOS console; registration "home"/"roaming" (and
// "registered") mean attached to a network.
const (
	monRegistration = "registration-status"
	monOperator     = "current-operator"
	monTechnology   = "access-technology"
	monRSSI         = "rssi"
	monRSRP         = "rsrp"
	monRSRQ         = "rsrq"
	monSINR         = "sinr"
)

var registeredStates = map[string]bool{"home": true, "roaming": true, "registered": true}

// monitorOnce is the "once" argument of monitor.
// ASSUMPTION(T00): REST accepts a JSON true (the console flag has no value;
// some clients send ""). Without "once" monitor streams until the 60 s limit.
const monitorOnce = true

// lteInterface is one entry of GET /interface/lte.
type lteInterface struct {
	name     string
	running  bool
	disabled bool
}

func (c *Conn) lteInterfaces(ctx context.Context, op string) ([]lteInterface, error) {
	resp, err := c.do(ctx, call{op: op, method: http.MethodGet, path: "/interface/lte"})
	if err != nil {
		return nil, err
	}
	var items []record
	if err := decode(op, "/interface/lte", resp, &items); err != nil {
		return nil, err
	}
	out := make([]lteInterface, 0, len(items))
	for _, it := range items {
		name := it.str("name")
		if name == "" {
			continue
		}
		out = append(out, lteInterface{name: name, running: it.boolean("running"), disabled: it.boolean("disabled")})
	}
	return out, nil
}

// Health implements connector.Connector.
func (c *Conn) Health(ctx context.Context, ch connector.ChannelRef) (connector.ChannelHealth, error) {
	ifaces, err := c.lteInterfaces(ctx, opHealth)
	if err != nil {
		c.log.Warn("health check failed", slog.String("port", ch.Port), slog.Any("error", err))
		return connector.ChannelHealth{}, err
	}
	for _, it := range ifaces {
		if it.name == ch.Port {
			h, err := c.portHealth(ctx, opHealth, it)
			if err != nil {
				c.log.Warn("health check failed", slog.String("port", ch.Port), slog.Any("error", err))
			}
			return h, err
		}
	}
	return connector.ChannelHealth{}, localError(domain.KindPermanent, opHealth, fmt.Errorf("%w: %q", ErrUnknownPort, ch.Port))
}

// portHealth reads the monitor of a known interface. A disabled or stopped
// interface is reported without asking the modem.
// ASSUMPTION(T00): monitor on a stopped interface fails or hangs.
func (c *Conn) portHealth(ctx context.Context, op string, it lteInterface) (connector.ChannelHealth, error) {
	h := connector.ChannelHealth{CheckedAt: c.clk.Now(), Running: it.running && !it.disabled}
	if !h.Running {
		return h, nil
	}
	resp, err := c.do(ctx, call{
		op: op, method: http.MethodPost, path: "/interface/lte/monitor",
		body: map[string]any{"numbers": it.name, "once": monitorOnce},
	})
	if err != nil {
		return h, err
	}
	var items []record
	if err := decode(op, "/interface/lte/monitor", resp, &items); err != nil {
		return h, err
	}
	if len(items) == 0 {
		return h, &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Code: domain.CodeUnexpectedResponse,
			Err: fmt.Errorf("/interface/lte/monitor: %w: empty result", ErrUnexpectedResponse)}
	}
	m := items[0]
	h.Registration = m.str(monRegistration)
	h.Registered = registeredStates[strings.ToLower(h.Registration)]
	h.Operator = m.str(monOperator)
	h.Technology = m.str(monTechnology)
	sig := domain.Signal{
		RSSI: parseLevel(m.str(monRSSI)), RSRP: parseLevel(m.str(monRSRP)),
		RSRQ: parseLevel(m.str(monRSRQ)), SINR: parseLevel(m.str(monSINR)),
	}
	if sig.RSSI != nil || sig.RSRP != nil || sig.RSRQ != nil || sig.SINR != nil {
		h.Signal = &sig
	}
	return h, nil
}

// parseLevel reads a signal value such as "-67", "-67dBm" or "-10.5dB",
// rounded to an integer; nil when absent or unparsable.
func parseLevel(s string) *int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] == '-' || s[end] == '+' || s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	if end == 0 {
		return nil
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1000 {
		return nil
	}
	v := int(math.Round(f))
	return &v
}

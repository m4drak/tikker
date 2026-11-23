// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// ---------------------------------------------------------------------------
// Devices

type deviceRepo struct{ h *handle }

var _ store.DeviceRepo = (*deviceRepo)(nil)

const deviceCols = `id, name, connector, base_url, username, password_enc, tls_mode, ca_pem,
	fingerprint, enabled, created_at, agent_token_enc`

func scanDevice(sc scanner) (domain.Device, error) {
	var d domain.Device
	var id, connector, tls string
	var enabled int
	var created int64
	err := sc.Scan(&id, &d.Name, &connector, &d.BaseURL, &d.Username, &d.PasswordEnc, &tls,
		&d.CAPEM, &d.Fingerprint, &enabled, &created, &d.AgentTokenEnc)
	if err != nil {
		return domain.Device{}, err
	}
	d.ID = domain.ID(id)
	d.Connector = domain.ConnectorKind(connector)
	d.TLSMode = domain.TLSMode(tls)
	d.Enabled = enabled != 0
	d.CreatedAt = fromMS(created)
	return d, nil
}

// Create implements [store.DeviceRepo].
func (r *deviceRepo) Create(ctx context.Context, d domain.Device) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = r.h.s.clk.Now()
	}
	pw := d.PasswordEnc
	if pw == nil {
		pw = []byte{}
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO devices (`+deviceCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(d.ID), d.Name, string(d.Connector), d.BaseURL, d.Username, pw, string(d.TLSMode),
			d.CAPEM, d.Fingerprint, boolInt(d.Enabled), toMS(d.CreatedAt), blobArg(d.AgentTokenEnc))
		if err != nil {
			return fmt.Errorf("sqlite: create device: %w", mapErr(err))
		}
		return nil
	})
}

// Get implements [store.DeviceRepo].
func (r *deviceRepo) Get(ctx context.Context, id domain.ID) (domain.Device, error) {
	d, err := scanDevice(r.h.r.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Device{}, notFound("device", id)
	}
	if err != nil {
		return domain.Device{}, fmt.Errorf("sqlite: get device: %w", err)
	}
	return d, nil
}

// List implements [store.DeviceRepo].
func (r *deviceRepo) List(ctx context.Context) ([]domain.Device, error) {
	rows, err := r.h.r.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list devices: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list devices: %w", err)
	}
	return out, nil
}

// Update implements [store.DeviceRepo].
func (r *deviceRepo) Update(ctx context.Context, d domain.Device) error {
	var pw any // NULL keeps the stored password
	if d.PasswordEnc != nil {
		pw = d.PasswordEnc
	}
	tok := blobArg(d.AgentTokenEnc) // NULL keeps the stored agent token
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE devices SET name = ?, connector = ?, base_url = ?, username = ?,
			        password_enc = COALESCE(?, password_enc), tls_mode = ?, ca_pem = ?,
			        fingerprint = ?, enabled = ?, agent_token_enc = COALESCE(?, agent_token_enc)
			  WHERE id = ?`,
			d.Name, string(d.Connector), d.BaseURL, d.Username, pw, string(d.TLSMode), d.CAPEM,
			d.Fingerprint, boolInt(d.Enabled), tok, string(d.ID))
		if err != nil {
			return fmt.Errorf("sqlite: update device: %w", mapErr(err))
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("device", d.ID)
		}
		return nil
	})
}

// blobArg maps a nil slice to SQL NULL.
func blobArg(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

// Delete implements [store.DeviceRepo].
func (r *deviceRepo) Delete(ctx context.Context, id domain.ID) error {
	return r.h.write(ctx, func(q execer) error {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE id = ?`, string(id)).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("device", id)
		}
		if err != nil {
			return fmt.Errorf("sqlite: delete device: %w", err)
		}
		err = q.QueryRowContext(ctx,
			`SELECT 1 FROM messages
			  WHERE status IN ('queued', 'sending')
			    AND (channel_id IN (SELECT id FROM channels WHERE device_id = ?1)
			      OR route_channel_id IN (SELECT id FROM channels WHERE device_id = ?1))
			  LIMIT 1`, string(id)).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: device %s has active messages", domain.ErrConflict, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: delete device: %w", err)
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, string(id)); err != nil {
			return fmt.Errorf("sqlite: delete device: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Channels

type channelRepo struct{ h *handle }

var _ store.ChannelRepo = (*channelRepo)(nil)

const channelCols = `c.id, c.device_id, c.port, c.name, c.tags, c.rate_per_min, c.max_segments,
	c.transliterate, c.enabled, c.state, c.state_reason, c.last_health_at, c.signal_rssi,
	c.signal_rsrp, c.signal_rsrq, c.signal_sinr, c.operator`

func scanChannel(sc scanner) (domain.Channel, error) {
	var c domain.Channel
	var id, device, tags, state string
	var translit, enabled int
	var health int64
	var rssi, rsrp, rsrq, sinr sql.NullInt64
	err := sc.Scan(&id, &device, &c.Port, &c.Name, &tags, &c.RatePerMin, &c.MaxSegments,
		&translit, &enabled, &state, &c.StateReason, &health, &rssi, &rsrp, &rsrq, &sinr, &c.Operator)
	if err != nil {
		return domain.Channel{}, err
	}
	c.ID = domain.ID(id)
	c.DeviceID = domain.ID(device)
	if c.Tags, err = parseTags(tags); err != nil {
		return domain.Channel{}, err
	}
	c.Transliterate = translit != 0
	c.Enabled = enabled != 0
	c.State = domain.ChannelState(state)
	c.LastHealthAt = fromMS(health)
	if rssi.Valid || rsrp.Valid || rsrq.Valid || sinr.Valid {
		c.Signal = &domain.Signal{RSSI: nullInt(rssi), RSRP: nullInt(rsrp), RSRQ: nullInt(rsrq), SINR: nullInt(sinr)}
	}
	return c, nil
}

func signalArgs(s *domain.Signal) (rssi, rsrp, rsrq, sinr any) {
	if s == nil {
		return nil, nil, nil, nil
	}
	return intArg(s.RSSI), intArg(s.RSRP), intArg(s.RSRQ), intArg(s.SINR)
}

// Create implements [store.ChannelRepo]. An empty State is stored as
// ChannelDown (not checked yet).
func (r *channelRepo) Create(ctx context.Context, c domain.Channel) error {
	if c.State == "" {
		c.State = domain.ChannelDown
	}
	rssi, rsrp, rsrq, sinr := signalArgs(c.Signal)
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO channels (id, device_id, port, name, tags, rate_per_min, max_segments,
			        transliterate, enabled, state, state_reason, last_health_at, signal_rssi,
			        signal_rsrp, signal_rsrq, signal_sinr, operator)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(c.ID), string(c.DeviceID), c.Port, c.Name, tagsJSON(c.Tags), c.RatePerMin,
			c.MaxSegments, boolInt(c.Transliterate), boolInt(c.Enabled), string(c.State),
			c.StateReason, toMS(c.LastHealthAt), rssi, rsrp, rsrq, sinr, c.Operator)
		if err != nil {
			return fmt.Errorf("sqlite: create channel: %w", mapErr(err))
		}
		return nil
	})
}

func getChannel(ctx context.Context, q execer, id domain.ID) (domain.Channel, error) {
	c, err := scanChannel(q.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels c WHERE c.id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Channel{}, notFound("channel", id)
	}
	if err != nil {
		return domain.Channel{}, fmt.Errorf("sqlite: get channel: %w", err)
	}
	return c, nil
}

// Get implements [store.ChannelRepo].
func (r *channelRepo) Get(ctx context.Context, id domain.ID) (domain.Channel, error) {
	return getChannel(ctx, r.h.r, id)
}

// List implements [store.ChannelRepo].
func (r *channelRepo) List(ctx context.Context, f store.ChannelFilter) ([]domain.Channel, error) {
	var conds []string
	var args []any
	if !f.DeviceID.IsZero() {
		conds = append(conds, "c.device_id = ?")
		args = append(args, string(f.DeviceID))
	}
	if f.EnabledOnly {
		conds = append(conds, "c.enabled = 1 AND d.enabled = 1")
	}
	if len(f.States) > 0 {
		conds = append(conds, "c.state IN ("+placeholders(len(f.States))+")")
		for _, s := range f.States {
			args = append(args, string(s))
		}
	}
	if len(f.Tags) > 0 {
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM json_each(?) w
		                        WHERE w.value NOT IN (SELECT value FROM json_each(c.tags)))`)
		args = append(args, tagsJSON(f.Tags))
	}
	query := `SELECT ` + channelCols + ` FROM channels c JOIN devices d ON d.id = c.device_id`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY c.name, c.id"
	rows, err := r.h.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list channels: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list channels: %w", err)
	}
	return out, nil
}

// Update implements [store.ChannelRepo].
func (r *channelRepo) Update(ctx context.Context, c domain.Channel) error {
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE channels SET name = ?, tags = ?, rate_per_min = ?, max_segments = ?,
			        transliterate = ?, enabled = ? WHERE id = ?`,
			c.Name, tagsJSON(c.Tags), c.RatePerMin, c.MaxSegments, boolInt(c.Transliterate),
			boolInt(c.Enabled), string(c.ID))
		if err != nil {
			return fmt.Errorf("sqlite: update channel: %w", mapErr(err))
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("channel", c.ID)
		}
		return nil
	})
}

func equalInts(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalSignal(a, b *domain.Signal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalInts(a.RSSI, b.RSSI) && equalInts(a.RSRP, b.RSRP) &&
		equalInts(a.RSRQ, b.RSRQ) && equalInts(a.SINR, b.SINR)
}

// UpdateHealth implements [store.ChannelRepo]. When nothing but the
// timestamp changed and the stored one is younger than Options.HealthRefresh,
// it does not write.
func (r *channelRepo) UpdateHealth(ctx context.Context, id domain.ID, h store.ChannelHealthUpdate) error {
	refresh := r.h.s.opts.HealthRefresh
	return r.h.write(ctx, func(q execer) error {
		cur, err := getChannel(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.State == h.State && cur.StateReason == h.StateReason && cur.Operator == h.Operator &&
			equalSignal(cur.Signal, h.Signal) && !cur.LastHealthAt.IsZero() &&
			h.At.Sub(cur.LastHealthAt) < refresh && !h.At.Before(cur.LastHealthAt) {
			return nil
		}
		rssi, rsrp, rsrq, sinr := signalArgs(h.Signal)
		if _, err := q.ExecContext(ctx,
			`UPDATE channels SET state = ?, state_reason = ?, last_health_at = ?, signal_rssi = ?,
			        signal_rsrp = ?, signal_rsrq = ?, signal_sinr = ?, operator = ?
			  WHERE id = ?`,
			string(h.State), h.StateReason, toMS(h.At), rssi, rsrp, rsrq, sinr, h.Operator, string(id)); err != nil {
			return fmt.Errorf("sqlite: update channel health: %w", err)
		}
		return nil
	})
}

// Delete implements [store.ChannelRepo].
func (r *channelRepo) Delete(ctx context.Context, id domain.ID) error {
	return r.h.write(ctx, func(q execer) error {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM channels WHERE id = ?`, string(id)).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("channel", id)
		}
		if err != nil {
			return fmt.Errorf("sqlite: delete channel: %w", err)
		}
		err = q.QueryRowContext(ctx,
			`SELECT 1 FROM messages
			  WHERE status IN ('queued', 'sending') AND (channel_id = ?1 OR route_channel_id = ?1) LIMIT 1`,
			string(id)).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: channel %s has active messages", domain.ErrConflict, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: delete channel: %w", err)
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, string(id)); err != nil {
			return fmt.Errorf("sqlite: delete channel: %w", err)
		}
		return nil
	})
}

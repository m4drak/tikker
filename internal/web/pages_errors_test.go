// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
)

var errBoom = errors.New("boom: secret detail")

// flaky wraps a store and fails the selected calls.
type flaky struct {
	store.Store
	msgList, msgCount, msgGet, chans, devs, users, tx bool
}

type flakyMsgs struct {
	store.MessageRepo
	f *flaky
}

func (m flakyMsgs) List(ctx context.Context, fl store.MessageFilter, p store.PageRequest) (store.Page[domain.Message], error) {
	if m.f.msgList {
		return store.Page[domain.Message]{}, errBoom
	}
	return m.MessageRepo.List(ctx, fl, p)
}

func (m flakyMsgs) Count(ctx context.Context, fl store.MessageFilter) (int, error) {
	if m.f.msgCount {
		return 0, errBoom
	}
	return m.MessageRepo.Count(ctx, fl)
}

func (m flakyMsgs) Get(ctx context.Context, id domain.ID) (domain.Message, error) {
	if m.f.msgGet {
		return domain.Message{}, errBoom
	}
	return m.MessageRepo.Get(ctx, id)
}

type flakyChans struct {
	store.ChannelRepo
	f *flaky
}

func (c flakyChans) List(ctx context.Context, fl store.ChannelFilter) ([]domain.Channel, error) {
	if c.f.chans {
		return nil, errBoom
	}
	return c.ChannelRepo.List(ctx, fl)
}

type flakyDevs struct {
	store.DeviceRepo
	f *flaky
}

func (d flakyDevs) List(ctx context.Context) ([]domain.Device, error) {
	if d.f.devs {
		return nil, errBoom
	}
	return d.DeviceRepo.List(ctx)
}

type flakyUsers struct {
	store.UserRepo
	f *flaky
}

func (u flakyUsers) Get(ctx context.Context, id domain.ID) (domain.User, error) {
	if u.f.users {
		return domain.User{}, errBoom
	}
	return u.UserRepo.Get(ctx, id)
}

func (f *flaky) Messages() store.MessageRepo { return flakyMsgs{f.Store.Messages(), f} }
func (f *flaky) Channels() store.ChannelRepo { return flakyChans{f.Store.Channels(), f} }
func (f *flaky) Devices() store.DeviceRepo   { return flakyDevs{f.Store.Devices(), f} }
func (f *flaky) Users() store.UserRepo       { return flakyUsers{f.Store.Users(), f} }
func (f *flaky) WithTx(ctx context.Context, fn func(tx store.Repos) error) error {
	if f.tx {
		return errBoom
	}
	return f.Store.WithTx(ctx, fn)
}

// newFlakyEnv builds a panel whose pages and sending service use a store that
// can be made to fail; sessions keep using the healthy one.
func newFlakyEnv(t *testing.T) (*env, *flaky) {
	t.Helper()
	var f *flaky
	e := newEnv(t, allRoles, func(o *Options, _ *auth.Config) {
		f = &flaky{Store: o.Store}
		o.Store = f
		svc, err := sending.New(sending.Deps{
			Store: f, Capabilities: &fakeCaps{def: modem}, Notifier: &fakeNotifier{}, Options: sending.Options{DefaultRegion: "PL"},
		})
		if err != nil {
			t.Fatal(err)
		}
		o.Sending = svc
	})
	return e, f
}

func TestStoreFailuresGive500(t *testing.T) {
	type tcase struct {
		name   string
		fail   func(*flaky)
		method string
		path   func(e *env) string
		form   url.Values
		role   string
	}
	msgPath := func(e *env) string {
		return "/messages/" + string(e.message("+48600100200").ID)
	}
	tests := []tcase{
		{"dashboard channels", func(f *flaky) { f.chans = true }, "GET", func(*env) string { return "/" }, nil, "viewer1"},
		{"dashboard devices", func(f *flaky) { f.devs = true }, "GET", func(*env) string { return "/dashboard/status" }, nil, "viewer1"},
		{"dashboard count", func(f *flaky) { f.msgCount = true }, "GET", func(*env) string { return "/" }, nil, "viewer1"},
		{"dashboard list", func(f *flaky) { f.msgList = true }, "GET", func(*env) string { return "/dashboard/status" }, nil, "viewer1"},
		{"messages list", func(f *flaky) { f.msgList = true }, "GET", func(*env) string { return "/messages" }, nil, "viewer1"},
		{"messages count", func(f *flaky) { f.msgCount = true }, "GET", func(*env) string { return "/messages" }, nil, "viewer1"},
		{"messages channels", func(f *flaky) { f.chans = true }, "GET", func(*env) string { return "/messages" }, nil, "viewer1"},
		{"message get", func(f *flaky) { f.msgGet = true }, "GET", msgPath, nil, "viewer1"},
		{"message channels", func(f *flaky) { f.chans = true }, "GET", func(e *env) string {
			c := e.channel("lte1")
			return "/messages/" + string(e.message("+48600100200", inChannel(c)).ID)
		}, nil, "viewer1"},
		{"message author", func(f *flaky) { f.users = true }, "GET", func(e *env) string {
			return "/messages/" + string(e.message("+48600100200", byUser(e.user("operator1"))).ID)
		}, nil, "viewer1"},
		{"cancel", func(f *flaky) { f.tx = true }, "POST", func(e *env) string { return msgPath(e) + "/cancel" }, url.Values{}, "operator1"},
		{"send form", func(f *flaky) { f.chans = true }, "GET", func(*env) string { return "/send" }, nil, "operator1"},
		{"send", func(f *flaky) { f.chans = true }, "POST", func(*env) string { return "/send" }, url.Values{"to": {"600100200"}, "body": {"x"}}, "operator1"},
		{"preview", func(f *flaky) { f.devs = true }, "POST", func(*env) string { return "/send/preview" }, url.Values{"body": {"x"}}, "operator1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, f := newFlakyEnv(t)
			c := e.login(tt.role)
			path := tt.path(e)
			tt.fail(f)
			var w = e.get(path, c, "en")
			if tt.method == "POST" {
				w = e.post(path, tt.form, c, "en")
			}
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status %d\n%s", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "boom") {
				t.Error("the error text leaked into the page")
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	t.Run("unknown enum values have a label", func(t *testing.T) {
		if statusKey("weird") != "status.unknown" || stateKey("weird") != "state.unknown" || directionKey("weird") != "direction.unknown" {
			t.Error("fallback keys")
		}
		if encodingLabel("weird") != "weird" || encodingLabel(domain.EncodingUCS2) != "UCS-2" {
			t.Error("encoding labels")
		}
	})
	t.Run("time", func(t *testing.T) {
		if formatTime(time.Time{}) != "-" || formatTime(t0.In(time.FixedZone("x", 3600))) != "2026-03-04 10:00:00" {
			t.Error("formatTime")
		}
	})
	t.Run("preview", func(t *testing.T) {
		if got := preview("  a \n\n b  ", 10); got != "a b" {
			t.Errorf("preview = %q", got)
		}
		if got := preview(strings.Repeat("ż", 100), 5); got != "żżżżż…" {
			t.Errorf("preview = %q", got)
		}
	})
	t.Run("channel name falls back to the port", func(t *testing.T) {
		if channelName(domain.Channel{Port: "lte1"}) != "lte1" || channelName(domain.Channel{Port: "lte1", Name: "Main"}) != "Main" {
			t.Error("channelName")
		}
	})
	t.Run("signal", func(t *testing.T) {
		rssi := -70
		if signalText(nil) != "" || signalText(&domain.Signal{}) != "" || signalText(&domain.Signal{RSSI: &rssi}) != "RSSI -70 dBm" {
			t.Error("signalText")
		}
	})
	t.Run("tags", func(t *testing.T) {
		got := parseTags(" Main, eu;\tvip\nx ,, ")
		if strings.Join(got, "|") != "main|eu|vip|x" || parseTags("") != nil {
			t.Errorf("parseTags = %q", got)
		}
	})
	t.Run("send problems fall back to a generic text", func(t *testing.T) {
		e := newEnv(t, allRoles)
		field, msg := e.srv.sendProblem("en", sending.FieldError{Field: "/client_ref", Code: "out_of_range"})
		if field != "client_ref" || msg != "The value is invalid." {
			t.Errorf("%s %q", field, msg)
		}
		field, msg = e.srv.sendProblem("pl", sending.FieldError{Field: "/anything", Code: "too_many_segments"})
		if field != "anything" || !strings.Contains(msg, "limit segmentów") {
			t.Errorf("%s %q", field, msg)
		}
		field, _ = e.srv.sendProblem("en", sending.FieldError{Field: "/tags/3", Code: "invalid_format"})
		if field != "tags" {
			t.Errorf("tags field = %q", field)
		}
	})
}

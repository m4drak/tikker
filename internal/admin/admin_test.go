// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

const (
	routerUser = "tikker"
	routerPass = "r0uter-Secret-Pa55"
	fpUpper    = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
)

type reloadCounter struct {
	n   int
	err error
}

func (r *reloadCounter) Reload(context.Context) error { r.n++; return r.err }

type checker struct {
	st  health.ChannelStatus
	err error
}

func (c checker) CheckNow(context.Context, domain.ID) (health.ChannelStatus, error) {
	return c.st, c.err
}

type env struct {
	t      *testing.T
	ctx    context.Context
	svc    *Service
	st     *sqlite.Store
	router *fake.Server
	url    string
	rel    *reloadCounter
	creds  *secrets.DeviceCredentials
	reg    *connector.Registry
	clk    *clock.Fake
}

func ptr[T any](v T) *T { return &v }

func newEnv(t *testing.T, mod ...func(*Deps)) *env {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(clock.System().Now())
	st, err := sqlite.Open(ctx, sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &env{t: t, ctx: ctx, st: st, rel: &reloadCounter{}, clk: clk}
	e.router = fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1", "lte2"}, Now: clk.Now})
	e.url = e.router.Start()
	t.Cleanup(e.router.Close)
	e.reg = connector.NewRegistry()
	if err := routeros.Register(e.reg, routeros.Options{Logger: log, Clock: clk}); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.creds = secrets.NewDeviceCredentials(ring)
	deps := Deps{
		Store: st, Credentials: e.creds, Connectors: e.reg, Reloader: e.rel, Clock: clk, Logger: log,
		Options: Options{AllowedHTTPHosts: []string{"127.0.0.1"}},
	}
	for _, m := range mod {
		m(&deps)
	}
	if e.svc, err = New(deps); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) device(name string) domain.Device {
	e.t.Helper()
	d, err := e.svc.CreateDevice(e.ctx, domain.SystemActor, DeviceCreate{
		Name: &name, BaseURL: &e.url, Username: ptr(routerUser), Password: ptr(secrets.Password(routerPass)),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func fieldsOf(err error) map[string]string {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	m := map[string]string{}
	for _, f := range ve.Errors {
		m[f.Field] = f.Code
	}
	return m
}

func TestNewValidatesDeps(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Error("empty deps accepted")
	}
	e := newEnv(t)
	_, err := New(Deps{Store: e.st, Credentials: e.creds, Connectors: e.reg, Reloader: e.rel, Options: Options{AllowedHTTPHosts: []string{"10.0.0.0/33"}}})
	if err == nil || !strings.Contains(err.Error(), "AllowedHTTPHosts") {
		t.Errorf("err = %v", err)
	}
	if _, err := New(Deps{Store: e.st, Credentials: e.creds, Connectors: e.reg, Reloader: e.rel}); err != nil {
		t.Error(err)
	}
}

func TestHostAllowlist(t *testing.T) {
	if _, err := parseHostAllowlist([]string{"10.0.0.0/33"}); err == nil {
		t.Error("invalid prefix accepted")
	}
	l, err := parseHostAllowlist([]string{"", "172.17.0.1", "192.168.0.0/16", "Gateway.Local", "::1", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"172.17.0.1": true, "172.17.0.2": false, "192.168.5.5": true, "192.169.0.1": false, "gateway.local": true,
		"other.local": false, "::1": true, "fd12::1": true, "fe80::1": false, "::ffff:172.17.0.1": true, "fe80::1%eth0": false,
		"": false,
	} {
		if got := l.allows(host); got != want {
			t.Errorf("allows(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	lower := strings.ToLower(strings.ReplaceAll(fpUpper, ":", ""))
	if got := FormatFingerprint(lower); got != fpUpper {
		t.Errorf("got %s", got)
	}
	if FormatFingerprint("") != "" || FormatFingerprint(fpUpper) != fpUpper {
		t.Error("format is not idempotent")
	}
	if got, ok := NormalizeFingerprint(lower); !ok || got != fpUpper {
		t.Errorf("normalize = %q %v", got, ok)
	}
	if _, ok := NormalizeFingerprint("nope"); ok {
		t.Error("accepted garbage")
	}
}

func TestErrorTypes(t *testing.T) {
	ve := &ValidationError{Errors: []FieldError{{"/a", CodeRequired, "x"}, {"/b", CodeOutOfRange, "y"}}}
	if !errors.Is(ve, ErrValidation) || !strings.Contains(ve.Error(), "/a: x; /b: y") {
		t.Errorf("validation error: %v", ve)
	}
	am := &ActiveMessagesError{Count: 2}
	if !errors.Is(am, domain.ErrConflict) || am.Error() == "" || !errors.Is(ErrDuplicate, domain.ErrConflict) {
		t.Error("conflict errors")
	}
	var v validation
	for i := 0; i < maxReported+5; i++ {
		v.add("/x", CodeRequired, "n")
	}
	if len(v.errs) != maxReported {
		t.Errorf("%d errors kept", len(v.errs))
	}
}

func TestCreateDeviceValidation(t *testing.T) {
	e := newEnv(t)
	pem := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	long := strings.Repeat("x", 300)
	tests := []struct {
		name string
		req  DeviceCreate
		want map[string]string
	}{
		{"empty", DeviceCreate{}, map[string]string{"/name": CodeRequired, "/base_url": CodeRequired, "/username": CodeRequired, "/password": CodeRequired}},
		{"ranges", DeviceCreate{Name: ptr(""), BaseURL: ptr("https://" + long), Username: ptr(long), Password: ptr(secrets.Password(""))},
			map[string]string{"/name": CodeOutOfRange, "/base_url": CodeOutOfRange, "/username": CodeOutOfRange, "/password": CodeOutOfRange}},
		{"scheme", DeviceCreate{Name: ptr("a"), BaseURL: ptr("ftp://x"), Username: ptr("u"), Password: ptr(secrets.Password("p"))}, map[string]string{"/base_url": CodeInvalidFormat}},
		{"userinfo", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://u:p@x"), Username: ptr("u"), Password: ptr(secrets.Password("p"))}, map[string]string{"/base_url": CodeInvalidFormat}},
		{"http", DeviceCreate{Name: ptr("a"), BaseURL: ptr("http://10.1.1.1"), Username: ptr("u"), Password: ptr(secrets.Password("p"))}, map[string]string{"/base_url": CodeInsecureTransport}},
		{"mode", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), TLSMode: ptr("nope")}, map[string]string{"/tls_mode": CodeInvalidFormat}},
		{"fingerprint format", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), TLSMode: ptr("verify"), Fingerprint: ptr("abc")}, map[string]string{"/fingerprint": CodeInvalidFormat}},
		{"fingerprint missing", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), TLSMode: ptr("fingerprint")}, map[string]string{"/fingerprint": CodeRequired}},
		{"ca missing", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), TLSMode: ptr("ca")}, map[string]string{"/ca_pem": CodeRequired}},
		{"ca garbage", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), TLSMode: ptr("ca"), CAPEM: ptr("junk")}, map[string]string{"/ca_pem": CodeInvalidFormat}},
		{"ca unparsable", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), TLSMode: ptr("ca"), CAPEM: &pem}, map[string]string{"/ca_pem": CodeInvalidFormat}},
		{"ca long", DeviceCreate{Name: ptr("a"), BaseURL: ptr("https://x"), Username: ptr("u"), Password: ptr(secrets.Password("p")), CAPEM: ptr(strings.Repeat("x", MaxCAPEM+1))}, map[string]string{"/ca_pem": CodeOutOfRange}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.CreateDevice(e.ctx, domain.SystemActor, tt.req)
			got := fieldsOf(err)
			if len(got) != len(tt.want) {
				t.Fatalf("errors = %v (%v), want %v", got, err, tt.want)
			}
			for f, c := range tt.want {
				if got[f] != c {
					t.Errorf("%s: %q, want %q", f, got[f], c)
				}
			}
		})
	}
	if e.rel.n != 0 {
		t.Error("reload after rejected requests")
	}
	if ds, _ := e.st.Devices().List(e.ctx); len(ds) != 0 {
		t.Errorf("%d devices stored", len(ds))
	}
}

func TestDeviceLifecycle(t *testing.T) {
	e := newEnv(t)
	d := e.device("one")
	if e.rel.n != 1 || len(d.PasswordEnc) == 0 || bytes.Contains(d.PasswordEnc, []byte(routerPass)) {
		t.Fatalf("created %+v, reloads %d", d, e.rel.n)
	}
	if _, err := e.svc.CreateDevice(e.ctx, domain.SystemActor, DeviceCreate{Name: ptr("one"), BaseURL: &e.url, Username: ptr("u"), Password: ptr(secrets.Password("p"))}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := e.svc.GetDevice(e.ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get: %v", err)
	}

	// Patch semantics: nil keeps, empty removes, the password is replaced.
	fp := strings.ToLower(strings.ReplaceAll(fpUpper, ":", ""))
	up, err := e.svc.UpdateDevice(e.ctx, domain.SystemActor, d.ID, DeviceChanges{
		Name: ptr("two"), Username: ptr("other"), TLSMode: ptr("fingerprint"), Fingerprint: &fp, Enabled: ptr(false), CAPEM: ptr(""),
	})
	if err != nil || up.Name != "two" || up.Fingerprint != fpUpper || up.Enabled || up.BaseURL != e.url {
		t.Fatalf("update: %+v %v", up, err)
	}
	got, _ := e.st.Devices().Get(e.ctx, d.ID)
	if !bytes.Equal(got.PasswordEnc, d.PasswordEnc) {
		t.Error("the password changed without being given")
	}
	if _, err := e.svc.UpdateDevice(e.ctx, domain.SystemActor, d.ID, DeviceChanges{Password: ptr(secrets.Password("new"))}); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.st.Devices().Get(e.ctx, d.ID); bytes.Equal(got.PasswordEnc, d.PasswordEnc) {
		t.Error("the password was not replaced")
	}
	// The merged TLS settings must be consistent: removing the fingerprint of
	// a pinned device is refused.
	if _, err := e.svc.UpdateDevice(e.ctx, domain.SystemActor, d.ID, DeviceChanges{Fingerprint: ptr("")}); !errors.Is(err, ErrValidation) {
		t.Errorf("inconsistent merge: %v", err)
	}
	if errs := e.svc.ValidateDeviceChanges(DeviceChanges{Name: ptr(""), TLSMode: ptr("x")}); len(errs) != 2 {
		t.Errorf("validate: %v", errs)
	}
	if _, err := e.svc.UpdateDevice(e.ctx, domain.SystemActor, "missing", DeviceChanges{Name: ptr("x")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	e.device("three")
	if _, err := e.svc.UpdateDevice(e.ctx, domain.SystemActor, d.ID, DeviceChanges{Name: ptr("three")}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename to a taken name: %v", err)
	}

	// Audit entries carry no secrets.
	page, _ := e.st.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Limit: 50})
	for _, l := range page.Items {
		for _, v := range l.Meta {
			if strings.Contains(v, routerPass) || v == "new" {
				t.Errorf("secret in audit meta: %v", l.Meta)
			}
		}
	}

	// A device with an active message cannot be deleted.
	ch := e.channel(d, "lte1")
	m := domain.Message{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Direction: domain.DirectionOut, ChannelID: ch.ID, Route: domain.Route{ChannelID: ch.ID},
		Peer: "+48600100200", Body: "x", Encoding: domain.EncodingGSM7, Segments: 1, CreatedBy: domain.SystemActor, CreatedAt: e.clk.Now()}
	if _, err := e.st.Messages().Enqueue(e.ctx, []domain.Message{m}, store.Idempotency{}); err != nil {
		t.Fatal(err)
	}
	var am *ActiveMessagesError
	if err := e.svc.DeleteDevice(e.ctx, domain.SystemActor, d.ID); !errors.As(err, &am) {
		t.Fatalf("delete with messages: %v", err)
	}
	if err := e.svc.DeleteChannel(e.ctx, domain.SystemActor, ch.ID); !errors.As(err, &am) || am.Count != 1 {
		t.Fatalf("delete channel with messages: %v", err)
	}
	if _, err := e.st.Messages().Cancel(e.ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteDevice(e.ctx, domain.SystemActor, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteDevice(e.ctx, domain.SystemActor, d.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func (e *env) channel(d domain.Device, port string) domain.Channel {
	e.t.Helper()
	id := string(d.ID)
	c, err := e.svc.CreateChannel(e.ctx, domain.SystemActor, ChannelCreate{DeviceID: &id, Port: &port})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func TestChannels(t *testing.T) {
	e := newEnv(t)
	d := e.device("dev")
	id := string(d.ID)

	bad := []struct {
		name string
		req  ChannelCreate
		want string
	}{
		{"no device", ChannelCreate{Port: ptr("lte1")}, "/device_id"},
		{"long device", ChannelCreate{DeviceID: ptr(strings.Repeat("x", 70)), Port: ptr("lte1")}, "/device_id"},
		{"unknown device", ChannelCreate{DeviceID: ptr("nope"), Port: ptr("lte1")}, "/device_id"},
		{"no port", ChannelCreate{DeviceID: &id}, "/port"},
		{"long port", ChannelCreate{DeviceID: &id, Port: ptr(strings.Repeat("x", 40))}, "/port"},
		{"port chars", ChannelCreate{DeviceID: &id, Port: ptr("a b")}, "/port"},
		{"name", ChannelCreate{DeviceID: &id, Port: ptr("p"), Name: ptr(strings.Repeat("x", 70))}, "/name"},
		{"tags", ChannelCreate{DeviceID: &id, Port: ptr("p"), Tags: []string{"Bad Tag"}}, "/tags/0"},
		{"rate", ChannelCreate{DeviceID: &id, Port: ptr("p"), RatePerMin: ptr(0)}, "/rate_per_min"},
		{"segments", ChannelCreate{DeviceID: &id, Port: ptr("p"), MaxSegments: ptr(11)}, "/max_segments"},
	}
	for _, tt := range bad {
		if got := fieldsOf(func() error { _, err := e.svc.CreateChannel(e.ctx, domain.SystemActor, tt.req); return err }()); got[tt.want] == "" {
			t.Errorf("%s: %v", tt.name, got)
		}
	}

	c := e.channel(d, "lte1")
	if !c.Transliterate || c.Name != "dev/lte1" || c.RatePerMin != DefaultRatePerMin || c.MaxSegments != DefaultMaxSegments || !c.Enabled || c.State != domain.ChannelDown {
		t.Errorf("defaults: %+v", c)
	}
	if _, err := e.svc.CreateChannel(e.ctx, domain.SystemActor, ChannelCreate{DeviceID: &id, Port: ptr("lte1")}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate: %v", err)
	}
	long := strings.Repeat("n", 60)
	c2, err := e.svc.CreateChannel(e.ctx, domain.SystemActor, ChannelCreate{DeviceID: &id, Port: ptr("lte2"), Transliterate: ptr(false), Enabled: ptr(false), Tags: []string{"a"}})
	if err != nil || c2.Transliterate || c2.Enabled || len(c2.Tags) != 1 {
		t.Errorf("explicit: %+v %v", c2, err)
	}
	named, err := e.svc.CreateChannel(e.ctx, domain.SystemActor, ChannelCreate{DeviceID: &id, Port: ptr(strings.Repeat("p", 30)), Name: ptr(long)})
	if err != nil || named.Name != long {
		t.Errorf("named: %+v %v", named, err)
	}

	// View: queue depth and effective state.
	v, err := e.svc.View(e.ctx, c, true)
	if err != nil || v.QueueDepth != 0 || v.State() != domain.ChannelDown {
		t.Errorf("view: %+v %v", v, err)
	}
	if got, _, err := e.svc.GetChannel(e.ctx, c.ID); err != nil || got.ID != c.ID {
		t.Errorf("get: %v", err)
	}
	if _, _, err := e.svc.GetChannel(e.ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	if EffectiveState(c, false) != domain.ChannelDisabled || EffectiveState(c2, true) != domain.ChannelDisabled {
		t.Error("effective state")
	}

	// Update: only real changes are reloaded and audited.
	before := e.rel.n
	if _, err := e.svc.UpdateChannel(e.ctx, domain.SystemActor, c.ID, ChannelChanges{Name: &c.Name, Tags: ptr([]string(nil)), RatePerMin: &c.RatePerMin}); err != nil {
		t.Fatal(err)
	}
	if e.rel.n != before {
		t.Error("reload without a change")
	}
	up, err := e.svc.UpdateChannel(e.ctx, domain.SystemActor, c.ID, ChannelChanges{
		Name: ptr("Main"), Tags: ptr([]string{"x", "y"}), RatePerMin: ptr(10), MaxSegments: ptr(2), Transliterate: ptr(false), Enabled: ptr(false),
	})
	if err != nil || up.Name != "Main" || up.RatePerMin != 10 || up.MaxSegments != 2 || up.Transliterate || up.Enabled || len(up.Tags) != 2 || e.rel.n != before+1 {
		t.Errorf("update: %+v %v", up, err)
	}
	if errs := ValidateChannelChanges(ChannelChanges{Name: ptr(""), Tags: ptr([]string{"A"}), RatePerMin: ptr(1000), MaxSegments: ptr(0)}); len(errs) != 4 {
		t.Errorf("validate: %v", errs)
	}
	if _, err := e.svc.UpdateChannel(e.ctx, domain.SystemActor, c.ID, ChannelChanges{RatePerMin: ptr(0)}); !errors.Is(err, ErrValidation) {
		t.Errorf("invalid: %v", err)
	}
	if _, err := e.svc.UpdateChannel(e.ctx, domain.SystemActor, "nope", ChannelChanges{Enabled: ptr(true)}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}

	if err := e.svc.DeleteChannel(e.ctx, domain.SystemActor, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteChannel(e.ctx, domain.SystemActor, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

type failingCreds struct{ Credentials }

func (failingCreds) ConnectorConfig(domain.Device) (connector.DeviceConfig, error) {
	return connector.DeviceConfig{}, errors.New("cannot decrypt")
}

type failingFactory struct{}

func (failingFactory) New(context.Context, connector.DeviceConfig) (connector.Connector, error) {
	return nil, errors.New("no connector: " + routerPass)
}

type viewStub struct{}

func (viewStub) ApplyTo(c *domain.Channel) bool { c.State = domain.ChannelDegraded; return true }

func TestDefaultTransliterateFallbacks(t *testing.T) {
	e := newEnv(t)
	d := e.device("dev")
	// Unknown capabilities: the GSM-7-safe choice.
	svc, err := New(Deps{Store: e.st, Credentials: failingCreds{e.creds}, Connectors: e.reg, Reloader: e.rel})
	if err != nil {
		t.Fatal(err)
	}
	if !svc.DefaultTransliterate(e.ctx, d) {
		t.Error("credentials error: should be on")
	}
	svc, _ = New(Deps{Store: e.st, Credentials: e.creds, Connectors: failingFactory{}, Reloader: e.rel, Health: viewStub{}})
	if !svc.DefaultTransliterate(e.ctx, d) {
		t.Error("connector error: should be on")
	}
	// The live health is overlaid on views.
	c := domain.Channel{ID: "x", Enabled: true, State: domain.ChannelOK}
	svc.Overlay(&c)
	if v, _ := svc.View(e.ctx, c, true); v.State() != domain.ChannelDegraded {
		t.Errorf("overlay: %v", v.State())
	}
}

func TestCheckChannel(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.CheckChannel(e.ctx, "x"); !errors.Is(err, ErrNoChecker) {
		t.Errorf("without checker: %v", err)
	}
	e = newEnv(t, func(d *Deps) { d.Checker = checker{st: health.ChannelStatus{State: domain.ChannelOK}} })
	c := e.channel(e.device("dev"), "lte1")
	if st, err := e.svc.CheckChannel(e.ctx, c.ID); err != nil || st.State != domain.ChannelOK {
		t.Errorf("check: %+v %v", st, err)
	}
	if _, err := e.svc.CheckChannel(e.ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	e = newEnv(t, func(d *Deps) { d.Checker = checker{err: errors.New("boom")} })
	c = e.channel(e.device("dev"), "lte1")
	if _, err := e.svc.CheckChannel(e.ctx, c.ID); err == nil {
		t.Error("check error swallowed")
	}
}

func TestTestDevice(t *testing.T) {
	e := newEnv(t)
	d := e.device("dev")
	e.channel(d, "lte2")

	res, err := e.svc.TestDevice(e.ctx, d.ID)
	if err != nil || !res.OK || res.RouterOSVersion == "" || res.BoardName == "" || len(res.Ports) != 2 || res.Latency == nil {
		t.Fatalf("test: %+v %v", res, err)
	}
	for _, p := range res.Ports {
		if (p.Port == "lte2") != (p.ChannelID != "") {
			t.Errorf("port %s: channel %q", p.Port, p.ChannelID)
		}
	}
	if _, err := e.svc.TestDevice(e.ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}

	// A failed test is a result; the secret never reaches it.
	e.router.SetCredentials("x", "y")
	res, err = e.svc.TestDevice(e.ctx, d.ID)
	if err != nil || res.OK || res.Error == nil || res.Error.Code != domain.CodeAuthFailed || strings.Contains(res.Error.Detail, routerPass) {
		t.Errorf("auth failure: %+v %v", res, err)
	}

	svc, _ := New(Deps{Store: e.st, Credentials: failingCreds{e.creds}, Connectors: e.reg, Reloader: e.rel})
	if res, _ = svc.TestDevice(e.ctx, d.ID); res.Error == nil || !strings.Contains(res.Error.Detail, "cannot be decrypted") {
		t.Errorf("credentials error: %+v", res)
	}
	svc, _ = New(Deps{Store: e.st, Credentials: e.creds, Connectors: failingFactory{}, Reloader: e.rel})
	if res, _ = svc.TestDevice(e.ctx, d.ID); res.Error == nil || strings.Contains(res.Error.Detail, routerPass) || !strings.Contains(res.Error.Detail, "***") {
		t.Errorf("connector error: %+v", res)
	}
}

func TestProbeHelpers(t *testing.T) {
	ctx := context.Background()
	if c := probeErrorCode(ctx, domain.AuthError("x", errors.New("a"))); c != domain.CodeAuthFailed {
		t.Errorf("auth -> %s", c)
	}
	if c := probeErrorCode(ctx, domain.WithCode(domain.Transient("x", errors.New("a")), domain.CodeTLSError)); c != domain.CodeTLSError {
		t.Errorf("code kept -> %s", c)
	}
	if c := probeErrorCode(ctx, domain.WithCode(domain.Transient("x", errors.New("a")), "send_failed")); c != domain.CodeUnexpectedResponse {
		t.Errorf("unknown code -> %s", c)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if c := probeErrorCode(done, errors.New("x")); c != domain.CodeTimeout {
		t.Errorf("expired ctx -> %s", c)
	}
	if got := scrub("password hunter2 here", "hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("scrub = %q", got)
	}
	if got := scrub("ab", "ab"); got != "ab" {
		t.Errorf("short secrets are left alone: %q", got)
	}
	if truncate("abcdef", 3) != "abc..." || truncate("ab", 3) != "ab" {
		t.Error("truncate")
	}
}

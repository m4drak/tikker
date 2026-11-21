// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Command fake-routeros runs the RouterOS REST emulator from
// internal/connector/fake as a stand-alone server for end-to-end tests and
// local development. It is not part of the tikker release.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/m4drak/tikker/internal/connector/fake"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "fake-routeros:", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until ctx ends. If ready is not nil it
// receives the listen address once the socket is open.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready chan<- string) error {
	fs := flag.NewFlagSet("fake-routeros", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	user := fs.String("user", "tikker", "basic-auth user")
	password := fs.String("password", "tikker-test", "basic-auth password")
	ports := fs.String("ports", "lte1", "comma-separated LTE interface names")
	useTLS := fs.Bool("tls", false, "serve HTTPS with a self-signed certificate and print its SHA-256 fingerprint")
	tlsHosts := fs.String("tls-hosts", "", "extra comma-separated DNS names/IPs for the certificate")
	lenient := fs.Bool("lenient", false, "accept messages over 160 characters and non-GSM-7 text")
	control := fs.Bool("control", false, "enable the /_fake/ JSON control API (no authentication; keep it on loopback)")
	sendDelay := fs.Duration("send-delay", 0, "how long each send holds its modem port")
	sessionTimeout := fs.Duration("session-timeout", 60*time.Second, "REST per-request limit before 'Session closed'")
	inboxLimit := fs.Int("inbox-limit", 0, "inbox capacity (0 = unlimited)")
	autoErase := fs.Bool("auto-erase", false, "drop the oldest SMS when the inbox is full")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var portList []string
	for _, p := range strings.Split(*ports, ",") {
		if p = strings.TrimSpace(p); p != "" {
			portList = append(portList, p)
		}
	}
	if len(portList) == 0 {
		return errors.New("at least one port required")
	}

	srv := fake.New(fake.Options{
		User: *user, Password: *password, Ports: portList, Lenient: *lenient,
		SendDelay: *sendDelay, SessionTimeout: *sessionTimeout,
		InboxLimit: *inboxLimit, AutoErase: *autoErase,
	})
	defer srv.Close()

	mux := http.NewServeMux()
	mux.Handle("/", srv)
	if *control {
		mux.Handle(fake.ControlPrefix, srv.ControlHandler())
	}
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	scheme := "http"
	if *useTLS {
		var hosts []string
		for _, h := range strings.Split(*tlsHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		if host, _, err := net.SplitHostPort(*listen); err == nil && host != "" {
			hosts = append(hosts, host)
		}
		cert, fp, err := fake.SelfSignedCert(hosts...)
		if err != nil {
			_ = ln.Close()
			return err
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		scheme = "https"
		fmt.Fprintf(stdout, "certificate SHA-256: %s\n", fp)
		fmt.Fprintf(stdout, "certificate SHA-256 (plain hex): %s\n", strings.ToLower(strings.ReplaceAll(fp, ":", "")))
	}
	addr := ln.Addr().String()
	fmt.Fprintf(stdout, "fake RouterOS listening on %s://%s (ports: %s, control API: %t)\n",
		scheme, addr, strings.Join(portList, ","), *control)
	if ready != nil {
		ready <- addr
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Close() // release hanging requests so Shutdown does not wait for them
		_ = hs.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

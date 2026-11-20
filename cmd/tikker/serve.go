// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/app"
	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
	"github.com/m4drak/tikker/internal/version"
)

// loadConfig loads the configuration and prints every problem to stderr.
func loadConfig(prefix, path string, stderr io.Writer) (config.Config, bool) {
	cfg, err := config.Load(config.Options{Path: path})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: invalid configuration:\n", prefix)
		for _, line := range errorLines(err) {
			_, _ = fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return cfg, false
	}
	return cfg, true
}

// newLogger builds the process logger from the configuration.
func newLogger(cfg config.Config, w io.Writer) (*slog.Logger, error) {
	level, err := logging.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	return logging.New(w, cfg.LogFormat, level)
}

// runServe implements `tikker serve`: it runs the server until ctx ends
// (SIGINT/SIGTERM) and returns exitOK after a clean shutdown. Logs go to
// stdout (SPEC §2.2); start-up failures are also printed to stderr.
func runServe(ctx context.Context, configPath string, stdout, stderr io.Writer) int {
	cfg, ok := loadConfig("tikker serve", configPath, stderr)
	if !ok {
		return exitConfig
	}
	log, err := newLogger(cfg, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tikker serve: %v\n", err)
		return exitConfig
	}
	log.Info("starting tikker", slog.String("version", version.String()), slog.Any("config", cfg))

	a, err := app.New(ctx, app.Options{Config: cfg, Logger: log})
	if err != nil {
		log.Error("start-up failed", slog.Any("error", err))
		_, _ = fmt.Fprintf(stderr, "tikker serve: %v\n", err)
		return exitFailure
	}
	if err := a.Run(ctx); err != nil {
		log.Error("stopped with errors", slog.Any("error", err))
		return exitFailure
	}
	log.Info("tikker stopped")
	return exitOK
}

// scopeList collects repeated --scope flags.
type scopeList []string

func (s *scopeList) String() string { return strings.Join(*s, ",") }

func (s *scopeList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// runToken implements `tikker token create`.
func runToken(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "create" {
		printUsage(stderr)
		return exitConfig
	}
	fs := flag.NewFlagSet("tikker token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.String("user", "admin", "owner of the token")
	name := fs.String("name", "cli", "token name")
	ttl := fs.Duration("ttl", 0, "lifetime (e.g. 720h); 0 never expires")
	var scopes scopeList
	fs.Var(&scopes, "scope", "scope (repeatable or comma-separated)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "tikker token create: %v\n", err)
		printUsage(stderr)
		return exitConfig
	}
	if fs.NArg() != 0 {
		printUsage(stderr)
		return exitConfig
	}
	if len(scopes) == 0 {
		scopes = scopeList{string(domain.ScopeAdmin)}
	}
	var parsed domain.Scopes
	for _, s := range scopes {
		sc, err := domain.ParseScope(s)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "tikker token create: %v\n", err)
			return exitConfig
		}
		parsed = append(parsed, sc)
	}

	cfg, ok := loadConfig("tikker token create", configPath, stderr)
	if !ok {
		return exitConfig
	}
	// Logs of the auth service go to stderr, so stdout carries only the token.
	log, err := newLogger(cfg, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tikker token create: %v\n", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, full, err := app.CreateToken(ctx, cfg, log, app.TokenRequest{Username: *user, Name: *name, Scopes: parsed, TTL: *ttl})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tikker token create: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(stderr, "token %s (%s) created for %s; it is shown only once:\n", tok.ID, tok.Name, *user)
	_, _ = fmt.Fprintln(stdout, full)
	return exitOK
}

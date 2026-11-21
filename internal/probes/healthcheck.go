// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package probes

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// maxProbeBody bounds how much of a probe response is read and discarded.
const maxProbeBody = 64 << 10

// HealthcheckURL returns the URL that `tikker healthcheck` probes for a
// listen address in TIKKER_LISTEN form (host:port). With live set it returns
// the /healthz URL, otherwise /readyz.
//
// A wildcard host (empty, 0.0.0.0, ::) is replaced by 127.0.0.1, because the
// probe runs inside the same machine or container. Any other host is used
// as given. Supported forms include ":8080", "0.0.0.0:8080", "[::]:8080" and
// "[::1]:8080".
func HealthcheckURL(listen string, live bool) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return "", fmt.Errorf("probes: listen address %q must be host:port: %w", listen, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("probes: listen address %q has an invalid port", listen)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	path := "/readyz"
	if live {
		path = "/healthz"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

// Healthcheck sends GET to url with client and returns nil only for 200 OK.
// Any other status, transport error or ctx expiry is an error. The error
// text contains the URL and the status code, never the response body.
func Healthcheck(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("probes: building request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probes: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probes: GET %s: status %d", url, resp.StatusCode)
	}
	return nil
}

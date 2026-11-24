// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/m4drak/tikker/internal/client"
)

// Defaults of the client commands.
const (
	defaultURL     = "http://127.0.0.1:8080"
	defaultTimeout = 10 * time.Second
	// maxBodyBytes bounds a body read from --body-file or stdin. The API
	// accepts at most 1600 characters; this only protects against a huge
	// accidental input.
	maxBodyBytes = 64 << 10
)

// pollInterval is the delay between status checks of `sms send --wait`.
// It is a variable so that tests can shorten it.
var pollInterval = time.Second

const smsUsage = `usage: tikker sms <command> [options]

commands:
  send      queue an SMS:
            sms send --to NUMBER [--body TEXT | --body-file PATH|-] [--channel ID] [--tag T]...
                     [--client-ref R] [--expires-at RFC3339] [--idempotency-key K] [--wait DURATION]
                     (without --body and --body-file the body is read from stdin)
  get       show one message:           sms get ID
  list      list messages (newest first): sms list [--status S]... [--limit N] [--all]
  cancel    cancel a queued message:    sms cancel ID
  inbox     list incoming messages:     sms inbox [--limit N] [--all]

client options (accepted by every command):
  --url URL          tikker address (default: TIKKER_URL, then http://127.0.0.1:8080)
  --token-file PATH  read the API token from PATH (default: the TIKKER_TOKEN environment variable)
  --output FORMAT    text (default) or json (the API response, indented)
  --timeout D        timeout of a single HTTP request (default 10s)

exit codes: 0 success, 1 API or operation error (including a failed message
or a --wait timeout), 2 usage error
`

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// clientFlags are the options shared by every `sms` command.
type clientFlags struct {
	url       string
	tokenFile string
	output    string
	timeout   time.Duration
}

func (f *clientFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.url, "url", "", "tikker address (default TIKKER_URL, then "+defaultURL+")")
	fs.StringVar(&f.tokenFile, "token-file", "", "read the API token from this file (default $TIKKER_TOKEN)")
	fs.StringVar(&f.output, "output", "text", "output format: text or json")
	fs.DurationVar(&f.timeout, "timeout", defaultTimeout, "timeout of a single HTTP request")
}

// usageError marks a problem with the command line (exit code 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// newClient validates the shared options and builds the API client. The token
// is read from the file or the environment and never echoed.
func (f *clientFlags) newClient() (*client.Client, error) {
	if f.output != "text" && f.output != "json" {
		return nil, usagef("invalid --output %q: want text or json", f.output)
	}
	if f.timeout <= 0 {
		return nil, usagef("--timeout must be positive")
	}
	base := f.url
	if base == "" {
		base = os.Getenv("TIKKER_URL")
	}
	if base == "" {
		base = defaultURL
	}
	token := ""
	if f.tokenFile != "" {
		b, err := os.ReadFile(f.tokenFile)
		if err != nil {
			return nil, usagef("read --token-file: %v", err)
		}
		token = strings.TrimSpace(string(b))
	} else {
		token = strings.TrimSpace(os.Getenv("TIKKER_TOKEN"))
	}
	if token == "" {
		return nil, usagef("no API token: set TIKKER_TOKEN or use --token-file PATH (the token is never accepted as a flag)")
	}
	c, err := client.New(base, token, &http.Client{Timeout: f.timeout})
	if err != nil {
		return nil, usagef("%v", err)
	}
	return c, nil
}

// parseInterspersed parses flags that may appear before and after positional
// arguments (`sms get ID --output json`) and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// runSMS implements `tikker sms ...`.
func runSMS(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, smsUsage)
		return exitConfig
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = fmt.Fprint(stdout, smsUsage)
		return exitOK
	}

	var run func(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error
	switch args[0] {
	case "send":
		run = smsSend
	case "get":
		run = smsGet
	case "list":
		run = func(ctx context.Context, a []string, _ io.Reader, w io.Writer) error {
			return smsList(ctx, "list", a, w)
		}
	case "inbox":
		run = func(ctx context.Context, a []string, _ io.Reader, w io.Writer) error {
			return smsList(ctx, "inbox", a, w)
		}
	case "cancel":
		run = smsCancel
	default:
		_, _ = fmt.Fprintf(stderr, "tikker sms: unknown command %q\n\n%s", args[0], smsUsage)
		return exitConfig
	}

	err := run(ctx, args[1:], stdin, stdout)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		_, _ = fmt.Fprint(stdout, smsUsage)
		return exitOK
	}
	var ue usageError
	if errors.As(err, &ue) {
		_, _ = fmt.Fprintf(stderr, "tikker sms %s: %v\n\n%s", args[0], err, smsUsage)
		return exitConfig
	}
	_, _ = fmt.Fprintf(stderr, "tikker sms %s: %v\n", args[0], err)
	return exitFailure
}

// newFlagSet creates a quiet flag set; errors are reported by runSMS.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("tikker sms "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// flagError turns a flag parsing error into a usage error.
func flagError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return usageError{err.Error()}
}

var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

func smsSend(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("send")
	var cf clientFlags
	cf.register(fs)
	var tags stringList
	to := fs.String("to", "", "recipient number")
	body := fs.String("body", "", "message text")
	bodyFile := fs.String("body-file", "", "read the message text from this file (- for stdin)")
	channel := fs.String("channel", "", "channel id")
	fs.Var(&tags, "tag", "routing tag (repeatable)")
	clientRef := fs.String("client-ref", "", "client reference stored with the message")
	expiresAt := fs.String("expires-at", "", "expiry time, RFC 3339")
	idemKey := fs.String("idempotency-key", "", "Idempotency-Key header")
	wait := fs.Duration("wait", 0, "wait up to this long for a terminal status")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagError(err)
	}
	if len(pos) != 0 {
		return usagef("unexpected argument %q", pos[0])
	}
	if strings.TrimSpace(*to) == "" {
		return usagef("--to is required")
	}
	if *wait < 0 {
		return usagef("--wait must not be negative")
	}
	bodySet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "body" {
			bodySet = true
		}
	})
	if bodySet && *bodyFile != "" {
		return usagef("--body and --body-file are mutually exclusive")
	}
	for _, t := range tags {
		if !tagPattern.MatchString(t) || len(t) > 32 {
			return usagef("invalid --tag %q: want 1-32 characters [a-z0-9_.-] starting with a letter or digit", t)
		}
	}
	if *expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *expiresAt); err != nil {
			return usagef("invalid --expires-at %q: want RFC 3339, e.g. 2026-11-25T09:55:00Z", *expiresAt)
		}
	}

	text := *body
	if !bodySet {
		text, err = readBody(*bodyFile, stdin)
		if err != nil {
			return err
		}
	}
	if text == "" {
		return usagef("the message body is empty")
	}

	c, err := cf.newClient()
	if err != nil {
		return err
	}

	resp, raw, err := c.Send(ctx, client.SendRequest{
		To: strings.TrimSpace(*to), Body: text, ChannelID: *channel, Tags: tags,
		ClientRef: *clientRef, ExpiresAt: *expiresAt,
	}, *idemKey)
	if err != nil {
		return err
	}
	if len(resp.Messages) == 0 {
		return errors.New("the server accepted the request but returned no messages")
	}

	if *wait == 0 {
		if cf.output == "json" {
			return writeJSON(stdout, raw)
		}
		return writeMessageLines(stdout, resp.Messages)
	}

	final, raws, werr := waitForTerminal(ctx, c, resp.Messages, *wait)
	if cf.output == "json" {
		if err := writeJSON(stdout, joinMessages(raws)); err != nil {
			return err
		}
	} else if err := writeMessageLines(stdout, final); err != nil {
		return err
	}
	if werr != nil {
		return werr
	}
	for _, m := range final {
		if m.Status != client.StatusSent {
			return fmt.Errorf("message %s ended in status %s%s", m.ID, m.Status, lastErrorSuffix(m))
		}
	}
	return nil
}

func lastErrorSuffix(m client.Message) string {
	if m.LastError != nil && *m.LastError != "" {
		return ": " + *m.LastError
	}
	return ""
}

// readBody returns the message text from path ("-" is stdin) or, without a
// path, from stdin. A single trailing newline is removed. Reading from a
// terminal is refused instead of blocking.
func readBody(path string, stdin io.Reader) (string, error) {
	var r io.Reader
	switch path {
	case "":
		if f, ok := stdin.(*os.File); ok {
			if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				return "", usagef("no message body: use --body TEXT, --body-file PATH, or pipe the text on stdin")
			}
		}
		r = stdin
	case "-":
		r = stdin
	default:
		f, err := os.Open(path) // #nosec G304 -- the path is chosen by the user running the CLI
		if err != nil {
			return "", usagef("read --body-file: %v", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read message body: %w", err)
	}
	if len(data) > maxBodyBytes {
		return "", usagef("the message body is longer than %d bytes", maxBodyBytes)
	}
	s := strings.TrimSuffix(string(data), "\n")
	s = strings.TrimSuffix(s, "\r")
	return s, nil
}

// waitForTerminal polls every message until all are in a terminal status or
// the wait expires. It returns the latest state and raw JSON of each message.
// On timeout the error names the messages that are still pending.
func waitForTerminal(ctx context.Context, c *client.Client, msgs []client.Message, wait time.Duration) ([]client.Message, []json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cur := slices.Clone(msgs)
	raws := make([]json.RawMessage, len(msgs))
	for i, m := range cur {
		raws[i], _ = json.Marshal(m)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			var pending []string
			for _, m := range cur {
				if !client.IsTerminal(m.Status) {
					pending = append(pending, m.ID+" ("+m.Status+")")
				}
			}
			return cur, raws, fmt.Errorf("timed out after %s waiting for: %s", wait, strings.Join(pending, ", "))
		case <-timer.C:
		}
		done := true
		for i, m := range cur {
			if client.IsTerminal(m.Status) {
				continue
			}
			nm, raw, err := c.Get(ctx, m.ID)
			if err != nil {
				if ctx.Err() != nil {
					break // reported as a timeout on the next iteration
				}
				return cur, raws, err
			}
			cur[i], raws[i] = nm, raw
			if !client.IsTerminal(nm.Status) {
				done = false
			}
		}
		if ctx.Err() == nil && done {
			return cur, raws, nil
		}
		timer.Reset(pollInterval)
	}
}

// joinMessages renders {"messages":[...]} from raw message documents.
func joinMessages(raws []json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
	}{raws})
	return b
}

func smsGet(ctx context.Context, args []string, _ io.Reader, stdout io.Writer) error {
	return singleMessage(ctx, "get", args, stdout, func(c *client.Client, id string) (client.Message, json.RawMessage, error) {
		return c.Get(ctx, id)
	})
}

func smsCancel(ctx context.Context, args []string, _ io.Reader, stdout io.Writer) error {
	return singleMessage(ctx, "cancel", args, stdout, func(c *client.Client, id string) (client.Message, json.RawMessage, error) {
		return c.Cancel(ctx, id)
	})
}

// singleMessage implements the commands that take one message id.
func singleMessage(_ context.Context, name string, args []string, stdout io.Writer,
	call func(*client.Client, string) (client.Message, json.RawMessage, error),
) error {
	fs := newFlagSet(name)
	var cf clientFlags
	cf.register(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagError(err)
	}
	if len(pos) != 1 {
		return usagef("want exactly one message ID")
	}
	c, err := cf.newClient()
	if err != nil {
		return err
	}
	m, raw, err := call(c, pos[0])
	if err != nil {
		return err
	}
	if cf.output == "json" {
		return writeJSON(stdout, raw)
	}
	return writeMessageDetail(stdout, m)
}

// smsList implements `sms list` and `sms inbox`.
func smsList(ctx context.Context, name string, args []string, stdout io.Writer) error {
	fs := newFlagSet(name)
	var cf clientFlags
	cf.register(fs)
	limit := fs.Int("limit", 0, "page size, 1-200 (server default 50)")
	all := fs.Bool("all", false, "follow the cursor and print every page")
	var statuses stringList
	if name == "list" {
		fs.Var(&statuses, "status", "only messages in this status (repeatable): "+strings.Join(client.Statuses, ", "))
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagError(err)
	}
	if len(pos) != 0 {
		return usagef("unexpected argument %q", pos[0])
	}
	limitSet := false
	fs.Visit(func(f *flag.Flag) { limitSet = limitSet || f.Name == "limit" })
	if limitSet && (*limit < 1 || *limit > 200) {
		return usagef("--limit must be between 1 and 200")
	}
	for _, s := range statuses {
		if !slices.Contains(client.Statuses, s) {
			return usagef("invalid --status %q: want one of %s", s, strings.Join(client.Statuses, ", "))
		}
	}
	c, err := cf.newClient()
	if err != nil {
		return err
	}

	params := client.ListParams{Statuses: statuses, Limit: *limit}
	if name == "inbox" {
		params.Direction = client.DirectionIn
	}
	var items []client.Message
	var rawItems []json.RawMessage
	var next *string
	for {
		page, raw, err := c.List(ctx, params)
		if err != nil {
			return err
		}
		next = page.NextCursor
		if !*all {
			if cf.output == "json" {
				return writeJSON(stdout, raw)
			}
			items = page.Items
			break
		}
		items = append(items, page.Items...)
		if cf.output == "json" {
			var p struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			rawItems = append(rawItems, p.Items...)
		}
		if next == nil || *next == "" {
			next = nil
			break
		}
		params.Cursor = *next
	}

	if cf.output == "json" {
		if rawItems == nil {
			rawItems = []json.RawMessage{}
		}
		b, _ := json.Marshal(struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor *string           `json:"next_cursor"`
		}{rawItems, next})
		return writeJSON(stdout, b)
	}
	if err := writeMessageTable(stdout, items); err != nil {
		return err
	}
	if next != nil {
		_, err = fmt.Fprintf(stdout, "more messages available: use --all or --limit (next cursor: %s)\n", *next)
	}
	return err
}

// writeJSON pretty-prints a JSON document followed by a newline.
func writeJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("format response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// writeMessageLines prints "ID STATUS PEER" per message (tab separated).
func writeMessageLines(w io.Writer, msgs []client.Message) error {
	for _, m := range msgs {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", m.ID, m.Status, oneLine(m.Peer, 64)); err != nil {
			return err
		}
	}
	return nil
}

// writeMessageTable prints messages as an aligned table.
func writeMessageTable(w io.Writer, msgs []client.Message) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tDIR\tSTATUS\tPEER\tCREATED\tBODY")
	for _, m := range msgs {
		body := "-"
		if m.Body != nil {
			body = oneLine(*m.Body, 48)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Direction, m.Status, oneLine(m.Peer, 32), m.CreatedAt, body)
	}
	return tw.Flush()
}

// writeMessageDetail prints every field of one message as "name: value".
func writeMessageDetail(w io.Writer, m client.Message) error {
	str := func(p *string) string {
		if p == nil {
			return "-"
		}
		return oneLine(*p, 1700)
	}
	rows := [][2]string{
		{"id", m.ID},
		{"direction", m.Direction},
		{"status", m.Status},
		{"peer", oneLine(m.Peer, 64)},
		{"channel", str(m.ChannelID)},
		{"body", str(m.Body)},
		{"encoding", m.Encoding},
		{"segments", fmt.Sprint(m.Segments)},
		{"attempts", fmt.Sprint(m.Attempts)},
		{"next_attempt_at", str(m.NextAttemptAt)},
		{"last_error", str(m.LastError)},
		{"possible_duplicate", fmt.Sprint(m.PossibleDuplicate)},
		{"client_ref", str(m.ClientRef)},
		{"tags", strings.Join(m.Tags, ",")},
		{"expires_at", str(m.ExpiresAt)},
		{"created_by", str(m.CreatedBy)},
		{"created_at", m.CreatedAt},
		{"sent_at", str(m.SentAt)},
		{"received_at", str(m.ReceivedAt)},
	}
	for _, r := range rows {
		v := r[1]
		if v == "" {
			v = "-"
		}
		if _, err := fmt.Fprintf(w, "%-19s %s\n", r[0]+":", v); err != nil {
			return err
		}
	}
	return nil
}

// oneLine collapses whitespace and control characters (so that message text
// cannot inject terminal escapes or extra lines) and truncates to limit runes.
func oneLine(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit-3]) + "..."
	}
	return s
}

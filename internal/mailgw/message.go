// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailgw

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

// Limits of the MIME walk: a message is small (MaxMessageBytes), but a
// crafted one could still nest many empty parts.
const (
	maxMIMEDepth = 5
	maxMIMEParts = 50
	// maxMessageIDLen bounds the Message-ID used as idempotency key.
	maxMessageIDLen = 250
)

// Content error codes, reported in the SMTP reply text like the API problem
// codes.
const (
	codeInvalidFormat = "invalid_format"
	codeRequired      = "required"
	codeHTMLOnly      = "html_only"
	codeCharset       = "unsupported_charset"
	codeEncoding      = "unsupported_encoding"
)

// ContentError is a message the gateway cannot turn into an SMS text; it is
// answered with a permanent (5xx) reply.
type ContentError struct {
	Code   string
	Detail string
}

func (e *ContentError) Error() string { return e.Code + ": " + e.Detail }

func contentErr(code, format string, args ...any) error {
	return &ContentError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// errNoPlain marks a subtree without a usable text/plain part.
var errNoPlain = errors.New("no text/plain part")

// Mail is what the gateway takes from an e-mail.
type Mail struct {
	// MessageID is the Message-ID without angle brackets; empty when the
	// header is missing or unusable.
	MessageID string
	// Subject is the decoded subject.
	Subject string
	// Text is the SMS body: the first text/plain part, decoded, without
	// the signature, and with the subject on the first line when asked.
	Text string
}

// ParseOptions tune [ParseMessage].
type ParseOptions struct {
	// SubjectInBody puts the subject on the first line of the text.
	SubjectInBody bool
}

// ParseMessage extracts the SMS text from a raw RFC 5322 message: the first
// text/plain part that is not an attachment (quoted-printable and base64
// decoded; UTF-8, US-ASCII, ISO-8859-1, ISO-8859-2 and Windows-1250;
// format=flowed joined), cut at the signature separator "-- ". Messages
// with HTML only are rejected. Errors are *ContentError.
func ParseMessage(raw []byte, opts ParseOptions) (Mail, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Mail{}, contentErr(codeInvalidFormat, "Cannot parse the message header.")
	}
	var out Mail
	out.MessageID = messageID(m.Header.Get("Message-Id"))
	if s := m.Header.Get("Subject"); s != "" {
		dec := mime.WordDecoder{CharsetReader: charsetReader}
		if d, err := dec.DecodeHeader(s); err == nil {
			s = d
		}
		out.Subject = cleanLine(s)
	}

	w := walker{}
	text, err := w.extract(textproto.MIMEHeader(m.Header), m.Body, 0)
	switch {
	case errors.Is(err, errNoPlain) && w.sawHTML:
		return Mail{}, contentErr(codeHTMLOnly, "The message has an HTML part only; send it as plain text (text/plain).")
	case errors.Is(err, errNoPlain):
		return Mail{}, contentErr(codeRequired, "The message has no text/plain part.")
	case err != nil:
		return Mail{}, err
	}
	text = normalizeText(text)
	if opts.SubjectInBody && out.Subject != "" {
		if text == "" {
			text = out.Subject
		} else {
			text = out.Subject + "\n" + text
		}
	}
	if text == "" {
		return Mail{}, contentErr(codeRequired, "The message text is empty.")
	}
	out.Text = text
	return out, nil
}

// messageID returns the Message-ID without brackets, or "" when it is not
// a printable ASCII token of reasonable length.
func messageID(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(strings.TrimPrefix(v, "<"), ">")
	if v == "" || len(v) > maxMessageIDLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// walker finds the first text/plain part, depth first.
type walker struct {
	parts   int
	sawHTML bool
}

func (w *walker) extract(h textproto.MIMEHeader, body io.Reader, depth int) (string, error) {
	w.parts++
	if depth > maxMIMEDepth || w.parts > maxMIMEParts {
		return "", contentErr(codeInvalidFormat, "The message has too many or too deeply nested MIME parts.")
	}
	mt, params := "text/plain", map[string]string{}
	if ct := h.Get("Content-Type"); ct != "" {
		var err error
		if mt, params, err = mime.ParseMediaType(ct); err != nil {
			// RFC 2045 §5.2: a malformed type defaults to text/plain.
			mt, params = "text/plain", map[string]string{}
		}
	}
	if disp, _, err := mime.ParseMediaType(h.Get("Content-Disposition")); err == nil && disp == "attachment" {
		return "", errNoPlain
	}
	switch {
	case strings.HasPrefix(mt, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return "", contentErr(codeInvalidFormat, "A multipart part has no boundary.")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			p, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				return "", errNoPlain
			}
			if err != nil {
				return "", contentErr(codeInvalidFormat, "Malformed multipart body.")
			}
			text, err := w.extract(p.Header, p, depth+1)
			if !errors.Is(err, errNoPlain) {
				return text, err
			}
		}
	case mt == "text/plain":
		return decodePart(h, params, body)
	case mt == "text/html":
		w.sawHTML = true
	}
	return "", errNoPlain
}

// decodePart undoes the transfer encoding and the charset of a text/plain
// part.
func decodePart(h textproto.MIMEHeader, params map[string]string, body io.Reader) (string, error) {
	var r io.Reader
	switch cte := strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))); cte {
	case "", "7bit", "8bit", "binary":
		r = body
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &skipSpace{r: body})
	default:
		return "", contentErr(codeEncoding, "Content-Transfer-Encoding %q is not supported.", cleanLine(cte))
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", contentErr(codeInvalidFormat, "The text part is not validly encoded.")
	}
	text, err := decodeCharset(params["charset"], data)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(params["format"], "flowed") {
		text = unflow(text, strings.EqualFold(params["delsp"], "yes"))
	}
	return text, nil
}

// charsets maps the supported single-byte charsets (Polish mail clients
// commonly use ISO-8859-2 and Windows-1250).
var charsets = map[string]encoding.Encoding{
	"iso-8859-1":   charmap.ISO8859_1,
	"latin1":       charmap.ISO8859_1,
	"iso-8859-2":   charmap.ISO8859_2,
	"latin2":       charmap.ISO8859_2,
	"windows-1250": charmap.Windows1250,
	"cp1250":       charmap.Windows1250,
	"windows-1252": charmap.Windows1252,
}

func decodeCharset(name string, data []byte) (string, error) {
	switch cs := strings.ToLower(strings.TrimSpace(name)); cs {
	case "", "us-ascii", "ascii", "utf-8", "utf8":
		if !utf8.Valid(data) {
			return "", contentErr(codeCharset, "The text is not valid UTF-8; declare its charset.")
		}
		return string(data), nil
	default:
		enc, ok := charsets[cs]
		if !ok {
			return "", contentErr(codeCharset, "Charset %q is not supported (use UTF-8, ISO-8859-2 or Windows-1250).", cleanLine(cs))
		}
		out, err := enc.NewDecoder().Bytes(data)
		if err != nil {
			return "", contentErr(codeCharset, "The text is not valid %s.", cs)
		}
		return string(out), nil
	}
}

// charsetReader serves mime.WordDecoder (encoded subjects).
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, ok := charsets[strings.ToLower(charset)]
	if !ok {
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
	return enc.NewDecoder().Reader(input), nil
}

// skipSpace drops whitespace so that base64 lines can be decoded.
type skipSpace struct{ r io.Reader }

func (s *skipSpace) Read(p []byte) (int, error) {
	for {
		n, err := s.r.Read(p)
		j := 0
		for _, b := range p[:n] {
			if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
				p[j] = b
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}

// unflow joins the soft line breaks of format=flowed (RFC 3676): a line
// ending in a space continues on the next one. The signature separator
// "-- " is never flowed. Quoted lines (">") are kept as they are.
func unflow(text string, delsp bool) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if l != "-- " && strings.HasPrefix(l, " ") {
			l = l[1:] // space-stuffing
		}
		soft := l != "-- " && strings.HasSuffix(l, " ") && i < len(lines)-1
		if soft && delsp {
			l = l[:len(l)-1]
		}
		b.WriteString(l)
		if !soft && i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// normalizeText converts line ends, cuts the signature (from a line "-- "),
// drops control characters and trailing spaces, and trims blank lines at
// both ends.
func normalizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "-- " {
			break
		}
		out = append(out, strings.TrimRightFunc(stripControls(l), unicode.IsSpace))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// stripControls removes control characters except tabs (shown as spaces).
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case unicode.IsControl(r), r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
}

// cleanLine makes s safe for one line of a reply or a header: control
// characters are dropped and the result is trimmed.
func cleanLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

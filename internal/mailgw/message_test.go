// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailgw

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func latin2(s string) string {
	b, err := charmap.ISO8859_2.NewEncoder().String(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseMessage(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		subject  bool
		wantText string
		wantID   string
		wantSubj string
		wantCode string
	}{
		{
			name:     "plain text, signature cut",
			raw:      crlf("Message-ID: <abc@example.net>\nSubject: Alarm\n\nDisk full on nas1\n\n-- \nJohn\n"),
			wantText: "Disk full on nas1", wantID: "abc@example.net", wantSubj: "Alarm",
		},
		{
			name:     "no content type is text/plain",
			raw:      "Subject: x\n\n  hello  \n\n",
			wantText: "  hello",
		},
		{
			name:     "subject on the first line",
			raw:      crlf("Subject: =?UTF-8?Q?Za=C5=BC=C3=B3=C5=82=C4=87?=\n\nbody"),
			subject:  true,
			wantText: "Zażółć\nbody", wantSubj: "Zażółć",
		},
		{
			name:     "subject only",
			raw:      crlf("Subject: Ping\n\n-- \nsig"),
			subject:  true,
			wantText: "Ping",
		},
		{
			name: "multipart/alternative takes the plain part",
			raw: crlf(`Subject: x
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="b1"

--b1
Content-Type: text/html; charset=utf-8

<p>HTML</p>
--b1
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Za=C5=BC=C3=B3=C5=82=C4=87 g=C4=99=C5=9Bl=C4=85 ja=C5=BA=C5=84 =
w jednej linii
--b1--
`),
			wantText: "Zażółć gęślą jaźń w jednej linii",
		},
		{
			name: "multipart/mixed: attachment skipped, nested alternative",
			raw: crlf(`Content-Type: multipart/mixed; boundary=outer

--outer
Content-Type: text/plain; name="report.txt"
Content-Disposition: attachment; filename="report.txt"

not this
--outer
Content-Type: multipart/alternative; boundary=inner

--inner
Content-Type: text/plain; charset=us-ascii
Content-Transfer-Encoding: base64

SGVsbG8g
ZnJvbSBi
YXNlNjQ=
--inner--
--outer--
`),
			wantText: "Hello from base64",
		},
		{
			name:     "iso-8859-2",
			raw:      "Content-Type: text/plain; charset=ISO-8859-2\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + latin2("Łódź, źdźbło"),
			wantText: "Łódź, źdźbło",
		},
		{
			name:     "windows-1250 subject and body",
			raw:      "Subject: =?windows-1250?Q?=A3=F3d=9F?=\r\nContent-Type: text/plain; charset=windows-1250\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=A3=F3d=9F",
			subject:  true,
			wantText: "Łódź\nŁódź",
		},
		{
			name:     "format=flowed",
			raw:      crlf("Content-Type: text/plain; charset=utf-8; format=flowed\n\nThis is a long \nline. \n >quoted\n-- \nsig"),
			wantText: "This is a long line. >quoted",
		},
		{
			name:     "format=flowed delsp",
			raw:      crlf("Content-Type: text/plain; format=flowed; delsp=yes\n\nabc \ndef"),
			wantText: "abcdef",
		},
		{
			name:     "control characters removed",
			raw:      "Subject: a\x07b\r\n\r\nx\x00y\tz\r\n",
			wantText: "xy z", wantSubj: "a b",
		},
		{
			name:     "bad Message-ID ignored",
			raw:      "Message-ID: <a b@c>\r\n\r\nx",
			wantText: "x",
		},
		{name: "html only", raw: "Content-Type: text/html\r\n\r\n<b>x</b>", wantCode: codeHTMLOnly},
		{name: "html in alternative only", raw: crlf("Content-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/html\n\n<b>x</b>\n--b--\n"), wantCode: codeHTMLOnly},
		{name: "attachment only", raw: crlf("Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: application/pdf\n\nxx\n--b--\n"), wantCode: codeRequired},
		{name: "empty", raw: "Subject: x\r\n\r\n   \r\n", wantCode: codeRequired},
		{name: "no header end", raw: "garbage", wantCode: codeInvalidFormat},
		{name: "multipart without boundary", raw: "Content-Type: multipart/mixed\r\n\r\nx", wantCode: codeInvalidFormat},
		{name: "unknown charset", raw: "Content-Type: text/plain; charset=koi8-r\r\n\r\nx", wantCode: codeCharset},
		{name: "invalid utf-8", raw: "Content-Type: text/plain; charset=utf-8\r\n\r\n\xff\xfe", wantCode: codeCharset},
		{name: "unknown transfer encoding", raw: "Content-Transfer-Encoding: x-uuencode\r\n\r\nx", wantCode: codeEncoding},
		{name: "bad base64", raw: "Content-Transfer-Encoding: base64\r\n\r\n!!!!", wantCode: codeInvalidFormat},
		{name: "deep nesting", raw: nested(10), wantCode: codeInvalidFormat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseMessage([]byte(tt.raw), ParseOptions{SubjectInBody: tt.subject})
			if tt.wantCode != "" {
				var ce *ContentError
				if !errors.As(err, &ce) || ce.Code != tt.wantCode {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Text != tt.wantText {
				t.Errorf("text = %q, want %q", m.Text, tt.wantText)
			}
			if m.MessageID != tt.wantID {
				t.Errorf("message id = %q, want %q", m.MessageID, tt.wantID)
			}
			if tt.wantSubj != "" && m.Subject != tt.wantSubj {
				t.Errorf("subject = %q, want %q", m.Subject, tt.wantSubj)
			}
		})
	}
}

// nested builds multipart parts nested n deep.
func nested(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("Content-Type: multipart/mixed; boundary=b" + string(rune('a'+i)) + "\r\n\r\n--b" + string(rune('a'+i)) + "\r\n")
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nx\r\n")
	for i := n - 1; i >= 0; i-- {
		b.WriteString("--b" + string(rune('a'+i)) + "--\r\n")
	}
	return b.String()
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		arg, prefix, addr string
		params            int
		ok                bool
	}{
		{"FROM:<a@b>", "FROM:", "a@b", 0, true},
		{"from: <a@b> SIZE=1 BODY=8BITMIME", "FROM:", "a@b", 2, true},
		{"FROM:<>", "FROM:", "", 0, true},
		{"TO:<@relay:x@d>", "TO:", "x@d", 0, true},
		{"TO:x@d", "TO:", "", 0, false},
		{"TO:<x@d", "TO:", "", 0, false},
		{"TO:<a b@d>", "TO:", "", 0, false},
		{"FROM<a@b>", "FROM:", "", 0, false},
		{"", "TO:", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			addr, params, ok := parsePath(tt.arg, tt.prefix)
			if ok != tt.ok || addr != tt.addr || len(params) != tt.params {
				t.Errorf("parsePath(%q) = %q %v %v", tt.arg, addr, params, ok)
			}
		})
	}
}

func TestRecipientIndex(t *testing.T) {
	tests := []struct {
		field string
		i     int
		ok    bool
	}{
		{"/to", 0, true}, {"", 0, true}, {"/messages/2/to", 2, true}, {"/messages/x/to", 0, false}, {"x", 0, false},
	}
	for _, tt := range tests {
		if i, ok := recipientIndex(tt.field); i != tt.i || ok != tt.ok {
			t.Errorf("recipientIndex(%q) = %d %v", tt.field, i, ok)
		}
	}
}

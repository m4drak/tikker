// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
}

func b64(b byte) string { return base64.StdEncoding.EncodeToString(testKey(b)) }

func TestLoadKeyGeneratesAndReloads(t *testing.T) {
	skipWindows(t)
	dir := filepath.Join(t.TempDir(), "data") // does not exist yet
	key, src, err := LoadKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceFileGenerated || len(key) != KeySize {
		t.Fatalf("src = %s, len = %d", src, len(key))
	}
	st, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o", dst.Mode().Perm())
	}
	// No temporary files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}

	again, src, err := LoadKey("", dir)
	if err != nil || src != SourceFile || !bytes.Equal(key, again) {
		t.Fatalf("reload: src=%s err=%v same=%v", src, err, bytes.Equal(key, again))
	}
}

func TestLoadKeyEnvWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(b64(9)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, src, err := LoadKey(" "+b64(1)+"\n", dir)
	if err != nil || src != SourceEnv || !bytes.Equal(key, testKey(1)) {
		t.Fatalf("src=%s err=%v", src, err)
	}
	// With env set, the data dir is not touched at all.
	empty := filepath.Join(t.TempDir(), "never")
	if _, _, err := LoadKey(b64(1), empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data dir created despite env key")
	}
}

func TestLoadKeyErrors(t *testing.T) {
	skipWindows(t)
	write := func(t *testing.T, content string, mode os.FileMode) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, KeyFileName)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name string
		env  string
		dir  func(t *testing.T) string
		want string
	}{
		{"bad env", "not-base64!", func(*testing.T) string { return t.TempDir() }, "TIKKER_SECRET_KEY"},
		{"short env", base64.StdEncoding.EncodeToString([]byte("short")), func(*testing.T) string { return "" }, "32 bytes"},
		{"no source", "", func(*testing.T) string { return "" }, "data directory"},
		{"group readable", "", func(t *testing.T) string { return write(t, b64(1), 0o640) }, "chmod 600"},
		{"world readable", "", func(t *testing.T) string { return write(t, b64(1), 0o644) }, "chmod 600"},
		{"garbage file", "", func(t *testing.T) string { return write(t, "garbage", 0o600) }, "secret.key"},
		{"short file", "", func(t *testing.T) string { return write(t, base64.StdEncoding.EncodeToString([]byte("x")), 0o600) }, "32 bytes"},
		{"directory instead of file", "", func(t *testing.T) string {
			d := t.TempDir()
			if err := os.Mkdir(filepath.Join(d, KeyFileName), 0o700); err != nil {
				t.Fatal(err)
			}
			return d
		}, "regular file"},
		{"unwritable data dir", "", func(t *testing.T) string {
			f := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return f // a file, so MkdirAll / open fail
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := LoadKey(tt.env, tt.dir(t))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadKeyPermissionErrorIsTyped(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	p := filepath.Join(dir, KeyFileName)
	if err := os.WriteFile(p, []byte(b64(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o604); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadKey("", dir); !errors.Is(err, ErrKeyFilePermissions) {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateKeyFileDoesNotOverwrite(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	p := filepath.Join(dir, KeyFileName)
	if err := generateKeyFile(p, bytes.NewReader(testKey(1))); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(p)
	if err := generateKeyFile(p, bytes.NewReader(testKey(2))); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(p)
	if !bytes.Equal(first, second) {
		t.Fatal("existing key file overwritten")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestGenerateKeyFileRandFailure(t *testing.T) {
	dir := t.TempDir()
	if err := generateKeyFile(filepath.Join(dir, KeyFileName), errReader{}); err == nil {
		t.Fatal("expected error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left files: %v", entries)
	}
}

func TestGenerateKeyFileMissingDir(t *testing.T) {
	err := generateKeyFile(filepath.Join(t.TempDir(), "nope", KeyFileName), bytes.NewReader(testKey(1)))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadKeyCreateFailsBelowPlainFile(t *testing.T) {
	skipWindows(t)
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadKey("", filepath.Join(f, "sub")); err == nil {
		t.Fatal("expected error")
	}
}

func TestGenerateKeyFileLinkFallback(t *testing.T) {
	skipWindows(t)
	orig := linkFile
	t.Cleanup(func() { linkFile = orig })
	linkFile = func(string, string) error { return errors.New("links not supported") }

	dir := t.TempDir()
	p := filepath.Join(dir, KeyFileName)
	if err := generateKeyFile(p, bytes.NewReader(testKey(1))); err != nil {
		t.Fatal(err)
	}
	key, err := readKeyFile(p)
	if err != nil || !bytes.Equal(key, testKey(1)) {
		t.Fatalf("key = %v, err = %v", key, err)
	}
	// With the file present the fallback must not replace it.
	if err := generateKeyFile(p, bytes.NewReader(testKey(2))); err != nil {
		t.Fatal(err)
	}
	if key, _ = readKeyFile(p); !bytes.Equal(key, testKey(1)) {
		t.Fatal("existing key replaced")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
}

func TestLoadKeyConcurrentFirstStart(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	const n = 8
	keys := make(chan []byte, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			k, _, err := LoadKey("", dir)
			keys <- k
			errs <- err
		}()
	}
	var first []byte
	for i := 0; i < n; i++ {
		k, err := <-keys, <-errs
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = k
		} else if !bytes.Equal(first, k) {
			t.Fatal("concurrent starters ended up with different keys")
		}
	}
}

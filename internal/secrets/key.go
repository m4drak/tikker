// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/m4drak/tikker/internal/config"
)

// KeyFileName is the name of the generated key file inside the data directory.
const KeyFileName = "secret.key"

// KeySource tells where the active key came from.
type KeySource string

// Key sources.
const (
	SourceEnv           KeySource = "env"            // TIKKER_SECRET_KEY / secret_key
	SourceFile          KeySource = "file"           // existing <data_dir>/secret.key
	SourceFileGenerated KeySource = "file-generated" // created by this call
)

// ErrKeyFilePermissions is returned for a key file that group or others can
// access. Refusing (instead of warning) is deliberate: the key protects every
// router password, and a readable key file is already compromised; a warning
// in a container log is easily missed. The fix is one command, which the
// error message names.
var ErrKeyFilePermissions = errors.New("secrets: key file permissions too open")

// LoadKey returns the 32-byte encryption key. A non-empty envKey (the
// resolved TIKKER_SECRET_KEY, base64) always wins and the file is not touched.
// Otherwise the key is read from <dataDir>/secret.key, which is generated
// atomically on first start (mode 0600; dataDir is created with 0700 when
// missing).
func LoadKey(envKey, dataDir string) ([]byte, KeySource, error) {
	if strings.TrimSpace(envKey) != "" {
		key, err := config.DecodeSecretKey(envKey)
		if err != nil {
			return nil, "", fmt.Errorf("secret key (TIKKER_SECRET_KEY): %w", err)
		}
		return key, SourceEnv, nil
	}
	if dataDir == "" {
		return nil, "", errors.New("secret key: neither TIKKER_SECRET_KEY nor a data directory is set")
	}
	path := filepath.Join(dataDir, KeyFileName)
	key, err := readKeyFile(path)
	switch {
	case err == nil:
		return key, SourceFile, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create data directory: %w", err)
	}
	if err := generateKeyFile(path, rand.Reader); err != nil {
		return nil, "", err
	}
	// Re-read: if a concurrent starter won the race we use its key.
	key, err = readKeyFile(path)
	if err != nil {
		return nil, "", err
	}
	return key, SourceFileGenerated, nil
}

func readKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- path is <data_dir>/secret.key
	if err != nil {
		return nil, fmt.Errorf("open key file: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat key file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("key file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o, want 0600 (run: chmod 600 %s)",
			ErrKeyFilePermissions, path, st.Mode().Perm(), path)
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	key, err := config.DecodeSecretKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return key, nil
}

// generateKeyFile writes a new random key to path without ever exposing a
// partial file and without overwriting an existing one: the content goes to a
// 0600 temporary file in the same directory, is synced, and is then hard-linked
// into place (link fails if path exists). Where hard links are unsupported it
// falls back to rename after an existence check.
func generateKeyFile(path string, rnd io.Reader) error {
	raw := make([]byte, KeySize)
	if _, err := io.ReadFull(rnd, raw); err != nil {
		return fmt.Errorf("generate secret key: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".secret.key.*")
	if err != nil {
		return fmt.Errorf("create temporary key file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary key file: %w", err)
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close key file: %w", err)
	}
	if err := linkFile(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // someone else created it first; the caller re-reads
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			return nil
		}
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("install key file: %w", err)
		}
	}
	syncDir(dir)
	return nil
}

// linkFile is os.Link; tests replace it to exercise the rename fallback.
var linkFile = os.Link

// syncDir makes the new directory entry durable; failures are ignored because
// not every platform supports syncing a directory.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- data directory
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

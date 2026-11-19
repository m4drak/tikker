// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package secrets encrypts the credentials tikker stores for routers
// (SPEC §4.1, §7) with AES-256-GCM.
//
// Ciphertext layout (a BLOB in devices.password_enc):
//
//	version (1 byte, 0x01) | key ID (4 bytes) | nonce (12 bytes) | ciphertext+tag
//
// The key ID is the first four bytes of HMAC-SHA256(key, "tikker/key-id"). It
// lets a keyring pick the right key (rotation after the MVP) and lets startup
// detect "encrypted passwords exist, but the key is new or different" without
// a schema change. The additional authenticated data binds the version, key
// ID, a purpose label and the record ID to the ciphertext, so a ciphertext
// cannot be moved to another device or reused for another purpose.
//
// [Password] is the plaintext type: every formatting path redacts it.
package secrets

// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"errors"
	"fmt"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

const purposeDevicePassword = "device-password"

// DeviceCredentials encrypts and decrypts router passwords. It sits between
// the device repository (which only stores opaque Device.PasswordEnc bytes)
// and the API/UI (write) or connector factory (read).
type DeviceCredentials struct {
	ring *Keyring
}

// NewDeviceCredentials wraps a keyring.
func NewDeviceCredentials(ring *Keyring) *DeviceCredentials {
	return &DeviceCredentials{ring: ring}
}

// Encrypt seals the password for device id. Call it before storing a new
// device or changing its password; pass the result as Device.PasswordEnc.
func (c *DeviceCredentials) Encrypt(id domain.ID, p Password) ([]byte, error) {
	if id.IsZero() {
		return nil, errors.New("encrypt device password: empty device id")
	}
	b, err := c.ring.Seal(purposeDevicePassword, id.String(), []byte(p))
	if err != nil {
		return nil, fmt.Errorf("encrypt device password: %w", err)
	}
	return b, nil
}

// Decrypt returns the plaintext password of d.
func (c *DeviceCredentials) Decrypt(d domain.Device) (Password, error) {
	pt, err := c.ring.Open(purposeDevicePassword, d.ID.String(), d.PasswordEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt password of device %s: %w", d.ID, err)
	}
	return Password(pt), nil
}

// ConnectorConfig builds the connector configuration for d with the decrypted
// password. The result redacts the password when formatted or logged.
func (c *DeviceCredentials) ConnectorConfig(d domain.Device) (connector.DeviceConfig, error) {
	p, err := c.Decrypt(d)
	if err != nil {
		return connector.DeviceConfig{}, err
	}
	return connector.DeviceConfigFromDevice(d, p.Reveal()), nil
}

// Verify checks that every device password decrypts with the current key. Call
// it at startup after loading the devices: it turns a lost, regenerated or
// wrong key into one clear error (errors.Is(err, ErrUnknownKey) when the keys
// differ) instead of failures on first use. It returns the first failure.
func (c *DeviceCredentials) Verify(devices []domain.Device) error {
	for _, d := range devices {
		if _, err := c.Decrypt(d); err != nil {
			return fmt.Errorf("secret key does not match stored data: %w", err)
		}
	}
	return nil
}

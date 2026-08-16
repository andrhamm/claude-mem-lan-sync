package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxDeviceNameLen matches the client's own truncation.
const MaxDeviceNameLen = 80

// Device is a paired device as the hub has seen it.
type Device struct {
	ID        string
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
	Revoked   bool
}

// upsertDevice records that a device is active.
//
// The name is attacker-controlled and display-only: it is truncated, never used
// as a key, and callers must strip control characters before printing it.
func (s *Store) upsertDevice(ctx context.Context, tx *sql.Tx, deviceID, deviceName string, nowMS int64) error {
	if deviceID == "" {
		return nil
	}
	if len(deviceName) > MaxDeviceNameLen {
		deviceName = deviceName[:MaxDeviceNameLen]
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO devices (user_id, device_id, device_name, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, device_id) DO UPDATE SET
			device_name = excluded.device_name,
			last_seen   = excluded.last_seen`,
		s.userID, deviceID, deviceName, nowMS, nowMS)
	if err != nil {
		return fmt.Errorf("store: recording device: %w", err)
	}
	return nil
}

// SeenDevice records a device outside a push, so a device that only ever pulls
// still shows up in `cmemlan devices`.
func (s *Store) SeenDevice(ctx context.Context, deviceID, deviceName string) error {
	if deviceID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx = context.WithoutCancel(ctx)
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.upsertDevice(ctx, tx, deviceID, deviceName, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) deviceRevoked(ctx context.Context, deviceID string) (bool, error) {
	if deviceID == "" {
		return false, nil
	}
	var revokedAt sql.NullInt64
	err := s.r.QueryRowContext(ctx,
		`SELECT revoked_at FROM devices WHERE user_id = ? AND device_id = ?`,
		s.userID, deviceID).Scan(&revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil // unknown devices are not revoked; pairing gates entry
	case err != nil:
		return false, fmt.Errorf("store: checking device revocation: %w", err)
	}
	return revokedAt.Valid, nil
}

// DeviceRevoked reports whether a device has been revoked. The hub checks this
// on every protocol request, so revoking actually denies access rather than
// merely hiding the device from a listing.
func (s *Store) DeviceRevoked(ctx context.Context, deviceID string) (bool, error) {
	return s.deviceRevoked(ctx, deviceID)
}

// ListDevices returns every device the hub has seen.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT device_id, COALESCE(device_name, ''), first_seen, last_seen, revoked_at
		FROM devices WHERE user_id = ?
		ORDER BY last_seen DESC`, s.userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing devices: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Device
	for rows.Next() {
		var d Device
		var first, last int64
		var revoked sql.NullInt64
		if err := rows.Scan(&d.ID, &d.Name, &first, &last, &revoked); err != nil {
			return nil, err
		}
		d.FirstSeen = time.UnixMilli(first)
		d.LastSeen = time.UnixMilli(last)
		d.Revoked = revoked.Valid
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice denies a device further access without touching the log or the
// epoch: revocation is an access-control change, not a replication event.
func (s *Store) RevokeDevice(ctx context.Context, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.w.ExecContext(ctx,
		`UPDATE devices SET revoked_at = ? WHERE user_id = ? AND device_id = ? AND revoked_at IS NULL`,
		s.now().UnixMilli(), s.userID, deviceID)
	if err != nil {
		return fmt.Errorf("store: revoking device: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("store: no active device with id %q", deviceID)
	}
	s.log.Warn("device revoked", "device_id", deviceID)
	return nil
}

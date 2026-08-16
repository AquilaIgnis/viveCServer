package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDeviceNotFound covers both "no such device" and "not this account's device", on purpose:
// distinguishing them would tell one account whether another's device id exists.
var ErrDeviceNotFound = errors.New("no such device")

// lastSeenRefreshInterval is how stale `devices.last_seen_at` is allowed to get before an
// authenticated request refreshes it.
//
// Not updated on every request. Clients poll every 60 seconds (syncPlan.md SD6) and most of those
// polls have nothing to do, so writing a row each time would turn the cheapest request the server
// has into a write — and the plan commits to that poll staying nearly free. Five minutes keeps the
// field useful for "when did this device last check in" while making the write rare.
const lastSeenRefreshInterval = 5 * time.Minute

// Device is one registered client.
type Device struct {
	ID            string
	AccountID     string
	Name          string
	Platform      string
	CreatedAt     time.Time
	LastSeenAt    *time.Time
	LastPulledSeq int64
	RevokedAt     *time.Time
}

// RegisterDevice stores a new device for an account and returns its id.
func RegisterDevice(
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID string,
	deviceName string,
	platform string,
	tokenHash []byte,
) (string, error) {
	const statement = `
		INSERT INTO devices (account_id, name, platform, token_hash)
		VALUES ($1::uuid, $2, $3, $4)
		RETURNING id::text`

	var deviceID string
	err := pool.QueryRow(ctx, statement, accountID, deviceName, platform, tokenHash).Scan(&deviceID)
	if err != nil {
		return "", fmt.Errorf("registering a device: %w", err)
	}
	return deviceID, nil
}

// AuthenticateDeviceToken resolves a token hash to the device and account it belongs to, refreshing
// `last_seen_at` when it has gone stale.
//
// A revoked device is indistinguishable from an unknown one here, which is what makes revocation
// take effect on the very next request with no cache to invalidate.
func AuthenticateDeviceToken(ctx context.Context, pool *pgxpool.Pool, tokenHash []byte) (deviceID string, accountID string, err error) {
	const lookupStatement = `
		SELECT id::text, account_id::text, last_seen_at
		FROM devices
		WHERE token_hash = $1 AND revoked_at IS NULL`

	var lastSeenAt *time.Time
	err = pool.QueryRow(ctx, lookupStatement, tokenHash).Scan(&deviceID, &accountID, &lastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrDeviceNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("authenticating a device: %w", err)
	}

	if lastSeenAt == nil || time.Since(*lastSeenAt) > lastSeenRefreshInterval {
		const touchStatement = `UPDATE devices SET last_seen_at = now() WHERE id = $1::uuid`
		if _, err := pool.Exec(ctx, touchStatement, deviceID); err != nil {
			// A failed bookkeeping write must not fail an otherwise valid request. The caller logs
			// it; the request proceeds.
			return deviceID, accountID, fmt.Errorf("%w: %w", ErrLastSeenNotRecorded, err)
		}
	}

	return deviceID, accountID, nil
}

// ErrLastSeenNotRecorded signals that authentication succeeded but the diagnostic timestamp did
// not update. Callers should log it and carry on.
var ErrLastSeenNotRecorded = errors.New("device authenticated but last_seen_at was not recorded")

// ListDevices returns an account's devices, newest first. Token hashes are never included.
func ListDevices(ctx context.Context, pool *pgxpool.Pool, accountID string) ([]Device, error) {
	const statement = `
		SELECT id::text, account_id::text, name, platform, created_at, last_seen_at, last_pulled_seq, revoked_at
		FROM devices
		WHERE account_id = $1::uuid
		ORDER BY created_at DESC`

	rows, err := pool.Query(ctx, statement, accountID)
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	defer rows.Close()

	devices := make([]Device, 0)
	for rows.Next() {
		var d Device
		if err := rows.Scan(
			&d.ID, &d.AccountID, &d.Name, &d.Platform,
			&d.CreatedAt, &d.LastSeenAt, &d.LastPulledSeq, &d.RevokedAt,
		); err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	return devices, nil
}

// RevokeDevice marks a device unusable. It is scoped to the account so one account can never revoke
// another's device, and re-revoking an already revoked device is not an error — the caller asked for
// a state, and the state holds.
func RevokeDevice(ctx context.Context, pool *pgxpool.Pool, accountID string, deviceID string) error {
	if !isUUID(deviceID) {
		// Passing this to PostgreSQL would raise a cast error rather than answer the question.
		return ErrDeviceNotFound
	}

	const statement = `
		UPDATE devices
		SET revoked_at = coalesce(revoked_at, now())
		WHERE id = $1::uuid AND account_id = $2::uuid`

	result, err := pool.Exec(ctx, statement, deviceID, accountID)
	if err != nil {
		return fmt.Errorf("revoking a device: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// isUUID reports whether a string is a well-formed UUID, using the parser pgx already carries so
// that this agrees exactly with what the database would accept.
func isUUID(candidate string) bool {
	var parsed pgtype.UUID
	return parsed.Scan(candidate) == nil
}

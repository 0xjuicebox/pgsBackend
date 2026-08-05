package registration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTokenInvalid covers "doesn't exist", "expired", and "already used" —
// callers don't need to distinguish; the user experience is the same either
// way (send them back to WhatsApp for a fresh link).
var ErrTokenInvalid = errors.New("registration token is invalid, expired, or already used")

const tokenTTL = 30 * time.Minute

// GenerateToken creates a fresh single-use token tied to a phone number and
// stores it. Call this right before sending the registration link.
func GenerateToken(ctx context.Context, db *pgxpool.Pool, phoneNumber string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)

	_, err := db.Exec(ctx, `
		INSERT INTO registration_tokens (token, phone_number, expires_at)
		VALUES ($1, $2, $3)
	`, token, phoneNumber, time.Now().Add(tokenTTL))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ValidatePhone checks a token is valid and NOT yet used, returning the phone
// number it's tied to. Does not consume it — safe to call on every page load
// (e.g. GET /register) without burning the token.
func ValidatePhone(ctx context.Context, db *pgxpool.Pool, token string) (string, error) {
	var phone string
	var expiresAt time.Time
	var usedAt *time.Time

	err := db.QueryRow(ctx, `
		SELECT phone_number, expires_at, used_at FROM registration_tokens WHERE token = $1
	`, token).Scan(&phone, &expiresAt, &usedAt)

	if err != nil {
		if err == pgx.ErrNoRows {
			return "", ErrTokenInvalid
		}
		return "", err
	}

	if usedAt != nil || time.Now().After(expiresAt) {
		return "", ErrTokenInvalid
	}

	return phone, nil
}

// MarkUsed consumes a token so it can't be replayed. Call this only after a
// successful registration submission.
func MarkUsed(ctx context.Context, db *pgxpool.Pool, token string) error {
	_, err := db.Exec(ctx, `UPDATE registration_tokens SET used_at = NOW() WHERE token = $1`, token)
	return err
}

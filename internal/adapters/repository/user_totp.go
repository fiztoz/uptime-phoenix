package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TOTP credential state (migration 090) is written only by the statements in
// this file, each a single conditional UPDATE on the users row, so the
// database itself serializes competing requests on every engine and replica.
// UserRepo.Update on both engines excludes these columns.

// userTOTPColumns are the users columns UserRepo.Update must never write.
var userTOTPColumns = []string{"totp_secret", "totp_enabled", "totp_generation"}

// UserUpdateExcludedColumns returns the columns a generic user update skips.
func UserUpdateExcludedColumns() []string {
	return append([]string(nil), userTOTPColumns...)
}

// ReplaceUserTOTPCredential stores secret ("" clears it) with 2FA disabled,
// increments the credential generation and resets the consumed step.
func ReplaceUserTOTPCredential(ctx context.Context, db bun.IDB, userID int64, secret string) error {
	var stored any = secret
	if secret == "" {
		stored = nil
	}
	res, err := db.NewRaw(
		`UPDATE users SET totp_secret = ?, totp_enabled = 0, totp_generation = totp_generation + 1,
		 totp_last_step = 0, updated_at = ? WHERE id = ?`,
		stored, time.Now().UTC(), userID,
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("replace totp credential: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("replace totp credential: rows affected: %w", err)
	} else if n != 1 {
		return ports.ErrNotFound
	}
	return nil
}

// ConsumeUserTOTPStep implements ports.UserRepository.ConsumeTOTPStep.
func ConsumeUserTOTPStep(ctx context.Context, db bun.IDB, userID, generation, step int64) (bool, error) {
	return consumeUserTOTPStep(ctx, db, userID, generation, step, true)
}

// ConfirmUserTOTPCredential implements
// ports.UserRepository.ConfirmTOTPCredential.
func ConfirmUserTOTPCredential(ctx context.Context, db bun.IDB, userID, generation, step int64) (bool, error) {
	return consumeUserTOTPStep(ctx, db, userID, generation, step, false)
}

// consumeUserTOTPStep advances totp_last_step to step only if the row still
// holds the expected credential in the expected enabled state and step is
// strictly later than every consumed step. When confirming setup
// (enabled=false) the same statement turns 2FA on. RowsAffected is 1 for the
// single winner: the step always increases, so a matched row always changes.
func consumeUserTOTPStep(ctx context.Context, db bun.IDB, userID, generation, step int64, enabled bool) (bool, error) {
	if step <= 0 {
		return false, nil
	}
	want := 0
	if enabled {
		want = 1
	}
	res, err := db.NewRaw(
		`UPDATE users SET totp_last_step = ?, totp_enabled = 1
		 WHERE id = ? AND active = 1 AND totp_enabled = ? AND totp_generation = ?
		   AND totp_secret IS NOT NULL AND totp_secret <> '' AND totp_last_step < ?`,
		step, userID, want, generation, step,
	).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("consume totp step: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("consume totp step: rows affected: %w", err)
	}
	return n == 1, nil
}

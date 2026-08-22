package route

// Manifest locking — freezing each round's plan at its order cutoff.
//
// WHY LOCK AT ALL
//
// The manifest used to be computed live on every request, which meant three
// things were true at once: no record existed that a driver was ever given a
// list, past manifests were reconstructed from present-day subscriptions and
// were therefore wrong, and the list could change under a driver mid-round
// after the van was already loaded.
//
// Locking makes the plan a fact rather than a derivation.
//
// WHY AT THE ORDER CUTOFF
//
// The cutoff is already the moment the business stops accepting changes for
// that slot — morning at 02:00, evening at 13:00 by default. Locking at any
// other time would invent a second deadline nobody had agreed to. Locking at
// the cutoff means the frozen list is exactly what the customer was told they
// could still change up until.
//
// WHY ROWS RATHER THAN A SEPARATE MANIFEST TABLE
//
// A locked stop is written straight into delivery_logs as PENDING with its
// planned quantities. That reuses the table billing already reads, means the
// driver updates rows rather than creating them, and makes "the driver never
// opened the app" visible as a route full of untouched rows instead of an
// absence of data.
//
// WHY LOCKED ROWS ARE 'PENDING', NOT 'UNATTEMPTED'
//
// The obvious choice was UNATTEMPTED, since that is what CloseRoute writes for
// a stop nobody reached. It would have broken the driver app completely.
//
// The driver screen computes `isDone = status !== 'PENDING'`. Locking every
// stop as UNATTEMPTED would therefore have rendered the entire round as
// already finished — greyed out, struck through, with no deliver button
// anywhere. A driver would have opened the app to a completed list and been
// unable to record a single delivery.
//
// So the two states are kept distinct, which is also more honest about what
// they mean:
//
//	PENDING     the plan is frozen and this stop is waiting for the driver
//	UNATTEMPTED the round is over and this stop was never reached
//
// CloseRoute converts the first into the second when the round ends.
//
// SCHEDULE-PREDICATE-COPY — the due-today logic below is the fifth
// implementation of this predicate (GenerateManifest, driver.CloseRoute,
// stats.dueTodayCTE, schedule.NextDelivery, and this). That is too many, and
// extracting one shared version is the right eventual fix. This copy is
// written to match driver.CloseRoute exactly, because the two must agree:
// CloseRoute fills gaps at the end of a round that this leaves at the start.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// products in the order they appear on a bill, so a locked plan and an
// invoice read the same way.
var lockProducts = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
}

// StartManifestLockSweeper freezes each route+slot manifest once its cutoff
// passes.
//
//	go route.StartManifestLockSweeper(pool)
//
// Ten-minute ticker. The cutoff is a business deadline rather than a
// settlement, so being locked within ten minutes of it is fine — and a
// shorter interval would mean more no-op passes for no benefit.
func StartManifestLockSweeper(db *pgxpool.Pool) {
	lockManifestsDue(db)

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		lockManifestsDue(db)
	}
}

func lockManifestsDue(db *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		fmt.Printf("⚠️ manifest lock: cannot load Asia/Kolkata: %v\n", err)
		return
	}
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	var morningCutoff, eveningCutoff string
	if err := db.QueryRow(ctx,
		`SELECT morning_cutoff_time::text, evening_cutoff_time::text FROM system_config LIMIT 1`,
	).Scan(&morningCutoff, &eveningCutoff); err != nil {
		fmt.Printf("⚠️ manifest lock: no system_config row: %v\n", err)
		return
	}

	for _, s := range []struct {
		slot   string
		cutoff string
	}{
		{"morning", morningCutoff},
		{"evening", eveningCutoff},
	} {
		if !cutoffPassed(now, s.cutoff, loc) {
			continue
		}

		rows, err := db.Query(ctx, `
			SELECT r.id::text
			FROM routes r
			WHERE NOT EXISTS (
				SELECT 1 FROM manifest_locks ml
				WHERE ml.route_id = r.id AND ml.slot = $1 AND ml.manifest_date = $2::date
			)
		`, s.slot, today)
		if err != nil {
			fmt.Printf("⚠️ manifest lock: listing routes failed: %v\n", err)
			continue
		}

		var routeIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				routeIDs = append(routeIDs, id)
			}
		}
		rows.Close()

		for _, routeID := range routeIDs {
			n, err := LockManifest(ctx, db, routeID, s.slot, today)
			if err != nil {
				fmt.Printf("⚠️ manifest lock: route %s %s failed: %v\n", routeID, s.slot, err)
				continue
			}
			fmt.Printf("🔒 locked %s manifest for route %s — %d stop(s)\n", s.slot, routeID, n)
		}
	}
}

// cutoffPassed reports whether the wall clock has gone past "HH:MM[:SS]" today.
func cutoffPassed(now time.Time, cutoff string, loc *time.Location) bool {
	layouts := []string{"15:04:05", "15:04"}
	for _, l := range layouts {
		if t, err := time.Parse(l, cutoff); err == nil {
			deadline := time.Date(now.Year(), now.Month(), now.Day(),
				t.Hour(), t.Minute(), t.Second(), 0, loc)
			return now.After(deadline)
		}
	}
	// An unparseable cutoff must not freeze the manifest early — better to
	// leave the round unlocked and behave as before than to lock the wrong
	// list.
	fmt.Printf("⚠️ manifest lock: unparseable cutoff %q\n", cutoff)
	return false
}

// LockManifest freezes one route+slot manifest for a date and returns how many
// stops it wrote.
//
// Exported so an admin can lock a round early — useful when the day's
// production is already decided and nobody wants a late change — and so the
// behaviour is testable without waiting for a cutoff.
//
// Idempotent: the manifest_locks row is claimed first, and the INSERT skips
// any customer who already has a log for that date and slot.
func LockManifest(ctx context.Context, db *pgxpool.Pool, routeID, slot, date string) (int, error) {
	lockID, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Claim first. Two sweeper passes overlapping — or a manual lock racing
	// the ticker — must not double-write the round.
	tag, err := tx.Exec(ctx, `
		INSERT INTO manifest_locks (id, route_id, slot, manifest_date)
		VALUES ($1, $2::uuid, $3, $4::date)
		ON CONFLICT (route_id, slot, manifest_date) DO NOTHING
	`, lockID, routeID, slot, date)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, nil // already locked
	}

	// Build the planned_order JSON from the effective order: the override for
	// this date if one exists, otherwise the standing subscription.
	//
	// COALESCE per product rather than per row, matching how the manifest and
	// CloseRoute both resolve an override — an override that sets only milk
	// leaves every other product at its subscription default.
	//
	// EVERY product is stored, including zeros. Deliberately not jsonb_strip_nulls.
	//
	// Stripping zeros looks tidier and breaks the lock. GenerateManifest reads
	// COALESCE(planned_order->>'curd', override, subscription_default, 0) — so
	// a missing key falls through to the subscription. A customer who
	// cancelled their curd with a zero-quantity override would have it
	// reappear on the locked manifest, because "cancelled" and "not mentioned"
	// became the same thing in the stored plan.
	//
	// Zero is how a customer cancels one item. It has to be recorded, not
	// omitted.
	planExpr := "jsonb_build_object("
	for i, p := range lockProducts {
		if i > 0 {
			planExpr += ", "
		}
		planExpr += fmt.Sprintf("'%s', COALESCE(o.new_%s_qty, s.default_%s_qty, 0)", p, p, p)
	}
	planExpr += ")"

	totalExpr := "("
	for i, p := range lockProducts {
		if i > 0 {
			totalExpr += " + "
		}
		totalExpr += fmt.Sprintf("COALESCE(o.new_%s_qty, s.default_%s_qty, 0)", p, p)
	}
	totalExpr += ")"

	query := `
		INSERT INTO delivery_logs (customer_id, route_id, driver_id, delivery_date, slot,
		                           status, planned_order, locked_at)
		SELECT s.customer_id, s.route_id, rsd.driver_id, $2::date, $3,
		       'PENDING', ` + planExpr + `, NOW()
		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		LEFT JOIN route_slot_drivers rsd ON rsd.route_id = s.route_id AND rsd.slot = s.slot
		LEFT JOIN order_overrides o
		       ON o.customer_id = c.id AND o.target_date = $2::date AND o.slot = $3
		LEFT JOIN delivery_logs dl
		       ON dl.customer_id = c.id AND dl.delivery_date = $2::date AND dl.slot = $3
		WHERE s.route_id = $1::uuid AND s.slot = $3
		  AND c.is_active = TRUE
		  AND s.stop_order > 0
		  AND dl.id IS NULL
		  AND (
			  s.schedule_type = 'daily'
			  OR (s.schedule_type = 'custom' AND EXTRACT(DOW FROM $2::date)::int = ANY(s.active_days))
			  OR (s.schedule_type = 'alternate' AND ($2::date - s.anchor_date) % 2 = 0)
			  OR o.id IS NOT NULL
		  )
		  AND ` + totalExpr + ` > 0
	`

	res, err := tx.Exec(ctx, query, routeID, date, slot)
	if err != nil {
		return 0, err
	}
	count := int(res.RowsAffected())

	if _, err := tx.Exec(ctx,
		`UPDATE manifest_locks SET stop_count = $1 WHERE id = $2`, count, lockID); err != nil {
		return 0, err
	}

	return count, tx.Commit(ctx)
}

// PlannedOrder decodes a locked plan for display.
func PlannedOrder(raw []byte) map[string]int {
	out := map[string]int{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fmt.Printf("⚠️ manifest lock: bad planned_order: %v\n", err)
	}
	return out
}

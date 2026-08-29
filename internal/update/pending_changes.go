package update

// Admin side of staged customer changes, plus the sweeper that applies them.
//
// Write side lives in update.go (Submit). This file covers everything after
// the customer hits save:
//
//   ListPendingChanges  — the admin review queue
//   ApprovePendingChange — schedules it for tomorrow, optionally re-routing
//   RejectPendingChange  — discards it; customer keeps their current order
//   StartChangeSweeper   — applies APPROVED changes once their date arrives
//
// The customer's live subscription is never touched until the sweeper runs.
// That's the whole point: production is planned a day ahead, so nothing
// submitted today can alter what's already been milked for today, and a
// rejection costs the customer nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/schedule"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// -------------------------------------------------------------------------
// Types
// -------------------------------------------------------------------------

// SlotAssignment mirrors customer.SlotAssignment. Duplicated rather than
// imported because update already imports customer for Order, and pulling the
// approval types across would tangle the two packages further for one struct.
type SlotAssignment struct {
	Slot      string    `json:"slot"`
	RouteId   uuid.UUID `json:"routeId"`
	StopOrder int       `json:"stopOrder"`
}

// PendingChange is one row of the admin review queue, enriched with what the
// customer currently has so the screen can show a before/after without a
// second round trip.
type PendingChange struct {
	Id             uuid.UUID `json:"id"`
	CustomerId     uuid.UUID `json:"customerId"`
	PhoneNumber    string    `json:"phoneNumber"`
	AddressChanged bool      `json:"addressChanged"`
	SubmittedAt    time.Time `json:"submittedAt"`
	ReviewStatus   string    `json:"reviewStatus"`
	EffectiveFrom  *string   `json:"effectiveFrom"`

	// Proposed
	Name          string             `json:"name"`
	Address       string             `json:"address"`
	Latitude      string             `json:"latitude"`
	Longitude     string             `json:"longitude"`
	Subscriptions []SlotSubscription `json:"subscriptions"`

	// Current, for comparison
	CurrentName          string             `json:"currentName"`
	CurrentAddress       string             `json:"currentAddress"`
	CurrentSubscriptions []SlotSubscription `json:"currentSubscriptions"`
}

// -------------------------------------------------------------------------
// Routes — add to UpdateResource.Routes()
//
//     r.Get("/changes", ur.ListPendingChanges)
//     r.Post("/changes/{id}/approve", ur.ApprovePendingChange)
//     r.Post("/changes/{id}/reject", ur.RejectPendingChange)
// -------------------------------------------------------------------------

// ListPendingChanges returns open requests, newest first. Includes the
// customer's current subscriptions so the admin can see exactly what's
// changing rather than just what's proposed.
func (ur UpdateResource) ListPendingChanges(w http.ResponseWriter, r *http.Request) {
	rows, err := ur.DB.Query(r.Context(), `
		SELECT p.id, p.customer_id, c.phone_number, p.address_changed, p.submitted_at,
		       p.review_status, p.effective_from,
		       p.name, p.house_address, COALESCE(p.geo_latitude, ''), COALESCE(p.geo_longitude, ''),
		       p.subscriptions,
		       c.name, c.house_address
		FROM pending_subscription_changes p
		JOIN customers c ON c.id = p.customer_id
		WHERE p.review_status = 'PENDING'
		ORDER BY p.submitted_at DESC
		LIMIT 200
	`)
	if err != nil {
		http.Error(w, "Failed to load change requests: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []PendingChange{}
	for rows.Next() {
		var pc PendingChange
		var raw []byte
		var eff *time.Time
		if err := rows.Scan(
			&pc.Id, &pc.CustomerId, &pc.PhoneNumber, &pc.AddressChanged, &pc.SubmittedAt,
			&pc.ReviewStatus, &eff,
			&pc.Name, &pc.Address, &pc.Latitude, &pc.Longitude, &raw,
			&pc.CurrentName, &pc.CurrentAddress,
		); err != nil {
			continue
		}
		_ = json.Unmarshal(raw, &pc.Subscriptions)
		if eff != nil {
			s := eff.Format("2006-01-02")
			pc.EffectiveFrom = &s
		}
		out = append(out, pc)
	}

	// Second pass for current subscriptions. N+1, but this queue is small by
	// nature — it's a human review list, not a report.
	for i := range out {
		out[i].CurrentSubscriptions = ur.loadCurrentSubscriptions(r.Context(), out[i].CustomerId.String())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (ur UpdateResource) loadCurrentSubscriptions(ctx context.Context, customerID string) []SlotSubscription {
	rows, err := ur.DB.Query(ctx, `
		SELECT slot, schedule_type, active_days, TO_CHAR(anchor_date, 'YYYY-MM-DD'),
		       default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
		       default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
		       default_oil_qty, default_atta_qty, default_burfi_qty
		FROM subscriptions WHERE customer_id = $1`, customerID)
	if err != nil {
		return []SlotSubscription{}
	}
	defer rows.Close()

	out := []SlotSubscription{}
	for rows.Next() {
		var s SlotSubscription
		if err := rows.Scan(
			&s.Slot, &s.ScheduleType, &s.ActiveDays, &s.StartDate,
			&s.Items.Milk, &s.Items.Curd, &s.Items.Butter, &s.Items.Ghee,
			&s.Items.Lassi, &s.Items.Paneer, &s.Items.Jaggery, &s.Items.Khand,
			&s.Items.Oil, &s.Items.Atta, &s.Items.Burfi,
		); err != nil {
			continue
		}
		s.Subscribed = true
		out = append(out, s)
	}
	return out
}

type approveChangeRequest struct {
	// Optional. Supply when the address moved and the customer needs a
	// different route or stop position. Omitted means keep existing routing.
	Assignments []SlotAssignment `json:"assignments"`
}

// ApprovePendingChange schedules the change for tomorrow.
//
// Nothing is written to subscriptions here — the sweeper does that once
// effective_from arrives. Approving at 2 PM must not alter this evening's
// manifest, which was planned against the order the customer had this morning.
func (ur UpdateResource) ApprovePendingChange(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req approveChangeRequest
	// An empty body is legitimate — "approve as-is, keep current routing".
	_ = json.NewDecoder(r.Body).Decode(&req)

	for _, a := range req.Assignments {
		if a.Slot != "morning" && a.Slot != "evening" {
			http.Error(w, "Invalid slot: "+a.Slot, http.StatusBadRequest)
			return
		}
	}

	var assignmentsJSON []byte
	if len(req.Assignments) > 0 {
		assignmentsJSON, _ = json.Marshal(req.Assignments)
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	tomorrow := time.Now().In(loc).AddDate(0, 0, 1).Format("2006-01-02")

	var customerID uuid.UUID
	var phone, name string
	err := ur.DB.QueryRow(r.Context(), `
		UPDATE pending_subscription_changes p
		SET review_status = 'APPROVED',
		    assignments = $2,
		    effective_from = $3::date,
		    reviewed_at = NOW()
		FROM customers c
		WHERE p.id = $1 AND p.review_status = 'PENDING' AND c.id = p.customer_id
		RETURNING p.customer_id, c.phone_number, c.name
	`, id, assignmentsJSON, tomorrow).Scan(&customerID, &phone, &name)

	if err == pgx.ErrNoRows {
		http.Error(w, "Change request not found or already reviewed", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if ur.WhatsApp != nil {
		// Fired in the background so a slow Twilio call doesn't hold up the
		// admin's response. Reads the applied order from the staged payload
		// rather than from subscriptions, because the change hasn't been
		// applied yet — the sweeper does that tomorrow.
		go ur.notifyChangeApproved(customerID.String(), phone, name, tomorrow)
	}

	writeJSONUpdate(w, http.StatusOK, map[string]string{
		"message":       "Change approved. It will apply from " + tomorrow + ".",
		"effectiveFrom": tomorrow,
	})
}

type rejectChangeRequest struct {
	Reason string `json:"reason"`
}

// RejectPendingChange discards the request. The customer's existing order and
// active status are untouched — they keep receiving deliveries exactly as
// before, which is the entire reason this table exists.
func (ur UpdateResource) RejectPendingChange(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req rejectChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, "A reason is required — the customer is told why", http.StatusBadRequest)
		return
	}

	var phone, name string
	err := ur.DB.QueryRow(r.Context(), `
		UPDATE pending_subscription_changes p
		SET review_status = 'REJECTED', review_reason = $2, reviewed_at = NOW()
		FROM customers c
		WHERE p.id = $1 AND p.review_status = 'PENDING' AND c.id = p.customer_id
		RETURNING c.phone_number, c.name
	`, id, req.Reason).Scan(&phone, &name)

	if err == pgx.ErrNoRows {
		http.Error(w, "Change request not found or already reviewed", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if ur.WhatsApp != nil {
		// Leads with the reassurance, not the refusal — the thing the customer
		// most needs to know is that their milk isn't stopping.
		msg := fmt.Sprintf(
			"Hello %s, we weren't able to apply your requested changes.\n\n"+
				"*Reason:* %s\n\n"+
				"*Your current deliveries continue as normal* — nothing has changed on your account.\n\n"+
				"If you'd like to discuss this or stop your deliveries instead, please reply here and our team will help.",
			name, req.Reason,
		)
		go ur.notifyChangeDeclined(phone, name, req.Reason, msg)
	}

	writeJSONUpdate(w, http.StatusOK, map[string]string{
		"message": "Change rejected. The customer keeps their existing order and stays active.",
	})
}

// -------------------------------------------------------------------------
// Sweeper
// -------------------------------------------------------------------------

// StartChangeSweeper applies approved changes once their effective date
// arrives. Hourly, plus once at boot.
//
// Hourly rather than a midnight cron for the same reason as the price
// sweeper: a timer anchored to process start drifts with every restart, and
// `effective_from <= CURRENT_DATE` means a server that was down overnight
// catches up as soon as it returns.
//
// Start from main.go:
//
//	go update.StartChangeSweeper(pool)
func StartChangeSweeper(db *pgxpool.Pool) {
	applyDueChanges(db)

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		applyDueChanges(db)
	}
}

func applyDueChanges(db *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rows, err := db.Query(ctx, `
		SELECT id, customer_id, name, house_address,
		       COALESCE(geo_latitude, ''), COALESCE(geo_longitude, ''),
		       subscriptions, assignments
		FROM pending_subscription_changes
		WHERE review_status = 'APPROVED' AND effective_from <= CURRENT_DATE
	`)
	if err != nil {
		fmt.Printf("⚠️ applyDueChanges: %v\n", err)
		return
	}

	type due struct {
		id, customerID          uuid.UUID
		name, address, lat, lng string
		subsRaw, assignRaw      []byte
	}
	var batch []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.customerID, &d.name, &d.address, &d.lat, &d.lng, &d.subsRaw, &d.assignRaw); err != nil {
			continue
		}
		batch = append(batch, d)
	}
	rows.Close()

	for _, d := range batch {
		if err := applyOneChange(ctx, db, d.id, d.customerID.String(), d.name, d.address, d.lat, d.lng, d.subsRaw, d.assignRaw); err != nil {
			// Log and continue — one malformed change must not block the rest.
			fmt.Printf("⚠️ applyDueChanges: change %s failed: %v\n", d.id, err)
			continue
		}
		fmt.Printf("✅ applied staged change %s for customer %s\n", d.id, d.customerID)
	}
}

func applyOneChange(
	ctx context.Context, db *pgxpool.Pool,
	changeID uuid.UUID, customerID, name, address, lat, lng string,
	subsRaw, assignRaw []byte,
) error {
	var subs []SlotSubscription
	if err := json.Unmarshal(subsRaw, &subs); err != nil {
		return fmt.Errorf("unmarshal subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return fmt.Errorf("no subscriptions in change")
	}

	var assignments []SlotAssignment
	if len(assignRaw) > 0 {
		_ = json.Unmarshal(assignRaw, &assignments)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Identity. Status and is_active are deliberately untouched — the
	// customer was never suspended, and approving their change shouldn't
	// suspend them either.
	if _, err := tx.Exec(ctx, `
		UPDATE customers
		SET name = $1, house_address = $2, geo_latitude = $3, geo_longitude = $4
		WHERE id = $5
	`, name, address, lat, lng, customerID); err != nil {
		return fmt.Errorf("update customer: %w", err)
	}

	// Subscriptions. route_id and stop_order are absent from both the insert
	// column list and the conflict update, so an existing customer keeps
	// their routing through a quantity change. A newly added slot lands with
	// route_id NULL and is picked up by the assignments loop below, or by a
	// later admin approval.
	subQuery := `
		INSERT INTO subscriptions (
			id, customer_id, slot, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty, item_schedules
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (customer_id, slot) DO UPDATE SET
			schedule_type = EXCLUDED.schedule_type, active_days = EXCLUDED.active_days,
			anchor_date = EXCLUDED.anchor_date,
			default_milk_qty = EXCLUDED.default_milk_qty, default_curd_qty = EXCLUDED.default_curd_qty,
			default_butter_qty = EXCLUDED.default_butter_qty, default_ghee_qty = EXCLUDED.default_ghee_qty,
			default_lassi_qty = EXCLUDED.default_lassi_qty, default_paneer_qty = EXCLUDED.default_paneer_qty,
			default_jaggery_qty = EXCLUDED.default_jaggery_qty, default_khand_qty = EXCLUDED.default_khand_qty,
			default_oil_qty = EXCLUDED.default_oil_qty, default_atta_qty = EXCLUDED.default_atta_qty,
			default_burfi_qty = EXCLUDED.default_burfi_qty,
			item_schedules = EXCLUDED.item_schedules,
			updated_at = NOW()`

	submitted := map[string]bool{}
	for _, s := range subs {
		if s.Slot != "evening" {
			s.Slot = "morning"
		}
		submitted[s.Slot] = true

		activeDays := s.ActiveDays
		if s.ScheduleType != "custom" {
			activeDays = []int{0, 1, 2, 3, 4, 5, 6}
		}
		anchor := time.Now().Format("2006-01-02")
		if s.ScheduleType == "alternate" && s.StartDate != "" {
			anchor = s.StartDate
		}

		// Per-item schedules were validated when the change was submitted, so
		// a failure here means the staged JSON was altered between submission
		// and application. Storing NULL is the safe outcome: every item falls
		// back to the slot schedule, which is what the customer had before.
		// Refusing the whole change instead would strand an approved request
		// that retries hourly forever and is invisible in the review queue.
		encoded, schErr := schedule.ValidateItemSchedules(toScheduleMap(s.ItemSchedules), anchor)
		if schErr != nil {
			fmt.Printf("⚠️ applyOneChange: bad item schedules for %s %s, storing none: %v\n",
				customerID, s.Slot, schErr)
			encoded = nil
		}

		subID, _ := uuid.NewV7()
		if _, err := tx.Exec(ctx, subQuery,
			subID, customerID, s.Slot, s.ScheduleType, activeDays, anchor,
			s.Items.Milk, s.Items.Curd, s.Items.Butter, s.Items.Ghee,
			s.Items.Lassi, s.Items.Paneer, s.Items.Jaggery, s.Items.Khand,
			s.Items.Oil, s.Items.Atta, s.Items.Burfi, encoded,
		); err != nil {
			return fmt.Errorf("upsert subscription %s: %w", s.Slot, err)
		}
	}

	// Slots the customer dropped. Future overrides go with them — leaving
	// those would resurrect deliveries for a slot with no standing order.
	// Past delivery_logs are untouched: that's billing history.
	for _, slot := range []string{"morning", "evening"} {
		if submitted[slot] {
			continue
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM order_overrides WHERE customer_id = $1 AND slot = $2 AND target_date >= CURRENT_DATE`,
			customerID, slot); err != nil {
			return fmt.Errorf("clear overrides %s: %w", slot, err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM subscriptions WHERE customer_id = $1 AND slot = $2`,
			customerID, slot); err != nil {
			return fmt.Errorf("delete subscription %s: %w", slot, err)
		}
	}

	// Re-routing supplied at approval time.
	for _, a := range assignments {
		if _, err := tx.Exec(ctx, `
			UPDATE subscriptions SET route_id = $1, stop_order = $2, updated_at = NOW()
			WHERE customer_id = $3 AND slot = $4
		`, a.RouteId, a.StopOrder, customerID, a.Slot); err != nil {
			return fmt.Errorf("assign route %s: %w", a.Slot, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE pending_subscription_changes
		SET review_status = 'APPLIED', applied_at = NOW()
		WHERE id = $1
	`, changeID); err != nil {
		return fmt.Errorf("mark applied: %w", err)
	}

	return tx.Commit(ctx)
}

// writeJSONUpdate is local to the update package to avoid colliding with the
// writeJSON helpers in customer/delivery/billing.
func writeJSONUpdate(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// -------------------------------------------------------------------------
// Change review notifications
// -------------------------------------------------------------------------

// notifyChangeApproved tells the customer their new order is confirmed, which
// slot it applies to, when it starts, and what it actually is.
//
// The echo matters. If an admin approves 1.5 L when the customer asked for
// 1 L, this message is the only place the customer could notice before the
// difference turns up on a bill a month later.
func (ur UpdateResource) notifyChangeApproved(customerID, phone, name, effectiveFrom string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	slotLabel, order := ur.stagedSummary(ctx, customerID)
	startLabel := prettyDate(effectiveFrom)

	if ur.Templates.ChangeApproved != "" {
		if err := ur.WhatsApp.SendChangeApproved(
			phone, ur.Templates.ChangeApproved, name, slotLabel, startLabel, order,
		); err == nil {
			return
		} else {
			fmt.Printf("⚠️ change-approved template failed for %s, falling back: %v\n", phone, err)
		}
	}

	ur.WhatsApp.SendDeliveryUpdate(phone, fmt.Sprintf(
		"✅ Hello %s, your requested changes have been approved.\n\n"+
			"They take effect from *%s*. Today's delivery goes ahead on your existing order.",
		name, startLabel,
	))
}

func (ur UpdateResource) notifyChangeDeclined(phone, name, reason, fallbackMsg string) {
	if ur.Templates.ChangeDeclined != "" {
		if err := ur.WhatsApp.SendChangeDeclined(
			phone, ur.Templates.ChangeDeclined, name, reason,
		); err == nil {
			return
		} else {
			fmt.Printf("⚠️ change-declined template failed for %s, falling back: %v\n", phone, err)
		}
	}
	ur.WhatsApp.SendDeliveryUpdate(phone, fallbackMsg)
}

// stagedSummary reads the approved-but-not-yet-applied change and renders the
// slot and the new order for the confirmation message.
//
// Reads pending_subscription_changes, not subscriptions: at this point the
// change has been approved but the sweeper hasn't applied it, so the live
// subscription still holds the OLD order. Reading that would confirm back to
// the customer exactly what they asked to change away from.
func (ur UpdateResource) stagedSummary(ctx context.Context, customerID string) (slotLabel, order string) {
	var raw []byte
	err := ur.DB.QueryRow(ctx, `
		SELECT subscriptions FROM pending_subscription_changes
		WHERE customer_id = $1 AND review_status = 'APPROVED'
		ORDER BY reviewed_at DESC LIMIT 1
	`, customerID).Scan(&raw)
	if err != nil {
		fmt.Printf("⚠️ stagedSummary: %v\n", err)
		return "Your", "your new order"
	}

	var subs []struct {
		Slot  string         `json:"slot"`
		Items map[string]int `json:"items"`
	}
	if err := json.Unmarshal(raw, &subs); err != nil || len(subs) == 0 {
		return "Your", "your new order"
	}

	// Slot label: name it when there's exactly one, since the message reads
	// "your Morning order change is approved".
	slotLabel = "Your"
	if len(subs) == 1 {
		slotLabel = capitalise(subs[0].Slot)
	}

	// Items arrive keyed as milkQuantity/curdQuantity — the /update payload
	// shape, which differs from /register's {milk: 2000}. Both are handled at
	// the edges elsewhere; this is the third place that has to know.
	labels := []struct {
		key   string
		label string
		litre bool
	}{
		{"milkQuantity", "Milk", true},
		{"curdQuantity", "Curd", true},
		{"lassiQuantity", "Buttermilk", true},
		{"oilQuantity", "Mustard Oil", true},
		{"butterQuantity", "Butter", false},
		{"gheeQuantity", "Ghee", false},
		{"paneerQuantity", "Paneer", false},
		{"jaggeryQuantity", "Jaggery", false},
		{"khandQuantity", "Desi Khand", false},
		{"attaQuantity", "Atta", false},
		{"burfiQuantity", "Burfi", false},
	}

	totals := map[string]int{}
	for _, sub := range subs {
		for k, v := range sub.Items {
			totals[k] += v
		}
	}

	var parts []string
	for _, l := range labels {
		if totals[l.key] <= 0 {
			continue
		}
		parts = append(parts, l.label+" "+formatBaseUnit(totals[l.key], l.litre))
	}
	if len(parts) == 0 {
		return slotLabel, "no items — deliveries will stop"
	}
	return slotLabel, strings.Join(parts, ", ")
}

// capitalise upper-cases the first letter. strings.Title is deprecated and
// does more than wanted here — slot values are always a single lowercase word.
func capitalise(v string) string {
	if v == "" {
		return v
	}
	return strings.ToUpper(v[:1]) + v[1:]
}

func formatBaseUnit(v int, litre bool) string {
	if v < 1000 {
		if litre {
			return fmt.Sprintf("%d ml", v)
		}
		return fmt.Sprintf("%d g", v)
	}
	unit := "kg"
	if litre {
		unit = "L"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.2f", float64(v)/1000), ".00") + " " + unit
}

// prettyDate turns "2026-08-20" into "20 August" for customer copy.
func prettyDate(ymd string) string {
	if t, err := time.Parse("2006-01-02", ymd); err == nil {
		return t.Format("2 January")
	}
	return ymd
}

// toScheduleMap converts the wire type to the storage type.
//
// Two identical structs, deliberately. The wire type belongs to the JSON
// contract with update.html; the storage type belongs to the contract with
// item_due_on(). Keeping them apart means a change to what the page sends
// cannot silently alter what reaches the database.
func toScheduleMap(in map[string]ItemSchedule) map[string]schedule.ItemSchedule {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]schedule.ItemSchedule, len(in))
	for k, v := range in {
		out[k] = schedule.ItemSchedule{Type: v.Type, Days: v.Days, Anchor: v.Anchor}
	}
	return out
}

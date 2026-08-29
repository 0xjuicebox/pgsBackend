package schedule

// Next-delivery calculation, for telling a customer when milk actually starts.
//
// WHY THIS EXISTS
//
// Before this, approving a registration told the customer "you're approved"
// and nothing else. They had no idea whether the first delivery was that
// evening, the next morning, or three days later — so they either bought milk
// they didn't need or waited for milk that wasn't coming. Same for a customer
// coming back from a pause.
//
// A FOURTH COPY, DELIBERATELY
//
// The "is a delivery due on date D" predicate already exists three times:
// route.GenerateManifest, driver.CloseRoute, and stats.dueTodayCTE. A
// divergence between those has already caused one bug in this codebase.
//
// This is a fourth. That is not good, and it is the wrong long-term answer —
// the right one is to extract a single implementation and have the manifest,
// the close, the stats and this all call it. That change touches the hot path
// of every delivery run, which is not something to do days before a pilot
// launch.
//
// This copy is deliberately the most conservative of the four: it only ever
// looks forward, never writes anything, and only feeds message copy. If it
// disagrees with the manifest the customer gets a slightly wrong date in one
// message, not a missed delivery. The other three must agree with each other;
// this one merely wants to.
//
// When the extraction happens, search for SCHEDULE-PREDICATE-COPY to find all
// four sites.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// IST is the only timezone this system operates in. Delivery dates are
// wall-clock dates in India; computing them in UTC shifts them by a day for
// five and a half hours out of every twenty-four.
var IST = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// Should be impossible on any image with tzdata. Falling back to a
		// fixed offset is wrong across DST elsewhere but India has none, so
		// this stays correct rather than panicking at boot.
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}()

// Slot describes one subscription's schedule, enough to answer "when next".
type Slot struct {
	Slot         string // "morning" | "evening"
	ScheduleType string // "daily" | "alternate" | "custom"
	ActiveDays   []int  // 0=Sunday .. 6=Saturday, used when custom
	AnchorDate   time.Time
	Routed       bool // has a route_id — an unrouted slot is never delivered

	// ItemSchedules overrides the slot schedule per product. Empty means every
	// item follows the slot.
	ItemSchedules map[string]ItemSchedule
	// Quantities, so a next-delivery answer only counts items the customer
	// actually orders. Without this an alternate-day-butter customer would be
	// told their next delivery is tomorrow on the strength of ten products
	// they buy none of.
	Quantities map[string]int
}

// LoadSlots reads a customer's subscriptions in the shape this package needs.
func LoadSlots(ctx context.Context, db *pgxpool.Pool, customerID string) ([]Slot, error) {
	rows, err := db.Query(ctx, `
		SELECT slot, schedule_type, COALESCE(active_days, ARRAY[0,1,2,3,4,5,6]),
		       anchor_date, route_id IS NOT NULL,
		       item_schedules,
		       default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
		       default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
		       default_oil_qty, default_atta_qty, default_burfi_qty
		FROM subscriptions
		WHERE customer_id = $1
		ORDER BY slot
	`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		var s Slot
		var schedulesRaw []byte
		qty := make([]int, len(Products))
		ptrs := []any{&s.Slot, &s.ScheduleType, &s.ActiveDays, &s.AnchorDate, &s.Routed, &schedulesRaw}
		// Scan order matches the SELECT, which matches Products.
		for i := range qty {
			ptrs = append(ptrs, &qty[i])
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		s.ItemSchedules = ParseItemSchedules(schedulesRaw)
		s.Quantities = make(map[string]int, len(Products))
		for i, p := range Products {
			s.Quantities[p] = qty[i]
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// dueOn reports whether this slot delivers anything on the given date.
//
// With per-item schedules a slot is due when ANY item the customer orders is
// due. A customer whose only alternate-day item is butter has days where
// nothing at all arrives, and telling them "your next delivery is tomorrow"
// on such a day would be wrong.
//
// SCHEDULE-PREDICATE-COPY — see the package comment.
func (s Slot) dueOn(d time.Time) bool {
	if len(s.Quantities) > 0 {
		any := false
		for _, p := range Products {
			if s.Quantities[p] <= 0 {
				continue
			}
			if s.itemDueOn(p, d) {
				any = true
				break
			}
		}
		return any
	}
	return s.slotDueOn(d)
}

// itemDueOn applies a product's own schedule, falling back to the slot's.
// The Go mirror of item_due_on() in SQL — see the package comment on why a
// second implementation exists here and nowhere else.
func (s Slot) itemDueOn(product string, d time.Time) bool {
	sch, ok := s.ItemSchedules[product]
	if !ok || sch.Type == "" {
		return s.slotDueOn(d)
	}

	switch sch.Type {
	case "alternate":
		anchor := s.AnchorDate
		if sch.Anchor != "" {
			if t, err := time.Parse("2006-01-02", sch.Anchor); err == nil {
				anchor = t
			}
		}
		a := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, IST)
		t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, IST)
		days := int(t.Sub(a).Hours() / 24)
		if days < 0 {
			return false
		}
		return days%2 == 0
	case "custom":
		for _, wd := range sch.Days {
			if wd == int(d.Weekday()) {
				return true
			}
		}
		return false
	default:
		// Unrecognised types deliver, matching item_due_on(). Over-delivering
		// gets noticed the same morning; under-delivering does not.
		return true
	}
}

func (s Slot) slotDueOn(d time.Time) bool {
	switch s.ScheduleType {
	case "alternate":
		// Every second day counting from the anchor. Uses whole days between
		// midnights rather than a raw duration, so it can't be thrown off by
		// the times of day involved.
		a := time.Date(s.AnchorDate.Year(), s.AnchorDate.Month(), s.AnchorDate.Day(), 0, 0, 0, 0, IST)
		t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, IST)
		days := int(t.Sub(a).Hours() / 24)
		if days < 0 {
			return false
		}
		return days%2 == 0
	case "custom":
		for _, wd := range s.ActiveDays {
			if wd == int(d.Weekday()) {
				return true
			}
		}
		return false
	default: // "daily"
		return true
	}
}

// cutoffHour is the hour after which today's round for a slot has already
// been prepared, so a customer starting now cannot be on it.
//
// Approximations, not the configured cutoffs: this only decides whether to
// say "today evening" or "tomorrow morning" in a message, and reading
// system_config for a piece of message copy isn't worth the round trip. They
// are deliberately conservative — better to promise tomorrow and deliver
// today than the reverse.
func cutoffHour(slot string) int {
	if slot == "evening" {
		return 13 // evening order cutoff
	}
	return 2 // morning order cutoff
}

// NextDelivery returns the next date this slot will actually be delivered,
// looking forward from `from`. Returns false when the slot is unrouted or
// nothing falls within the search horizon.
func (s Slot) NextDelivery(from time.Time) (time.Time, bool) {
	if !s.Routed {
		// An unrouted slot appears on no manifest. Promising a date would be
		// a lie, and this is exactly the partial-approval case where a
		// customer is told deliveries are starting and then nothing arrives.
		return time.Time{}, false
	}

	now := from.In(IST)
	start := 0
	// Past today's cutoff, today's round is already loaded.
	if now.Hour() >= cutoffHour(s.Slot) {
		start = 1
	}

	// Two weeks is well beyond any schedule this system supports — a custom
	// schedule repeats weekly and alternate repeats every other day.
	for i := start; i <= 14; i++ {
		d := now.AddDate(0, 0, i)
		if s.dueOn(d) {
			return d, true
		}
	}
	return time.Time{}, false
}

// FirstDeliveryLabel renders the customer-facing description of when
// deliveries begin, across all of a customer's slots.
//
//	"Friday, 21 August (morning)"
//	"tomorrow morning"
//
// Returns a plain fallback rather than an empty string when nothing can be
// computed, because the value goes straight into a WhatsApp template variable
// and an empty one renders as a gap mid-sentence.
func FirstDeliveryLabel(slots []Slot, from time.Time) string {
	var best time.Time
	var bestSlot string
	found := false

	for _, s := range slots {
		d, ok := s.NextDelivery(from)
		if !ok {
			continue
		}
		if !found || d.Before(best) {
			best, bestSlot, found = d, s.Slot, true
		}
	}

	if !found {
		return "your next scheduled delivery"
	}
	return DateLabel(best, bestSlot, from)
}

// DateLabel renders a single date and slot the way a person would say it.
// Today and tomorrow are named rather than dated, because "tomorrow morning"
// is instantly understood and "Wednesday, 20 August (morning)" requires the
// reader to work out whether that's soon.
func DateLabel(d time.Time, slot string, from time.Time) string {
	now := from.In(IST)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, IST)
	target := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, IST)

	slotWord := strings.ToLower(slot)
	switch int(target.Sub(today).Hours() / 24) {
	case 0:
		return "today " + slotWord
	case 1:
		return "tomorrow " + slotWord
	}
	return fmt.Sprintf("%s (%s)", target.Format("Monday, 2 January"), slotWord)
}

// TomorrowLabel renders tomorrow's date for messages about staged order
// changes, which always take effect from the following day regardless of
// schedule — the customer's next delivery might be later, but the new order
// is what applies from then on.
func TomorrowLabel(from time.Time) string {
	return from.In(IST).AddDate(0, 0, 1).Format("2 January")
}

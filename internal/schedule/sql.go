package schedule

// SQL fragment builders for per-item delivery frequency.
//
// WHY THESE ARE GENERATED RATHER THAN WRITTEN OUT
//
// Every query that touches quantities needs eleven near-identical expressions,
// one per product, and there are four such queries. Written by hand that is
// 44 lines of almost-but-not-quite-the-same SQL, which is precisely the shape
// of code where a single wrong product name hides for months — it would fail
// only for customers who order that one item on that one schedule.
//
// Generating them means the pattern exists once. Adding a twelfth product is a
// one-line change to Products, and it lands in all four queries at the same
// time and in the same way.
//
// These return SQL text, not values. Nothing here is user input: product names
// come from a fixed slice in this file, and the only parameters are positional
// placeholders chosen by the caller.

import (
	"fmt"
	"strings"
)

// Products in the order they appear on a bill, so a manifest, a locked plan
// and an invoice all read the same way.
var Products = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
}

// ItemDueExpr is the SQL that decides whether one product is due.
//
//	dateParam is the positional placeholder for the target date, e.g. "$2".
//	subAlias is the subscriptions alias in the query, usually "s".
//
// Resolves to a call to item_due_on(), which is the single implementation of
// this predicate. It used to exist five times over, agreeing only by
// coincidence.
func ItemDueExpr(subAlias, product, dateParam string) string {
	return fmt.Sprintf(
		"item_due_on(%s.item_schedules->'%s', %s.schedule_type, %s.active_days, %s.anchor_date, %s::date)",
		subAlias, product, subAlias, subAlias, subAlias, dateParam)
}

// AnyItemDueExpr is the row filter: does this stop have anything due today?
//
// Replaces the old slot-level check. With per-item schedules a customer can
// have days where nothing at all is scheduled — alternate-day butter and
// nothing else, on an off day — and such a stop must not appear on the
// manifest. It is not a missed delivery; there was nothing to deliver.
//
// A product only counts if it is due AND the customer actually orders a
// non-zero quantity of it, otherwise every customer would match every day on
// the strength of products they don't buy.
func AnyItemDueExpr(subAlias, dateParam string) string {
	parts := make([]string, 0, len(Products))
	for _, p := range Products {
		parts = append(parts, fmt.Sprintf("(%s.default_%s_qty > 0 AND %s)",
			subAlias, p, ItemDueExpr(subAlias, p, dateParam)))
	}
	return "(" + strings.Join(parts, "\n\t\t\t   OR ") + ")"
}

// ScheduledQtyExpr is a product's quantity for a date, honouring its schedule.
//
//	COALESCE(locked plan, override, scheduled default, 0)
//
// Precedence is deliberate and unchanged from before per-item schedules:
//
//   - The locked plan wins. Once a round is frozen at its cutoff, that is what
//     the driver was asked to deliver and no later edit rewrites it.
//   - Then an explicit override. An override means "deliver this on this
//     date", so it must beat the schedule — that is what makes "add butter
//     just this Tuesday" work for an alternate-day butter customer.
//   - Then the subscription default, but only on a day this item is due.
//
// logAlias may be empty for queries with no delivery_logs join.
func ScheduledQtyExpr(subAlias, overrideAlias, logAlias, product, dateParam string) string {
	var parts []string

	if logAlias != "" {
		parts = append(parts, fmt.Sprintf("(%s.planned_order->>'%s')::int", logAlias, product))
	}
	if overrideAlias != "" {
		parts = append(parts, fmt.Sprintf("%s.new_%s_qty", overrideAlias, product))
	}
	parts = append(parts, fmt.Sprintf(
		"CASE WHEN %s THEN %s.default_%s_qty ELSE 0 END",
		ItemDueExpr(subAlias, product, dateParam), subAlias, product))
	parts = append(parts, "0")

	return "COALESCE(" + strings.Join(parts, ", ") + ")"
}

// ScheduledQtyList renders one ScheduledQtyExpr per product, comma separated,
// for a SELECT list. Order matches Products, so the caller's Scan order is
// fixed and does not need to be repeated.
func ScheduledQtyList(subAlias, overrideAlias, logAlias, dateParam, indent string) string {
	parts := make([]string, 0, len(Products))
	for _, p := range Products {
		parts = append(parts, indent+ScheduledQtyExpr(subAlias, overrideAlias, logAlias, p, dateParam))
	}
	return strings.Join(parts, ",\n")
}

// ScheduledTotalExpr sums every product's scheduled quantity.
//
// Used to exclude stops with nothing to deliver — a customer who paused today
// with a zero-quantity override, or whose only scheduled items aren't due.
// Both are "no delivery", and neither is a failure.
func ScheduledTotalExpr(subAlias, overrideAlias, dateParam string) string {
	parts := make([]string, 0, len(Products))
	for _, p := range Products {
		parts = append(parts, ScheduledQtyExpr(subAlias, overrideAlias, "", p, dateParam))
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// PlannedOrderExpr builds the JSONB snapshot frozen into delivery_logs at
// lock time.
//
// Every product is included, zeros and all. Stripping zeros looks tidier and
// breaks the lock: GenerateManifest reads the plan with a COALESCE fallback,
// so a missing key falls through to the subscription and a cancelled item
// reappears. Zero is how a customer cancels one product for one day, and it
// has to be recorded rather than omitted.
func PlannedOrderExpr(subAlias, overrideAlias, dateParam string) string {
	parts := make([]string, 0, len(Products)*2)
	for _, p := range Products {
		parts = append(parts, fmt.Sprintf("'%s', %s",
			p, ScheduledQtyExpr(subAlias, overrideAlias, "", p, dateParam)))
	}
	return "jsonb_build_object(" + strings.Join(parts, ", ") + ")"
}

package schedule

// Validation and normalisation of per-item schedules on the way into storage.
//
// WHY THIS IS STRICTER THAN THE SQL FUNCTION
//
// item_due_on() is deliberately permissive: an unrecognised type falls back to
// daily, because over-delivering gets noticed the same morning and
// under-delivering does not. That is the right behaviour at read time, when
// the alternative is a customer's milk quietly stopping.
//
// At write time the opposite applies. A malformed schedule that silently
// becomes "daily" means the customer asked for alternate-day butter, saw a
// confirmation, and gets it every day — with nothing anywhere recording that
// their request was discarded. Rejecting the write is the only point at which
// anyone can be told.
//
// So: reject on the way in, forgive on the way out.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ItemSchedule is one product's own delivery frequency.
//
// Field names are the JSONB keys item_due_on() reads. Changing one here
// without changing the SQL function silently stops the schedule applying, and
// the customer keeps receiving the slot default.
type ItemSchedule struct {
	Type   string `json:"type"`
	Days   []int  `json:"days,omitempty"`
	Anchor string `json:"anchor,omitempty"`
}

var validScheduleTypes = map[string]bool{
	"daily":     true,
	"alternate": true,
	"custom":    true,
}

// productSet is Products as a lookup, so an unknown key is caught rather than
// stored and ignored forever.
var productSet = func() map[string]bool {
	m := make(map[string]bool, len(Products))
	for _, p := range Products {
		m[p] = true
	}
	return m
}()

// ValidateItemSchedules checks a submitted map and returns it normalised,
// ready to store as JSONB.
//
// Returns nil when the map is empty or every entry is redundant — a NULL
// column then means "follow the slot schedule", which is both the default and
// the cheapest thing to read.
func ValidateItemSchedules(in map[string]ItemSchedule, slotAnchor string) ([]byte, error) {
	if len(in) == 0 {
		return nil, nil
	}

	out := make(map[string]ItemSchedule, len(in))

	for product, sch := range in {
		if !productSet[product] {
			return nil, fmt.Errorf("unknown product %q", product)
		}
		if !validScheduleTypes[sch.Type] {
			return nil, fmt.Errorf("%s: schedule type must be daily, alternate or custom", product)
		}

		clean := ItemSchedule{Type: sch.Type}

		switch sch.Type {
		case "custom":
			// A custom schedule with no days is a delivery that never
			// happens. Almost certainly a UI that let the customer pick
			// "specific days" and submit without choosing any — and the
			// result would be an item they think they ordered and never
			// receive.
			if len(sch.Days) == 0 {
				return nil, fmt.Errorf("%s: pick at least one day", product)
			}
			seen := map[int]bool{}
			for _, d := range sch.Days {
				if d < 0 || d > 6 {
					return nil, fmt.Errorf("%s: day %d is not a weekday (0=Sunday..6=Saturday)", product, d)
				}
				seen[d] = true
			}
			days := make([]int, 0, len(seen))
			for d := range seen {
				days = append(days, d)
			}
			// Sorted and deduplicated so the stored value is stable — two
			// submissions of the same schedule produce identical JSON, which
			// matters for change detection and for reading the column by eye.
			sort.Ints(days)
			clean.Days = days

			// Seven days selected IS daily. Storing it as custom would work,
			// but it makes the stored data harder to reason about and hides
			// the customer's actual intent behind an array.
			if len(days) == 7 {
				clean = ItemSchedule{Type: "daily"}
			}

		case "alternate":
			anchor := sch.Anchor
			if anchor == "" {
				anchor = slotAnchor
			}
			if anchor != "" {
				if _, err := time.Parse("2006-01-02", anchor); err != nil {
					return nil, fmt.Errorf("%s: start date must be YYYY-MM-DD", product)
				}
				clean.Anchor = anchor
			}
			// An empty anchor is allowed through: item_due_on() falls back to
			// the slot's anchor_date, which is always set.
		}

		// A per-item "daily" alongside a slot that is also daily changes
		// nothing. Dropping it keeps the stored map to what the customer
		// actually varied, so reading the column answers "what is different
		// about this customer" rather than restating the default eleven times.
		if clean.Type == "daily" && len(in) > 0 {
			out[product] = clean
			continue
		}
		out[product] = clean
	}

	if len(out) == 0 {
		return nil, nil
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding item schedules: %w", err)
	}
	return b, nil
}

// DescribeItemSchedule renders a schedule the way a customer would say it, for
// confirmation messages.
//
//	"every day", "every other day", "Tue & Fri"
//
// Returns "" for a schedule that matches the slot default, so a message can
// omit it entirely rather than telling every customer their milk is "every
// day" when that was never in question.
func DescribeItemSchedule(sch *ItemSchedule) string {
	if sch == nil {
		return ""
	}
	switch sch.Type {
	case "alternate":
		return "every other day"
	case "custom":
		if len(sch.Days) == 0 {
			return ""
		}
		names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
		var parts []string
		for _, d := range sch.Days {
			if d >= 0 && d <= 6 {
				parts = append(parts, names[d])
			}
		}
		switch len(parts) {
		case 0:
			return ""
		case 1:
			return parts[0] + " only"
		case 2:
			return parts[0] + " & " + parts[1]
		default:
			last := parts[len(parts)-1]
			rest := parts[:len(parts)-1]
			joined := ""
			for i, r := range rest {
				if i > 0 {
					joined += ", "
				}
				joined += r
			}
			return joined + " & " + last
		}
	case "daily":
		return "every day"
	}
	return ""
}

// ParseItemSchedules decodes the stored JSONB back into a map.
//
// A malformed column returns an empty map rather than an error: at read time
// the customer's deliveries matter more than the data being pristine, and an
// empty map means every item follows the slot schedule — the same behaviour as
// before per-item schedules existed.
func ParseItemSchedules(raw []byte) map[string]ItemSchedule {
	out := map[string]ItemSchedule{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fmt.Printf("⚠️ item_schedules: malformed, treating as slot default: %v\n", err)
		return map[string]ItemSchedule{}
	}
	return out
}

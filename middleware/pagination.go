package middleware

import (
	"context"
	"net/http"
	"strconv"
)

type contextKey string

const (
	PageKey  contextKey = "page"
	LimitKey contextKey = "limit"
)

// Default and maximum page sizes.
//
// The default was 10. That is a sensible number for a public API serving
// unknown clients, and the wrong one here: every consumer of these endpoints
// is our own admin app showing a list a human scrolls. A route picker that
// silently returned the ten most recent routes looked like "some routes are
// missing" rather than "this list is truncated" — and because the queries
// order by created_at DESC, it dropped the OLDEST records, which is the
// hardest form of truncation to notice.
//
// The handlers have their own fallbacks, but they never ran: this middleware
// always puts a value in the context, so the handler's `if limit < 1` branch
// is unreachable. Two defaults, one of them dead — which is how the real
// number stayed hidden.
const (
	defaultPageLimit = 100
	maxPageLimit     = 500
)

func Paginate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		//Safe fallbacks
		page := 1
		limit := defaultPageLimit

		if pStr := r.URL.Query().Get("page"); pStr != "" {
			if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
				page = p
			}
		}

		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
				// Silently clamping rather than erroring: a caller asking for
				// more than the cap gets the cap, which is the same shape of
				// quiet truncation described above — but an unbounded query on
				// a growing table is worse. 500 is well past any list a person
				// scrolls, so hitting it means the caller wants an export, and
				// that should be a different endpoint.
				if l > maxPageLimit {
					limit = maxPageLimit
				} else {
					limit = l
				}
			}
		}

		ctx := context.WithValue(r.Context(), PageKey, page)
		ctx = context.WithValue(ctx, LimitKey, limit)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

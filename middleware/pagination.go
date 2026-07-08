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

func Paginate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.Writer, r *http.Request) {

		//Safe fallbacks
		page := 1
		limit := 10

		if pStr := r.URL.Query().Get("page"); pStr != "" {
			if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
				page = p
			}
		}

		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
				if l > 100 {
					limit = 100
				} else {
					limit = l
				}
			}
		}

		ctx := context.WithValue(r.Context(), PageKey, page)
		ctx = context.WithValue(r.Context(), LimitKey, limit)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

package middleware

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

//type contextKey string

const UserIDKey contextKey = "user_id"
const PhoneKey contextKey = "phone"

var (
	jwks     keyfunc.Keyfunc
	jwksOnce sync.Once
)

// getJWKS parses the public keys directly from your local environment variable
func getJWKS() keyfunc.Keyfunc {
	jwksOnce.Do(func() {
		jwksJSON := os.Getenv("SUPABASE_JWKS")
		if jwksJSON == "" {
			fmt.Println("🚨 ERROR: SUPABASE_JWKS is missing from .env")
			return
		}

		// Directly parse the JSON Web Key Set!
		k, err := keyfunc.NewJWKSetJSON([]byte(jwksJSON))
		if err != nil {
			fmt.Printf("🚨 ERROR loading JWKS JSON: %v\n", err)
			return
		}

		jwks = k
		fmt.Println("✅ Supabase RS256 Auth initialized from local JSON!")
	})
	return jwks
}

func SupabaseAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, `{"error": "Missing Authorization header"}`, http.StatusUnauthorized)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			http.Error(w, `{"error": "Invalid Authorization header format"}`, http.StatusUnauthorized)
			return
		}

		k := getJWKS()
		if k == nil {
			http.Error(w, `{"error": "Auth system misconfigured"}`, http.StatusInternalServerError)
			return
		}

		// Verify the token using the Supabase JWKS
		token, err := jwt.Parse(parts[1], k.Keyfunc)

		if err != nil || !token.Valid {
			fmt.Printf("🚨 JWT REJECTED: %v\n", err)
			http.Error(w, `{"error": "Invalid or expired token"}`, http.StatusUnauthorized)
			return
		}

		claims, _ := token.Claims.(jwt.MapClaims)
		userID, _ := claims["sub"].(string)

		ctx := context.WithValue(r.Context(), UserIDKey, userID)
		if phone, ok := claims["phone"].(string); ok {
			ctx = context.WithValue(ctx, PhoneKey, phone)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

package util

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ElevationClaims is the payload of an elevation token. Distinct from the
// session JWT (entity.TokenPayload) so a stolen session cookie can't be
// used as an elevation token.
//
// Role is the per-host role the agent granted to this caller at /auth
// time. "user" by default; "admin" if the agent's MATRIX_ADMIN_USERS list
// includes the caller's email. The cloud uses it to gate per-app access
// (guest apps bypass entirely, user/admin apps require role >= required).
//
// IssuedAt/ExpiresAt are populated by ValidateElevation. For
// GenerateElevation: a zero IssuedAt is replaced by now (fresh elevation);
// a non-zero IssuedAt is preserved (slide refresh) so the absolute
// lifetime cap stays anchored to the original password prompt.
type ElevationClaims struct {
	Email     string
	Host      string
	Role      string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// GenerateElevation signs a short-lived elevation token. ttl is typically
// 5 minutes; the matching cookie should expire at the same time.
func GenerateElevation(ttl time.Duration, pl ElevationClaims, secret string) (string, error) {
	now := time.Now().UTC()
	iat := pl.IssuedAt
	if iat.IsZero() {
		iat = now
	}
	role := pl.Role
	if role == "" {
		role = "user"
	}
	claims := jwt.MapClaims{
		"sub":   pl.Email,
		"host":  pl.Host,
		"role":  role,
		"scope": "elev",
		"iat":   iat.Unix(),
		"exp":   now.Add(ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign elevation token: %w", err)
	}
	return signed, nil
}

// ValidateElevation parses and verifies an elevation JWT.
func ValidateElevation(token, secret string) (*ElevationClaims, error) {
	if token == "" {
		return nil, fmt.Errorf("elevation token required")
	}
	tk, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected method: %s", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := tk.Claims.(jwt.MapClaims)
	if !ok || !tk.Valid {
		return nil, fmt.Errorf("invalid elevation token")
	}
	if asString(claims["scope"]) != "elev" {
		return nil, fmt.Errorf("not an elevation token")
	}
	exp := time.Time{}
	if v, ok := claims["exp"].(float64); ok {
		exp = time.Unix(int64(v), 0)
	}
	iat := time.Time{}
	if v, ok := claims["iat"].(float64); ok {
		iat = time.Unix(int64(v), 0)
	}
	role := asString(claims["role"])
	if role == "" {
		// Backward-compat: tokens minted before the role claim existed
		// are treated as "user" (the default for everyone authenticated).
		role = "user"
	}
	return &ElevationClaims{
		Email:     asString(claims["sub"]),
		Host:      asString(claims["host"]),
		Role:      role,
		IssuedAt:  iat,
		ExpiresAt: exp,
	}, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// AuthRequest is the body of POST /auth.
//
// User is consulted on both code paths:
//   - OS path: must match the agent's running OS user; without that match
//     PAM/dscl/LogonUserW would need root to verify a different account
//     and we'd be silently weakening the gate.
//   - AuthURL path: forwarded verbatim to the external endpoint, which
//     owns the application-level user list.
type AuthRequest struct {
	User     string `json:"user"`
	Password string `json:"password" binding:"required"`
}

// AuthResponse is the body of a successful POST /auth.
type AuthResponse struct {
	User string `json:"user"`
	Role string `json:"role"` // "admin" or "user"
}

// AdminSet is a case-insensitive set of OAuth-identified emails used by
// the OS-auth path to scope who is admin on this host. Empty set means
// the OS-auth default (admin on any OS-verified login) applies.
type AdminSet map[string]struct{}

// NewAdminSet parses a comma-separated email list into a set. Whitespace
// around each entry is trimmed and case is folded so that
// "Alice@Example.com" matches "alice@example.com".
func NewAdminSet(spec string) AdminSet {
	out := make(AdminSet)
	for _, e := range strings.Split(spec, ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		out[e] = struct{}{}
	}
	return out
}

// Contains reports whether email is in the admin set (case-insensitive).
func (s AdminSet) Contains(email string) bool {
	if s == nil {
		return false
	}
	_, ok := s[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

// authHandler exposes the agent's credential check. The cloud's
// /h/:host/elevate handler proxies here; the agent itself doesn't mint
// session tokens — the cloud does, because only the cloud knows the
// OAuth-identified caller.
//
// Role policy:
//   - authURL != "" → external endpoint is fully in charge. Its returned
//     role is trusted; admins is ignored.
//   - authURL == "" → OS path. Default role is admin (an OS-verified
//     login already proves the caller owns the box). If admins is
//     non-empty it acts as an allowlist: callers whose
//     X-Periscope-User is missing from the list are downgraded to user.
func authHandler(auth hostauth.Authenticator, admins AdminSet, authURL string) gin.HandlerFunc {
	currentUser, _ := hostauth.CurrentUser()
	authURL = strings.TrimSpace(authURL)

	return func(c *gin.Context) {
		var req AuthRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.User = strings.TrimSpace(req.User)

		// Custom auth URL: fully delegates. The endpoint owns its user
		// list and the role decision.
		if authURL != "" {
			user, role, err := delegateAuth(authURL, req, c.GetHeader("X-Periscope-User"))
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, AuthResponse{User: user, Role: role})
			return
		}

		// OS path: username is required and must match the agent's own
		// OS user. Anything else is rejected before we touch PAM.
		if currentUser == "" {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "cannot determine current user"})
			return
		}
		if req.User == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "user required"})
			return
		}
		if !strings.EqualFold(req.User, currentUser) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		if err := auth.Authenticate(currentUser, req.Password); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}

		role := "admin"
		if len(admins) > 0 && !admins.Contains(c.GetHeader("X-Periscope-User")) {
			role = "user"
		}
		c.JSON(http.StatusOK, AuthResponse{User: currentUser, Role: role})
	}
}

// delegateAuth posts {user,password} to the configured external endpoint
// and parses its {user,role} response. The endpoint receives the
// portal-trusted X-Periscope-User header so it can correlate the
// application-level user to a matrix identity if it wants to.
func delegateAuth(authURL string, req AuthRequest, periscopeUser string) (string, string, error) {
	body, _ := json.Marshal(map[string]string{"user": req.User, "password": req.Password})
	httpReq, err := http.NewRequest(http.MethodPost, authURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if periscopeUser != "" {
		httpReq.Header.Set("X-Periscope-User", periscopeUser)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("auth endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("invalid credentials")
	}
	var out struct {
		User string `json:"user"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", fmt.Errorf("auth endpoint returned malformed body")
	}
	role := strings.ToLower(strings.TrimSpace(out.Role))
	if role != "admin" && role != "user" {
		role = "user"
	}
	user := strings.TrimSpace(out.User)
	if user == "" {
		user = strings.TrimSpace(req.User)
	}
	return user, role, nil
}

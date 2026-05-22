package adminui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// outboundSuggestionsResp is the shape the admin UI consumes when
// rendering the "Remote" dropdown in the Add-outbound form. Flat list,
// one row per (host, app); the UI groups visually by host.
type outboundSuggestionsResp struct {
	Suggestions []outboundSuggestion `json:"suggestions"`
}

type outboundSuggestion struct {
	Host   string `json:"host"`               // outpost agent name
	OsUser string `json:"os_user,omitempty"`  // OS user — used as the elevate `user` field
	Name   string `json:"name"`               // app name (matches the remote outpost's /apps row)
	Scheme string `json:"scheme,omitempty"`   // "http" or "tcp" — drives the local-mount shape
	Role   string `json:"role,omitempty"`     // remote's minimum role for the app
	Title  string `json:"title,omitempty"`    // host display title
	Online bool   `json:"online"`             // last_seen_at within freshness window
	Shared bool   `json:"shared,omitempty"`   // true if not owned by the caller
}

// handleOutboundSuggestions calls cloudbox's /api/v1/hosts (the
// aggregator that returns every paired host's app catalog plus
// per-app scheme) and flattens it into one row per (host, app). The
// admin UI uses the result to populate a dropdown so the operator
// doesn't have to type host/name/user/scheme by hand.
func (s *Server) handleOutboundSuggestions(c *gin.Context) {
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if fc == nil || fc.AccessToken == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable,
			gin.H{"error": "outpost has no access_token — pair with cloudbox first"})
		return
	}
	rows, err := fetchHostsFromCloudbox(c.Request.Context(), fc.ServerAddr, fc.ServerPort, fc.Protocol, fc.AccessToken)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	out := outboundSuggestionsResp{Suggestions: []outboundSuggestion{}}
	for _, h := range rows {
		for _, a := range h.Apps {
			scheme := a.Scheme
			if scheme == "" {
				scheme = "http" // legacy outposts that don't ship the field
			}
			out.Suggestions = append(out.Suggestions, outboundSuggestion{
				Host:   h.Host,
				OsUser: h.OsUser,
				Name:   a.Name,
				Scheme: scheme,
				Role:   a.Role,
				Title:  h.Title,
				Online: h.Online,
				Shared: h.Shared,
			})
		}
	}
	c.JSON(http.StatusOK, out)
}

// cbHostEntry mirrors the cloudbox /api/v1/hosts response. Defined
// locally so this package stays decoupled from any internal cloudbox
// types — the field set is intentionally narrow.
type cbHostEntry struct {
	Host   string        `json:"host"`
	OsUser string        `json:"os_user"`
	Title  string        `json:"title"`
	Online bool          `json:"online"`
	Shared bool          `json:"shared"`
	Apps   []cbAppEntry  `json:"apps"`
}

type cbAppEntry struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Scheme string `json:"scheme"`
}

// fetchHostsFromCloudbox calls /api/v1/hosts on the configured cloudbox
// with the outpost's persisted access_token. Protocol/port follow the
// same wss↔https / ws↔http mapping ssh-proxy uses.
func fetchHostsFromCloudbox(ctx context.Context, server string, port int, protocol, token string) ([]cbHostEntry, error) {
	s := strings.TrimSpace(server)
	if s == "" {
		return nil, fmt.Errorf("cloudbox URL not configured")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") || strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/hosts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("cloudbox /api/v1/hosts %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Hosts []cbHostEntry `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /api/v1/hosts: %w", err)
	}
	return out.Hosts, nil
}

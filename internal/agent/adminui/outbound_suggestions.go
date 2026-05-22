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
	Host         string `json:"host"`               // outpost agent name
	OsUser       string `json:"os_user,omitempty"`  // OS user — used as the elevate `user` field
	Name         string `json:"name"`               // app name (matches the remote outpost's /apps row)
	Scheme       string `json:"scheme,omitempty"`   // "http" or "tcp" — drives the local-mount shape
	RequireLogin bool   `json:"require_login"`      // whether the remote app demands elevation
	IndexPath    string `json:"index_path,omitempty"`
	Title        string `json:"title,omitempty"`    // host display title
	Online       bool   `json:"online"`             // last_seen_at within freshness window
	Shared       bool   `json:"shared,omitempty"`   // true if not owned by the caller
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
				Host:         h.Host,
				OsUser:       h.OsUser,
				Name:         a.Name,
				Scheme:       scheme,
				RequireLogin: a.RequireLogin,
				IndexPath:    a.IndexPath,
				Title:        h.Title,
				Online:       h.Online,
				Shared:       h.Shared,
			})
		}
		// Synthetic suggestion for the host's built-in /ssh server (no app
		// registration required). Scheme="ssh" tells the UI to render a
		// "Port" field and submit with name="" (the validator strips it).
		// Only emitted when the remote outpost actually has /ssh mounted —
		// older outposts omit `builtins`, which we treat as "all on" for
		// backward compat (matches the convention elsewhere in this code).
		if h.Builtins == nil || h.Builtins.SSH == nil || *h.Builtins.SSH {
			out.Suggestions = append(out.Suggestions, outboundSuggestion{
				Host:   h.Host,
				OsUser: h.OsUser,
				Name:   "", // signals built-in /ssh to the front-end
				Scheme: "ssh",
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
	Host     string        `json:"host"`
	OsUser   string        `json:"os_user"`
	Title    string        `json:"title"`
	Online   bool          `json:"online"`
	Shared   bool          `json:"shared"`
	Apps     []cbAppEntry  `json:"apps"`
	Builtins *cbBuiltins   `json:"builtins,omitempty"`
}

type cbAppEntry struct {
	Name         string `json:"name"`
	Scheme       string `json:"scheme"`
	RequireLogin bool   `json:"require_login"`
	IndexPath    string `json:"index_path"`
}

// cbBuiltins mirrors the `builtins` map cloudbox exposes per host. Fields
// are pointers so we can distinguish "absent (legacy outpost)" from
// "explicitly false". The synthetic-ssh-suggestion path treats nil as
// "enabled" for backward compat with old configs.
type cbBuiltins struct {
	Shell     *bool `json:"shell,omitempty"`
	Desktop   *bool `json:"desktop,omitempty"`
	Clipboard *bool `json:"clipboard,omitempty"`
	SSH       *bool `json:"ssh,omitempty"`
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

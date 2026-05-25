package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// upsertAppIn mirrors admincore.AppUpsertParams with extra schema
// annotations. URL is an alternative to the {scheme, host, port,
// socket} quartet — when set, it wins.
type upsertAppIn struct {
	Name               string   `json:"name" jsonschema:"Logical name; reachable at cloudbox /h/<host>/app/<name>/"`
	Icon               string   `json:"icon,omitempty"`
	URL                string   `json:"url,omitempty" jsonschema:"Single-URL target (http://, https://, tcp://, unix:///, npipe://). Wins over scheme/host/port/socket when set."`
	Scheme             string   `json:"scheme,omitempty" jsonschema:"http|https|tcp|unix|npipe"`
	Host               string   `json:"host,omitempty" jsonschema:"Defaults to 127.0.0.1 for TCP schemes"`
	Port               int      `json:"port,omitempty"`
	Socket             string   `json:"socket,omitempty" jsonschema:"Required for unix/npipe; ignored otherwise"`
	Enabled            bool     `json:"enabled" jsonschema:"Whether the app proxy is mounted live"`
	RequireLogin       bool     `json:"require_login,omitempty" jsonschema:"Refuse cloud requests that haven't cleared /elevate. Default false."`
	LANOnlyPaths       []string `json:"lan_only_paths,omitempty" jsonschema:"Path prefixes 404'd when X-Forwarded-Prefix is present"`
	IndexPath          string   `json:"index_path,omitempty" jsonschema:"Landing sub-path the cloudbox SPA prepends"`
	TrustCloudIdentity bool     `json:"trust_cloud_identity,omitempty" jsonschema:"Forward cloudbox-vouched identity as Remote-User / Remote-Email"`
}

type listAppsOut struct {
	Apps []conf.AppConfig `json:"apps"`
}

type upsertAppOut struct {
	OK  bool            `json:"ok"`
	App conf.AppConfig  `json:"app"`
}

type byNameIn struct {
	Name string `json:"name" jsonschema:"App name"`
}

type okOut struct {
	OK bool `json:"ok"`
}

type rotateTokenOut struct {
	OK                bool   `json:"ok"`
	ProvisioningToken string `json:"provisioning_token"`
}

type suggestionsOut struct {
	Suggestions []admincore.Suggestion `json:"suggestions"`
}

func (s *Server) registerAppsTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_list_apps",
		Description: "List the custom apps registered with this outpost. See also resource outpost://apps for an idempotent fetch.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, listAppsOut, error) {
		apps, err := s.core.ListApps()
		if err != nil {
			return apiErrResult[listAppsOut](err)
		}
		return nil, listAppsOut{Apps: apps}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_upsert_app",
		Description: "Add or update a custom app (reverse proxy to a local service). Live mutation — no restart. Pass either URL or the scheme/host/port/socket quartet.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in upsertAppIn) (*mcp.CallToolResult, upsertAppOut, error) {
		params := admincore.AppUpsertParams{
			AppConfig: conf.AppConfig{
				Name:               in.Name,
				Icon:               in.Icon,
				Scheme:             in.Scheme,
				Host:               in.Host,
				Port:               in.Port,
				Socket:             in.Socket,
				Enabled:            in.Enabled,
				RequireLogin:       in.RequireLogin,
				LANOnlyPaths:       in.LANOnlyPaths,
				IndexPath:          in.IndexPath,
				TrustCloudIdentity: in.TrustCloudIdentity,
			},
			URL: in.URL,
		}
		ac, err := s.core.UpsertApp(params)
		if err != nil {
			return apiErrResult[upsertAppOut](err)
		}
		return nil, upsertAppOut{OK: true, App: ac}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_delete_app",
		Description: "Remove a custom app by name. Idempotent — no error when the app doesn't exist.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in byNameIn) (*mcp.CallToolResult, okOut, error) {
		if err := s.core.DeleteApp(in.Name); err != nil {
			return apiErrResult[okOut](err)
		}
		return nil, okOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_rotate_app_token",
		Description: "Rotate the per-app provisioning bearer. Only valid when the app has trust_cloud_identity enabled. Returns the new token; the operator must update the cooperating app's config before its next push.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in byNameIn) (*mcp.CallToolResult, rotateTokenOut, error) {
		tok, err := s.core.RotateProvisioningToken(in.Name)
		if err != nil {
			return apiErrResult[rotateTokenOut](err)
		}
		return nil, rotateTokenOut{OK: true, ProvisioningToken: tok}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_suggest_apps",
		Description: "Probe well-known sockets (podman, docker, ollama) and the ycode manifest. Returns candidate apps the operator could register with outpost_upsert_app. Read-only — no mutation.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, suggestionsOut, error) {
		out, err := s.core.AppSuggestions()
		if err != nil {
			return apiErrResult[suggestionsOut](err)
		}
		return nil, suggestionsOut{Suggestions: out}, nil
	})
}

package otel

// This file lives in the otel package by historical accident — the
// package houses ycode-proxy discovery (Detect) and bearer injection
// (BearerInjector), both of which the ycode-share path needs. A
// future refactor could split otel-* vs ycode-* into separate
// packages; for now the bridge code lives here so SetProxyWrap can
// reuse the same wrap function for every ycode-backed surface.

// YcodeSurface describes one ycode-backed UI surface outpost can
// expose through the matrix tunnel as a per-surface built-in app.
// Each entry becomes /h/<host>/app/<Name>/ on cloudbox; outpost
// reverse-proxies to <ycode-proxy>/<Path>/ with the ycode bearer
// auto-injected.
type YcodeSurface struct {
	// Name is both the AppRegistry slot and the cloudbox tile name.
	// Convention: `ycode` for the canonical chat (cloudbox's
	// DefaultApps lists it), `ycode-<kind>` for everything else.
	Name string

	// Path is the sub-path on ycode's bearer-authed proxy at
	// 127.0.0.1:31415. Trailing slash kept so reverse-proxy joins
	// cleanly against an empty request path.
	Path string

	// Label is what the SPA toggle row says — human-readable.
	Label string

	// DefaultOn means the surface is enabled when YcodeShareSurfaces
	// in FileConfig is nil OR has no entry for this Name. Only `ycode`
	// (the chat) is default-on — turning ycode_share on without any
	// other config should land the operator on a useful surface.
	DefaultOn bool
}

// YcodeSurfaces returns the catalog of ycode-backed UI surfaces in
// stable order. main.go iterates and registers each one whose
// EnabledIn(fc) returns true; the SPA renders one toggle row per
// entry. Add new surfaces here when ycode publishes new ones in its
// componentPathMap (ycode/internal/observability/stack.go).
//
// `ycode-ollama` (the ycode-bundled ollama management UI) is distinct
// from the existing `ollama` built-in app, which proxies the ollama
// daemon's raw API at :11434 — operators should treat them as two
// different things, the API for programmatic use and the UI for
// pulling/listing/inspecting models.
func YcodeSurfaces() []YcodeSurface {
	return []YcodeSurface{
		{Name: "ycode", Path: "/chat/", Label: "ycode Chat", DefaultOn: true},
		{Name: "ycode-canvas", Path: "/ycode/canvas/", Label: "ycode Canvas (a2ui)"},
		{Name: "ycode-ollama", Path: "/ollama/", Label: "Ollama UI (model pull/list)"},
		{Name: "ycode-git", Path: "/git/", Label: "Embedded Gitea (git)"},
		{Name: "ycode-memos", Path: "/memos/", Label: "Embedded Memos"},
		{Name: "ycode-graph", Path: "/graph/", Label: "Bonsai graph (kg's DB)"},
	}
}

// YcodeSurfaceEnabled folds the catalog's DefaultOn with the
// per-surface overlay map. Returns true when the operator opted in,
// or when the surface is in the default-on set and the operator
// hasn't overridden. Reads safely from a nil map.
func YcodeSurfaceEnabled(overlay map[string]bool, name string) bool {
	for _, s := range YcodeSurfaces() {
		if s.Name != name {
			continue
		}
		if v, ok := overlay[name]; ok {
			return v
		}
		return s.DefaultOn
	}
	return false
}

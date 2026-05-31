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
// entry.
//
// Phase-scoped to chat-style entry points for now:
//   - ycode         → /chat/         the polished chat ycode itself
//                                     advertises as the canonical
//                                     entry (default-on)
//   - ycode-canvas  → /ycode/canvas/ canvas/a2ui interaction surface
//   - ycode-classic → /ycode/        the minimal chat at the legacy
//                                     /ycode/ path
//
// Non-chat surfaces (ollama UI, git, memos, graph) live in ycode's
// componentPathMap and are still reachable via outpost-add custom
// apps; they were briefly included in this catalog and pulled back
// out so operators get a focused list of "ways to chat with ycode."
// Re-introduce here once we have UI affordances to keep them visually
// grouped separately from chats.
//
// Forward direction: canvas + chat are converging toward the unified
// agentic interaction surface, with the other paths potentially
// subsumed in a future ycode release. Keeping this list small now
// matches the trajectory and avoids surfacing options that will
// disappear later.
func YcodeSurfaces() []YcodeSurface {
	return []YcodeSurface{
		{Name: "ycode", Path: "/chat/", Label: "ycode Chat", DefaultOn: true},
		{Name: "ycode-canvas", Path: "/ycode/canvas/", Label: "ycode Canvas (a2ui)"},
		{Name: "ycode-classic", Path: "/ycode/", Label: "ycode (classic SPA)"},
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

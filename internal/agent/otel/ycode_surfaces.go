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

	// Label is what the SPA offers in the surface picker — human-readable.
	Label string

	// DefaultOn means the surface is published when the ycode unit's
	// `settings.share` names no surface at all. Only `ycode` (the chat) is
	// default-on — enabling the unit without further configuration should
	// land the operator on a useful surface.
	DefaultOn bool
}

// YcodeSurfaces returns the catalog of ycode-backed UI surfaces in
// stable order. The ycode service unit's capability iterates it and
// publishes each surface its `settings.share` list selects (or the
// DefaultOn set when the list is empty); the SPA offers the catalog so an
// operator can write that list.
//
// Phase-scoped to chat-style entry points for now:
//   - ycode         → /chat/         the polished chat ycode itself
//     advertises as the canonical
//     entry (default-on)
//   - ycode-canvas  → /ycode/canvas/ canvas/a2ui interaction surface
//   - ycode-classic → /ycode/        the minimal chat at the legacy
//     /ycode/ path
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

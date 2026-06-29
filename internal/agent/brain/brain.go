// Package brain is cloudbox/outpost/bashy's built-in decision-maker — the faculty
// that lets the platform DECIDE, not just execute, for any operation that needs
// judgment. It is the defining differentiator from an ordinary PaaS: a cloud that
// reasons about its own operation.
//
// Every decision is made the same way: a deterministic BOOTSTRAP (always
// available, no dependency — so the brain is never a critical-path failure point)
// and then, when a pooled-LLM Refiner is wired, the mesh's OWN intelligence
// refines it. The bootstrap is what makes it safe to put a brain behind
// everything. Sharding's leader-election is the first caller; provisioning,
// placement, fleet upgrades, and conflict-resolution are the same shape.
package brain

import "context"

// Refiner consults the pooled LLM (cloudbox /v1 — served by the mesh itself) to
// improve a bootstrap answer, given a structured prompt describing the decision.
// A nil refiner, an error, or an unparseable reply leaves the bootstrap standing.
type Refiner func(ctx context.Context, prompt string) (string, error)

// Decide returns the deterministic bootstrap, then refines it with the pooled LLM
// when a refiner is available and yields a parseable improvement. The bootstrap
// ALWAYS stands otherwise — judgment is layered on, never depended on. fromLLM
// reports whether the pooled intelligence actually changed the answer.
func Decide[T any](ctx context.Context, bootstrap T, prompt string, refine Refiner, parse func(string) (T, bool)) (result T, fromLLM bool) {
	if refine != nil {
		if out, err := refine(ctx, prompt); err == nil {
			if v, ok := parse(out); ok {
				return v, true
			}
		}
	}
	return bootstrap, false
}

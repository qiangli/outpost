package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/qiangli/outpost/internal/agent"
)

const RepairFailureThreshold = 3
const RepairStuckAfter = 6 * time.Hour

type RepairLedger struct {
	Path      string
	Component string
	Peer      string
	Now       func() time.Time
}
type RepairEvidence struct {
	Running      string    `json:"running_version"`
	Target       Envelope  `json:"target"`
	Attempts     int       `json:"attempts"`
	FirstFailure time.Time `json:"first_failure,omitempty"`
	Automatic    bool      `json:"automatic"`
	Peer         string    `json:"peer,omitempty"`
}

func (r RepairLedger) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
func (r RepairLedger) component() string {
	if r.Component != "" {
		return r.Component
	}
	return "outpost"
}
func (r RepairLedger) id(env Envelope) string {
	return fmt.Sprintf("upgrade-%x", sha256.Sum256([]byte(r.component()+"\x00"+r.Peer+"\x00"+env.ReleaseID)))
}
func repairEvidence(row agent.HealRecord) (RepairEvidence, error) {
	var e RepairEvidence
	b, err := json.Marshal(row.Evidence)
	if err == nil {
		err = json.Unmarshal(b, &e)
	}
	return e, err
}

// ValidateRepairTarget requires an immutable, checksum-verified HTTPS source.
func ValidateRepairTarget(env Envelope) error {
	u, err := url.Parse(env.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("repair requires an HTTPS URL without credentials")
	}
	b, err := hex.DecodeString(env.SHA256)
	if err != nil || len(b) != 32 {
		return errors.New("repair requires a 64-digit sha256")
	}
	if env.ReleaseID == "" {
		return errors.New("repair requires a target version")
	}
	return nil
}

// Observe folds repeated observations into one incident per component/peer/target.
// Accepted async work is not success: applied must be set only after the swap.
func (r RepairLedger) Observe(cur agent.BuildInfo, env Envelope, automatic bool, reason string, applied bool) error {
	if r.Path == "" {
		return nil
	}
	if err := ValidateRepairTarget(env); err != nil {
		return err
	}
	decline, _ := shouldDeclinePulledTarget(cur, env)
	if decline && !applied {
		return nil
	}
	return agent.UpdateHeal(r.Path, r.id(env), func(row *agent.HealRecord) error {
		if row.Result == "healed" || row.Result == "repairing" {
			return nil
		}
		e := RepairEvidence{Running: cur.Version, Target: env, Automatic: automatic, Peer: r.Peer}
		if row.Rung != "" {
			var err error
			e, err = repairEvidence(*row)
			if err != nil {
				return err
			}
		} else {
			*row = agent.HealRecord{ID: row.ID, App: r.component(), Incident: row.ID, Rung: "upgrade", Time: r.now(), Result: "outdated", Reason: "newer verified target available"}
			if automatic {
				row.Result = "reported"
			}
		}
		e.Automatic = automatic
		if automatic && row.Result == "outdated" {
			row.Result = "reported"
		}
		if reason != "" {
			e.Attempts++
			if e.FirstFailure.IsZero() {
				e.FirstFailure = r.now()
			}
			row.Reason = reason
			if e.Attempts >= RepairFailureThreshold || r.now().Sub(e.FirstFailure) >= RepairStuckAfter {
				row.Result = "stuck"
			}
		}
		if applied {
			row.Result = "healed"
			row.Reason = "target applied"
		}
		row.Before = cur.Version
		row.After = env.ReleaseID
		row.Evidence = e
		return nil
	})
}

// Approve claims the incident durably before invoking the existing CLI swap.
// The swap callback must return only after verification of the running version.
func (r RepairLedger) Approve(ctx context.Context, id string, apply func(context.Context, Envelope) error) error {
	var env Envelope
	err := agent.UpdateHeal(r.Path, id, func(row *agent.HealRecord) error {
		if row.Rung != "upgrade" {
			return errors.New("upgrade repair not found")
		}
		if row.Result == "healed" || row.Result == "repairing" {
			return errors.New("repair already healed or running")
		}
		e, err := repairEvidence(*row)
		if err != nil {
			return err
		}
		if e.Peer != "" {
			return errors.New("use peers upgrade for a peer repair")
		}
		if row.App != "outpost" && row.App != "bashy" {
			return errors.New("component requires its managed reconciler")
		}
		if err := ValidateRepairTarget(e.Target); err != nil {
			return err
		}
		env = e.Target
		row.Result = "repairing"
		return nil
	})
	if err != nil {
		return err
	}
	err = apply(ctx, env)
	finish := r.Finish(id, err)
	return errors.Join(err, finish)
}
func (r RepairLedger) Finish(id string, cause error) error {
	return agent.UpdateHeal(r.Path, id, func(row *agent.HealRecord) error {
		if row.Rung != "upgrade" {
			return errors.New("upgrade repair not found")
		}
		row.Result = "healed"
		row.Reason = "target applied and verified"
		if cause != nil {
			row.Result = "failed"
			row.Reason = cause.Error()
		}
		return nil
	})
}

// RecordMismatch reports a failed managed companion reconcile, even when the
// release service could not yet resolve a checksum. Unresolved records cannot
// be approved until a subsequent reconcile supplies a verified target.
func (r RepairLedger) RecordMismatch(have string, env Envelope, cause error) error {
	if r.Path == "" || have == env.ReleaseID || cause == nil {
		return nil
	}
	return agent.UpdateHeal(r.Path, r.id(env), func(row *agent.HealRecord) error {
		e := RepairEvidence{Running: have, Target: env, FirstFailure: r.now(), Attempts: 1}
		if row.Rung != "" {
			old, err := repairEvidence(*row)
			if err != nil {
				return err
			}
			e.FirstFailure = old.FirstFailure
			e.Attempts = old.Attempts + 1
		}
		*row = agent.HealRecord{ID: row.ID, App: r.component(), Incident: row.ID, Rung: "upgrade", Result: "stuck", Before: have, After: env.ReleaseID, Reason: cause.Error(), Time: r.now(), Evidence: e}
		return nil
	})
}

func (r RepairLedger) FinishTarget(env Envelope, cause error) error {
	return r.Finish(r.id(env), cause)
}

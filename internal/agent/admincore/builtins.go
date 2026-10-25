package admincore

import (
	"context"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// BuiltinsParams is the partial-update shape for SetBuiltins. Pointer-
// bool fields mean "leave unchanged when nil"; non-nil fields are
// written through to the FileConfig.
//
// The set of fields here is broader than the admin SPA currently
// surfaces — SSHAllowRemoteForward, SSHAllowAgentForward, and
// SSHForwardSockets exist in FileConfig but the SPA has no toggle for
// them. MCP / CLI callers can drive them directly.
type BuiltinsParams struct {
	Shell                 *bool    `json:"shell,omitempty"`
	Desktop               *bool    `json:"desktop,omitempty"`
	Clipboard             *bool    `json:"clipboard,omitempty"`
	SSH                   *bool    `json:"ssh,omitempty"`
	SSHAllowLocalForward  *bool    `json:"ssh_allow_local_forward,omitempty"`
	SSHAllowRemoteForward *bool    `json:"ssh_allow_remote_forward,omitempty"`
	SSHAllowAgentForward  *bool    `json:"ssh_allow_agent_forward,omitempty"`
	SSHForwardSockets     []string `json:"ssh_forward_sockets,omitempty"`
	SFTP                  *bool    `json:"sftp,omitempty"`
	// The File Browser, the podman and sandbox container mounts, the Ollama
	// proxy and its pool, the warm plane, the shard plane, the same-LAN
	// inference listener and the observability / ycode surfaces are NOT
	// fields here any more. Each was a toggle plus a handful of config keys
	// that needed their own reader, lifecycle and exposure code; they are
	// in-process service units now (capability `outpost.<name>`), so they
	// are switched through Services and configured through ServiceSettings
	// below — the same two writes a seeded built-in or a third-party daemon
	// takes. Removing them is what makes "a new daemon needs no outpost
	// change" true rather than aspirational.
	Cluster        *bool    `json:"cluster,omitempty"`
	ClusterAgent   *bool    `json:"cluster_agent,omitempty"`
	ClusterVirtual []string `json:"cluster_virtual,omitempty"`
	// NOTE: update_mode / auto_rollback were removed. Upgrades always
	// apply and the auto-rollback watchdog always reverts an
	// unconfirmed upgrade — there is nothing to toggle.
	// Mesh toggles the libp2p mesh data plane (the peer node carrying
	// authenticated, NAT-traversing peer↔peer streams). MeshPort sets its
	// TCP+QUIC listen port (0 = ephemeral). nil = leave unchanged.
	Mesh     *bool `json:"mesh,omitempty"`
	MeshPort *int  `json:"mesh_port,omitempty"`
	// Services is the generic desired-state write over service units: unit
	// name -> enabled. It is the ONLY way outpost turns a service on or off,
	// and it is the same write `bashy service enable|disable` performs — one
	// sparse overlay in fleet's unit store, validated by fleet's rules
	// (including the port-conflict check).
	//
	// There are deliberately no per-service fields here any more. `loom`,
	// `meet` and `apps` were three hardcoded toggles that each needed their
	// own config key, reader, lifecycle and exposure code; they are seeded
	// unit DECLARATIONS now, so a third-party daemon an operator just
	// declared is configured through this same map with no outpost change.
	// The in-process outpost surfaces (the File Browser, the container
	// mounts, the Ollama proxy and pool, the warm and shard planes, the
	// same-LAN inference listener, the observability and ycode surfaces)
	// went the same way: they are units whose capability happens to live
	// inside this process, and nothing here knows which is which.
	//
	// Applies LIVE: the running monitor reconciles the new desired state and
	// outpost's exposure reconciler republishes, so a toggle does not need
	// an outpost restart.
	Services map[string]bool `json:"services,omitempty"`
	// ServiceSettings overrides fields of a unit's declaration: unit name ->
	// dotted path -> value, the `bashy service set NAME --set k=v` shape
	// (e.g. {"loom":{"ports.0.port":"31900"}}). An override of a seeded
	// built-in writes a sparse local overlay; removing it restores the seed.
	ServiceSettings map[string]map[string]string `json:"service_settings,omitempty"`
	// BashyVersion pins the bashy release the self-heal auto-install fetches
	// when bashy is missing ("" / "latest" = newest; a tag pins it). Takes
	// effect on the next restart. nil = leave unchanged.
	BashyVersion *string `json:"bashy_version,omitempty"`
	// CloudDOEnabled toggles Digital Ocean provider support; CloudDOToken is the
	// DO API token (exported as DIGITALOCEAN_ACCESS_TOKEN). nil = unchanged.
	CloudDOEnabled *bool   `json:"cloud_do_enabled,omitempty"`
	CloudDOToken   *string `json:"cloud_do_token,omitempty"`
}

// BuiltinsResult reports what happened. RestartPending is true when
// the change is one the tunnel / built-in routes need to reload to
// observe — callers should poll Status until the daemon is back.
// ServicesApplied is true when service desired state moved; that takes
// effect immediately, so a caller can say "applied" rather than repeating
// the old, false "restart to apply".
type BuiltinsResult struct {
	OK              bool `json:"ok"`
	RestartPending  bool `json:"restart_pending"`
	ServicesApplied bool `json:"services_applied,omitempty"`
}

// SetBuiltins applies the partial update p to the persisted FileConfig
// and (when the host is paired) schedules a restart so the new toggles
// take effect. On a first-time setup (AgentName empty) nothing is
// mounted yet, so the save is harmless and no restart is triggered.
//
// Service units are the exception, and deliberately so: p.Services /
// p.ServiceSettings are written to the host's unit registry and applied
// LIVE — the monitor reconciles the processes and the exposure reconciler
// republishes — so enabling a service no longer reports a restart it did not
// schedule. Everything still in FileConfig keeps the persist-then-defer
// behaviour described below.
//
// ctx bounds the unit-store writes (one of them shells out to the registry's
// own `set` verb); the FileConfig write is local and unbounded.
func (s *Server) SetBuiltins(ctx context.Context, p BuiltinsParams) (BuiltinsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return BuiltinsResult{}, err
	}
	servicesChanged, err := s.applyServiceDesiredState(ctx, p)
	if err != nil {
		return BuiltinsResult{}, err
	}
	if p.Shell != nil {
		fc.ShellEnabled = p.Shell
	}
	if p.Desktop != nil {
		fc.DesktopEnabled = p.Desktop
	}
	if p.Clipboard != nil {
		fc.ClipboardEnabled = p.Clipboard
	}
	if p.SSH != nil {
		fc.SSHEnabled = p.SSH
	}
	if p.SSHAllowLocalForward != nil {
		fc.SSHAllowLocalForward = p.SSHAllowLocalForward
	}
	if p.SSHAllowRemoteForward != nil {
		fc.SSHAllowRemoteForward = p.SSHAllowRemoteForward
	}
	if p.SSHAllowAgentForward != nil {
		fc.SSHAllowAgentForward = p.SSHAllowAgentForward
	}
	if p.SSHForwardSockets != nil {
		fc.SSHForwardSockets = p.SSHForwardSockets
	}
	if p.SFTP != nil {
		fc.SFTPEnabled = p.SFTP
	}
	if p.Mesh != nil {
		fc.MeshEnabled = p.Mesh
	}
	if p.MeshPort != nil {
		fc.MeshPort = *p.MeshPort
	}
	if p.BashyVersion != nil {
		fc.BashyVersion = strings.TrimSpace(*p.BashyVersion)
	}
	if p.CloudDOEnabled != nil {
		fc.CloudDOEnabled = p.CloudDOEnabled
	}
	if p.CloudDOToken != nil {
		fc.CloudDOToken = *p.CloudDOToken
	}
	if p.Cluster != nil {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		// Persist the pointer as-is: an explicit true/false from the
		// operator's toggle is exactly the opt-in/opt-out signal
		// ClusterOn wants to distinguish from the default-on nil.
		fc.Cluster.Enabled = p.Cluster
	}
	if p.ClusterAgent != nil || p.ClusterVirtual != nil {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		if p.ClusterAgent != nil {
			fc.Cluster.Runtimes.Agent = *p.ClusterAgent
		}
		if p.ClusterVirtual != nil {
			virtual, err := conf.NormalizeVirtualRuntimes(p.ClusterVirtual)
			if err != nil {
				return BuiltinsResult{}, badRequest("cluster_virtual: %s", err.Error())
			}
			fc.Cluster.Runtimes.Virtual = virtual
		}
	}
	if fc.ClusterOn() && !fc.Cluster.HasAgentRuntime() && len(fc.Cluster.VirtualRuntimes()) == 0 {
		return BuiltinsResult{}, badRequest("an enabled cluster requires cluster_agent or at least one cluster_virtual runtime")
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return BuiltinsResult{}, internalErr("%s", err.Error())
	}
	// Persist-then-defer: the new toggle is durable on disk, but the
	// route mount / built-in app registration only happens at boot.
	// Rather than auto-restart (which yanks the admin UI mid-click and
	// breaks batches of operator toggles), advertise RestartPending so
	// the SPA can surface a sticky "Restart to apply" banner and let
	// the operator pull the trigger when their batch is done.
	// Only a boot-only toggle needs a restart. Service desired state
	// was already applied by the unit store, the monitor and the
	// exposure reconciler — a call carrying just services reports no
	// restart, because none is coming.
	restart := fc.AgentName != "" && touchesBootOnlyConfig(p)
	return BuiltinsResult{OK: true, RestartPending: restart, ServicesApplied: servicesChanged}, nil
}

// touchesBootOnlyConfig reports whether p carries a FileConfig toggle that
// the tunnel or a built-in route only reads at boot, and therefore needs a
// daemon restart to observe.
//
// It is the single predicate the restart decision is made on, so a new
// toggle cannot be remembered in one of several parallel conditions and
// forgotten in another. Services/service_settings are deliberately absent
// from it: they are applied live by the unit store, the monitor and the
// exposure reconciler.
func touchesBootOnlyConfig(p BuiltinsParams) bool {
	return !(p.Shell == nil && p.Desktop == nil && p.Clipboard == nil && p.SSH == nil && p.SSHAllowLocalForward == nil && p.SSHAllowRemoteForward == nil && p.SSHAllowAgentForward == nil && p.SSHForwardSockets == nil && p.SFTP == nil && p.Cluster == nil && p.ClusterAgent == nil && p.ClusterVirtual == nil && p.Mesh == nil && p.MeshPort == nil && p.BashyVersion == nil && p.CloudDOEnabled == nil && p.CloudDOToken == nil)
}

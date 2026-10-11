package svcunit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// AppExposer is the matrix-tunnel half of outpost's exposure surface — the
// subset of agent.AppRegistry this package needs. Narrow on purpose: the
// registry lives in internal/agent, which must stay free of a dependency on
// this package.
type AppExposer interface {
	RegisterApp(name, target string, meta AppMeta) error
	UnregisterApp(name string)
}

// MeshExposer is the mesh half (mesh.Forwarder).
type MeshExposer interface {
	Expose(name, loopbackAddr string)
	Unexpose(name string)
}

// AppMeta is the access posture of one exposed app route. It mirrors
// agent.AppMeta's fields that a unit declaration can set, so the wiring in
// cmd/outpost translates one struct and no policy is decided here.
type AppMeta struct {
	RequireLogin       bool
	ElevationRequired  bool
	TrustCloudIdentity bool
	SSOSecret          string
	IndexPath          string
}

// Secrets supplies the per-app HMAC key outpost signs trusted-identity
// headers with. The key is outpost's, not the declaration's: a unit never
// carries an inline secret (fleet.ServiceSecret is a vault REFERENCE), so
// `trust_cloud_identity: true` in YAML means "mint and use one", and this is
// what mints it.
type Secrets interface {
	SSOSecret(app string) (string, error)
}

// Exposure is the resolved publication of one unit: what gets registered
// while the unit's state allows it, and withdrawn otherwise.
type Exposure struct {
	Unit string
	// App is the tunnel route, when the unit declares expose.app.
	App     *AppExposure
	Mesh    []MeshExposure
	Allowed bool // the unit's state permits publication
	// Reason explains a withdrawal (state, missing port, …) for the log and
	// the status surface.
	Reason string
}

// AppExposure is one matrix-tunnel app route.
type AppExposure struct {
	Name   string
	Target string
	Meta   AppMeta
}

// MeshExposure is one mesh service name bound to a loopback address.
type MeshExposure struct {
	Name string
	Addr string
}

// Names lists every published identity this exposure owns, for the status
// surface's `exposed` field.
func (e Exposure) Names() []string {
	var out []string
	if e.App != nil {
		out = append(out, "app:"+e.App.Name)
	}
	for _, m := range e.Mesh {
		out = append(out, "mesh:"+m.Name)
	}
	sort.Strings(out)
	return out
}

// Plan resolves what one unit should publish given its declaration and the
// monitor's state for it. It decides nothing about WHEN — a caller compares
// successive plans — and it reads only declared data, so a third-party unit
// and a seeded built-in produce identical shapes.
//
// state is the monitor's verdict. Only Active and Degraded publish
// (service.State.Exposed): a failed, stopping, starting, inactive or disabled
// unit is withdrawn, which is the behaviour the old supervisor lacked — a
// dead service kept its tunnel route and its mesh name forever.
func Plan(u fleet.Service, state service.State, sec Secrets) (Exposure, error) {
	ex := Exposure{Unit: u.Name}
	switch {
	case !u.Enabled:
		ex.Reason = "unit disabled"
	case !state.Exposed():
		ex.Reason = "unit state " + string(state)
	default:
		ex.Allowed = true
	}

	if a := u.Expose.App; a != nil {
		port, ok := u.PortNamed(a.Port)
		if !ok {
			return ex, fmt.Errorf("svcunit: %s: expose.app names port %q with no matching listener", u.Name, a.Port)
		}
		if port.Port <= 0 {
			return ex, fmt.Errorf("svcunit: %s: expose.app needs a positive port", u.Name)
		}
		name := a.Name
		if name == "" {
			name = u.Name
		}
		meta := AppMeta{
			RequireLogin:       a.RequireLogin,
			ElevationRequired:  a.Elevation,
			TrustCloudIdentity: a.TrustCloudIdentity,
		}
		if meta.TrustCloudIdentity {
			if sec == nil {
				return ex, fmt.Errorf("svcunit: %s: trust_cloud_identity needs an SSO secret source", u.Name)
			}
			secret, err := sec.SSOSecret(name)
			if err != nil {
				return ex, fmt.Errorf("svcunit: %s: sso secret for app %q: %w", u.Name, name, err)
			}
			if strings.TrimSpace(secret) == "" {
				return ex, fmt.Errorf("svcunit: %s: trust_cloud_identity requires a non-empty sso secret for app %q", u.Name, name)
			}
			meta.SSOSecret = secret
		}
		ex.App = &AppExposure{
			Name:   name,
			Target: fmt.Sprintf("http://%s", loopback(port.Port)),
			Meta:   meta,
		}
	}

	for _, m := range u.Expose.Mesh {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		port, ok := u.PortNamed(m.Port)
		if !ok || port.Port <= 0 {
			return ex, fmt.Errorf("svcunit: %s: expose.mesh %q names port %q with no usable listener", u.Name, m.Name, m.Port)
		}
		ex.Mesh = append(ex.Mesh, MeshExposure{Name: m.Name, Addr: loopback(port.Port)})
	}
	return ex, nil
}

// loopback is the address outpost proxies to. A unit's bind may be lan, but
// outpost always reaches it on loopback: the tunnel ingress and the mesh
// forwarder are both local to this host, and routing through the LAN address
// would put host traffic on the wire for no reason.
func loopback(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

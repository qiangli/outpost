package admincore

import (
	"fmt"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Runtime callbacks use the same disk owner as operator mutations. They must
// never recover a failed read by writing an old boot snapshot.
func (s *Server) loadRuntimeConfig() (*conf.FileConfig, error) {
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		return nil, err
	}
	if fc == nil {
		return nil, fmt.Errorf("runtime configuration is missing")
	}
	return fc, nil
}

func (s *Server) RotateMCPBearer() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadRuntimeConfig()
	if err != nil {
		return "", err
	}
	return conf.RotateMCPBearerToken(s.deps.ConfigPath, fc)
}

func (s *Server) StoreHostCertificate(agent, accessToken, cert, ca string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadRuntimeConfig()
	if err != nil {
		return err
	}
	if fc.AgentName != agent || fc.AccessToken != accessToken {
		return fmt.Errorf("host identity changed during certificate refresh")
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}
	fc.Cluster.HostCert, fc.Cluster.CAPubkey = cert, ca
	return conf.SaveFile(s.deps.ConfigPath, fc)
}

// PrepareControlPlane fills only absent generated credentials/path, returning
// an independent snapshot for runtime options and reconcilers. No container
// operation runs under the configuration lock.
func (s *Server) PrepareControlPlane(kubeconfig string) (*conf.FileConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadRuntimeConfig()
	if err != nil {
		return nil, err
	}
	if !fc.Cluster.ControlPlaneOn() {
		return fc, nil
	}
	if _, err = conf.EnsureClusterTunnelToken("", fc); err != nil {
		return nil, err
	}
	if _, err = conf.EnsureControlPlaneSTCPSecret("", fc); err != nil {
		return nil, err
	}
	if strings.TrimSpace(fc.Cluster.ControlPlaneKubeconfig) == "" {
		fc.Cluster.ControlPlaneKubeconfig = kubeconfig
	}
	if err = conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return nil, err
	}
	return fc, nil
}

// StoreClusterCredential updates cloud-issued credentials without re-enabling
// membership, replacing runtime choices, or mutating a running boot snapshot.
func (s *Server) StoreClusterCredential(agent, accessToken, apiURL, token string, ca []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadRuntimeConfig()
	if err != nil {
		return err
	}
	if fc.AgentName != agent || fc.AccessToken != accessToken {
		return fmt.Errorf("host identity changed during credential refresh")
	}
	if fc.Cluster == nil || fc.Cluster.JoinsPeerPlane() {
		return fmt.Errorf("cloud cluster membership changed during credential refresh")
	}
	fc.Cluster.APIURL, fc.Cluster.Token, fc.Cluster.CA = apiURL, token, ca
	return conf.SaveFile(s.deps.ConfigPath, fc)
}

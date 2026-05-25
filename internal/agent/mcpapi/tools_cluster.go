package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

type setKubeconfigIn struct {
	Kubeconfig string `json:"kubeconfig" jsonschema:"Pasted YAML — k3s.yaml for dev, cloudbox-issued kubeconfig for production"`
	NodeName   string `json:"node_name,omitempty" jsonschema:"Optional override; empty defaults to AgentName"`
	Enable     bool   `json:"enable,omitempty" jsonschema:"Also flip Cluster.Enabled to true (one-shot paste + join)"`
}

type clusterOut struct {
	OK             bool                  `json:"ok"`
	Cluster        admincore.ClusterView `json:"cluster"`
	RestartPending bool                  `json:"restart_pending"`
}

type clearClusterOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

func (s *Server) registerClusterTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_set_kubeconfig",
		Description: "Parse a kubeconfig YAML and persist the apiserver URL + bearer + CA into fc.Cluster. Pass enable=true to also flip the join switch.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in setKubeconfigIn) (*mcp.CallToolResult, clusterOut, error) {
		res, err := s.core.SetKubeconfig(admincore.KubeconfigParams{
			Kubeconfig: in.Kubeconfig,
			NodeName:   in.NodeName,
			Enable:     in.Enable,
		})
		if err != nil {
			return apiErrResult[clusterOut](err)
		}
		return nil, clusterOut{OK: res.OK, Cluster: res.Cluster, RestartPending: res.RestartPending}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_clear_kubeconfig",
		Description: "Clear the cluster credentials and the Enabled flag (i.e. leave the cluster).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.ClearKubeconfig()
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}

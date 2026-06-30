package admincore

import "context"

// Shard trigger/status are the four-surface seam over the libp2p mesh shard
// control plane: tell a peer to LEAD a shard for a model (no ssh — the message
// rides the mesh), and read a node's (local or a peer's) shard readiness. The
// daemon threads in the closures (capturing the shard.Manager + the cloudbox
// host→peer-id resolution); admincore stays independent of the shard package.

const shardOffMsg = "shard control plane is not enabled (needs a paired host with the mesh data plane + sharding on)"

// ShardTrigger tells <host> to LEAD a shard for <model> over the mesh.
func (s *Server) ShardTrigger(ctx context.Context, host, model string) error {
	if s.deps.ShardTrigger == nil {
		return badRequest("%s", shardOffMsg)
	}
	if host == "" || model == "" {
		return badRequest("host and model are required")
	}
	if err := s.deps.ShardTrigger(ctx, host, model); err != nil {
		if AsAPIError(err) != nil {
			return err
		}
		return upstream("%s", err.Error())
	}
	return nil
}

// ShardStatus returns the local node's (host == "") or a peer's shard readiness
// over the mesh.
func (s *Server) ShardStatus(ctx context.Context, host string) (any, error) {
	if s.deps.ShardStatus == nil {
		return nil, badRequest("%s", shardOffMsg)
	}
	rep, err := s.deps.ShardStatus(ctx, host)
	if err != nil {
		if AsAPIError(err) != nil {
			return nil, err
		}
		return nil, upstream("%s", err.Error())
	}
	return rep, nil
}

// ShardLog returns the local node's (host == "") or a peer's recent prima-rank
// shard logs over the mesh — the captured exit reason a crashed shard left
// behind, no ssh.
func (s *Server) ShardLog(ctx context.Context, host string) (string, error) {
	if s.deps.ShardLog == nil {
		return "", badRequest("%s", shardOffMsg)
	}
	text, err := s.deps.ShardLog(ctx, host)
	if err != nil {
		if AsAPIError(err) != nil {
			return "", err
		}
		return "", upstream("%s", err.Error())
	}
	return text, nil
}

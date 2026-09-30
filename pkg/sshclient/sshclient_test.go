package sshclient_test

import (
	"context"
	"net"
	"testing"

	"github.com/qiangli/outpost/pkg/sshclient"
)

// Compile-time public API gate: an external consumer can name the client and
// invoke the unattended primitives without importing an internal package.
func TestPublicAPI(t *testing.T) {
	var c *sshclient.Client
	_ = c
	_ = sshclient.Dial
	_ = (*sshclient.Client).Exec
	_ = (*sshclient.Client).SFTP
	_ = (*sshclient.Client).DirectTCPIP
	_ = (*sshclient.Client).LocalForward
	var _ func(context.Context, net.Listener, string, int) error = c.LocalForward
}

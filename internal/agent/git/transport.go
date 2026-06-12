package git

import (
	"context"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// go-git's stock "file" transport shells out to the git-upload-pack /
// git-receive-pack binaries — exactly the system-git dependency this
// package exists to avoid. Replace it with go-git's in-process server
// so clone/fetch/pull/push against local filesystem paths stay pure Go
// (Windows hosts with no git installed included).
//
// The replacement is process-wide via client.InstallProtocol, but this
// package is the only go-git network user in outpost, so nothing else
// observes the swap.
func init() {
	loader := dotGitLoader{base: osfs.New("")}
	client.InstallProtocol("file", &localTransport{
		inner:  server.NewClient(loader),
		loader: loader,
	})
}

// dotGitLoader resolves a transport endpoint path to a repository
// storer. Unlike server.DefaultLoader it serves non-bare repositories
// too: when the path has a .git subdirectory it chroots into it, so a
// normal worktree clone can be used as a remote.
type dotGitLoader struct{ base billy.Filesystem }

func (l dotGitLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	fs, err := l.base.Chroot(ep.Path)
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(".git"); err == nil {
		if fs, err = fs.Chroot(".git"); err != nil {
			return nil, err
		}
	}
	if _, err := fs.Stat("config"); err != nil {
		return nil, transport.ErrRepositoryNotFound
	}
	return filesystem.NewStorage(fs, cache.NewObjectLRUDefault()), nil
}

// localTransport wraps the in-process server to fix one negotiation
// gap: go-git's server errors with "object not found" when the client
// advertises a "have" the server doesn't know (a local-only commit —
// e.g. fetching into a repo that is ahead of or diverged from the
// remote). Real git servers ignore unknown haves; do the same by
// filtering them against the remote's object store before delegating.
type localTransport struct {
	inner  transport.Transport
	loader server.Loader
}

func (t *localTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	s, err := t.inner.NewUploadPackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	sto, err := t.loader.Load(ep)
	if err != nil {
		return nil, err
	}
	return &havesFilterSession{UploadPackSession: s, storer: sto}, nil
}

func (t *localTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return t.inner.NewReceivePackSession(ep, auth)
}

type havesFilterSession struct {
	transport.UploadPackSession
	storer storer.Storer
}

func (s *havesFilterSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	known := req.Haves[:0]
	for _, h := range req.Haves {
		if err := s.storer.HasEncodedObject(h); err == nil {
			known = append(known, h)
		}
	}
	req.Haves = known
	return s.UploadPackSession.UploadPack(ctx, req)
}

// InstalledFileTransportIsPureGo reports whether the "file" protocol
// currently resolves to this package's in-process transport. go-git's
// stock file transport execs git-upload-pack — the system-git
// dependency this package must never have. Tests assert on this so a
// future go-git upgrade or init-order change can't silently restore
// the shell-out path.
func InstalledFileTransportIsPureGo() bool {
	_, ok := client.Protocols["file"].(*localTransport)
	return ok
}

// ensure the interface stays satisfied as go-git evolves.
var _ transport.Transport = (*localTransport)(nil)

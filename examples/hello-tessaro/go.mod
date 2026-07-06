// hello-tessaro is a reference cooperative-app module. It consumes the shared
// cooperative-app SDK, github.com/qiangli/coreutils/pkg/coopauth, exactly the
// way a real third-party app would — the identity + admin-allowlist model lives
// in ONE place (coopauth) so a security fix there reaches every app at once.
// Inside the dhnt umbrella the SDK resolves via the sibling replace below; a
// standalone copy uses the published module once the dev-kit is released.
// It still has its own go.mod so outpost `go build ./...` skips this subtree.
module github.com/qiangli/outpost/examples/hello-tessaro

go 1.26.4

require github.com/qiangli/coreutils v0.0.0

replace github.com/qiangli/coreutils => ../../../coreutils

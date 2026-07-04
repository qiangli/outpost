// hello-tessaro is a standalone reference module. It is intentionally
// stdlib-only (no third-party deps) so `go build`/`go test` are hermetic and
// need no network. It has its own go.mod so it is NOT part of the outpost
// module's dependency graph (outpost `go build ./...` skips this subtree).
module github.com/qiangli/outpost/examples/hello-tessaro

go 1.26

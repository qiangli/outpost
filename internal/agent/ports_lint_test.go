package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoHardcodedServicePorts fails when a non-test .go file carries a
// literal named-service port outside the file that declares that port's
// default. A named-service port is one a unit declaration (or the config
// default resolver it mirrors) owns: the Ollama defaults, the tunnel and
// apiserver defaults, and the fleet baseline's seeded unit ports. Two
// copies of such a number drift apart silently — the configured port
// moves and some caller keeps dialing the old one — which is exactly
// what this check exists to prevent.
//
// Flagged shapes (comments and prose are deliberately not scanned — a
// doc string SAYING "default 7000" is documentation, not a dial):
//
//   - an integer literal equal to a watched port (e.g. `Port: 11434`)
//   - a string literal that is entirely a host:port / :port / URL
//     address whose port is watched (e.g. "http://127.0.0.1:11434")
//
// To make a new port watched, add it to watchedPorts below. To make a
// new file a declaring file, add it to declaringFiles with the constant
// it owns — the allowlist is exactly the declaring files, nothing else.
func TestNoHardcodedServicePorts(t *testing.T) {
	root := repoRoot(t)
	var bad []string
	for _, hit := range scanPorts(root) {
		rel := hit.path
		if _, ok := declaringFiles[rel]; ok {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s:%d: literal %s port %d — resolve it from the unit/configured URL or the declaring constant (see internal/agent/ports_lint_test.go)", rel, hit.line, watchedPorts[hit.port], hit.port))
	}
	if len(bad) > 0 {
		t.Errorf("hardcoded service ports outside their declaring files:\n\t%s", strings.Join(bad, "\n\t"))
	}
}

// watchedPorts are the named-service ports: the story's five (Ollama
// 11434, the bashy engine 11435, the matrix tunnel 7000, the hosted
// apiserver 16443, the fleet apps unit 22749), every other default an
// outpost unit or its mirrored config resolver declares, and the fleet
// baseline's seeded unit ports (declared in yoke's
// pkg/fleet/baseline/services — any outpost occurrence of those is a
// cross-repo copy and thus a violation by definition).
var watchedPorts = map[int]string{
	11434: "Ollama",
	11435: "bashy Ollama engine",
	11436: "LAN inference",
	7000:  "matrix tunnel",
	6443:  "k8s apiserver visitor",
	16443: "hosted control-plane apiserver",
	22749: "fleet apps",
	8637:  "fleet meet",
	24556: "fleet llm door",
	31880: "fleet loom",
	31881: "fleet loom ssh",
	31890: "peer storage name",
	5000:  "fleet zot",
	8333:  "fleet seaweedfs",
	51515: "fleet kopia",
	18466: "fleet headlamp",
	31415: "observability stack http",
	31416: "observability stack grpc",
}

// declaringFiles is the exhaustive allowlist: the ONE file per port
// where the default is declared. Yoke-baseline ports have no outpost
// declaring file, so they are absent here on purpose.
var declaringFiles = map[string]string{
	"cmd/outpost/capability_laninference.go":  "DefaultLANInferencePort",
	"internal/agent/conf/conf.go":             "DefaultServerPort",
	"internal/agent/conf/file.go":             "DefaultTunnelBindPort / DefaultK8sAPIPort / DefaultControlPlaneAPIAddr",
	"internal/agent/headlamp/headlamp.go":     "DefaultLocalPort",
	"internal/agent/ollama/bashy.go":          "BashyManagedPort",
	"internal/agent/ollama/default.go":        "DefaultURL / DefaultPort",
	"internal/agent/otel/detect.go":           "DefaultProxyURL",
	"internal/agent/storeroute/storeroute.go": "DefaultPort",
	"internal/agent/runtime/server.go":        "DefaultControlPlaneAPIPort / controlPlaneContainerAPIPort",
}

type portHit struct {
	path string
	line int
	port int
}

// addrPort matches a string literal that is entirely an address —
// "host:port", ":port", optionally under http(s)://, optionally with a
// path — so prose like "default 7000" or example commands never match.
var addrPort = regexp.MustCompile(`^((?:https?://)?(?:\[[^/\]]+\]|[^/?:]+)?):(\d+)(?:/.*)?$`)

func scanPorts(root string) []portHit {
	var hits []portHit
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			hits = append(hits, portHit{path: filepath.ToSlash(rel), line: fset.Position(f.Pos()).Line, port: 0})
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			switch lit.Kind {
			case token.INT:
				v, _ := strconv.Atoi(lit.Value)
				if _, watched := watchedPorts[v]; watched {
					hits = append(hits, portHit{filepath.ToSlash(rel), fset.Position(lit.Pos()).Line, v})
				}
			case token.STRING:
				s, _ := strconv.Unquote(lit.Value)
				m := addrPort.FindStringSubmatch(strings.TrimRight(s, "/"))
				if m == nil {
					return true
				}
				v, _ := strconv.Atoi(m[2])
				if _, watched := watchedPorts[v]; watched {
					hits = append(hits, portHit{filepath.ToSlash(rel), fset.Position(lit.Pos()).Line, v})
				}
			}
			return true
		})
		return nil
	})
	return hits
}

// repoRoot walks up from the test's working directory (the package dir)
// to the module root — the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test package")
		}
		dir = parent
	}
}

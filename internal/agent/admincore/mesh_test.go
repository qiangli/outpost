package admincore

import (
	"path/filepath"
	"testing"
)

type fakeMeshFwd struct {
	exposed   map[string]string
	unexposed []string
}

func (f *fakeMeshFwd) Expose(service, addr string) error {
	if f.exposed == nil {
		f.exposed = map[string]string{}
	}
	f.exposed[service] = addr
	return nil
}
func (f *fakeMeshFwd) Unexpose(service string) error {
	f.unexposed = append(f.unexposed, service)
	return nil
}
func (f *fakeMeshFwd) Listen(peerID, service, localAddr string) (string, error) { return "", nil }
func (f *fakeMeshFwd) CloseListen(addr string) error                            { return nil }
func (f *fakeMeshFwd) Forwards() MeshForwardView                                { return MeshForwardView{} }

// The wrap harness: upsert persists + exposes live; upsert updates in place;
// delete persists removal + unexposes live.
func TestMeshServicePersistAndApply(t *testing.T) {
	fake := &fakeMeshFwd{}
	s, err := New(Deps{
		ConfigPath:  filepath.Join(t.TempDir(), "agent.json"),
		MeshForward: fake,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.MeshServiceUpsert("git", "127.0.0.1:3000"); err != nil {
		t.Fatal(err)
	}
	svcs, _ := s.MeshServices()
	if len(svcs) != 1 || svcs[0].Name != "git" || svcs[0].Addr != "127.0.0.1:3000" {
		t.Fatalf("persisted services=%+v", svcs)
	}
	if fake.exposed["git"] != "127.0.0.1:3000" {
		t.Fatalf("expected live Expose, got %+v", fake.exposed)
	}

	// upsert updates in place (no duplicate)
	if err := s.MeshServiceUpsert("git", "127.0.0.1:4000"); err != nil {
		t.Fatal(err)
	}
	if svcs, _ = s.MeshServices(); len(svcs) != 1 || svcs[0].Addr != "127.0.0.1:4000" {
		t.Fatalf("upsert should update in place: %+v", svcs)
	}

	// delete persists + unexposes
	if err := s.MeshServiceDelete("git"); err != nil {
		t.Fatal(err)
	}
	if svcs, _ = s.MeshServices(); len(svcs) != 0 {
		t.Fatalf("expected empty after delete: %+v", svcs)
	}
	if len(fake.unexposed) != 1 || fake.unexposed[0] != "git" {
		t.Fatalf("expected live Unexpose, got %+v", fake.unexposed)
	}

	if err := s.MeshServiceUpsert("", "x"); err == nil {
		t.Fatal("empty name should error")
	}
}

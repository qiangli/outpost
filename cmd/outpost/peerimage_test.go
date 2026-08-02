package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/peerimage"
)

type fakePeerImageOps struct {
	calls []string

	pubRes peerimage.Publication
	pubErr error

	pubsRes []peerimage.Publication
	pubsErr error

	meshRes peerimage.ResolveResult
	meshErr error

	ensRes peerimage.EnsureResult
	ensErr error

	repRes peerimage.Report
	repErr error
}

func (f *fakePeerImageOps) Publish(_ context.Context, name, body string) (peerimage.Publication, error) {
	f.calls = append(f.calls, "Publish:"+name)
	return f.pubRes, f.pubErr
}

func (f *fakePeerImageOps) Publications() ([]peerimage.Publication, error) {
	f.calls = append(f.calls, "Publications")
	return f.pubsRes, f.pubsErr
}

func (f *fakePeerImageOps) MeshResolve(_ context.Context, service string, minimum int) (peerimage.ResolveResult, error) {
	f.calls = append(f.calls, "MeshResolve:"+service)
	return f.meshRes, f.meshErr
}

func (f *fakePeerImageOps) Ensure(_ context.Context, name string) (peerimage.EnsureResult, error) {
	f.calls = append(f.calls, "Ensure:"+name)
	return f.ensRes, f.ensErr
}

func (f *fakePeerImageOps) Report(_ context.Context, ch peerimage.Challenge) (peerimage.Report, error) {
	f.calls = append(f.calls, "Report:"+ch.Node+":"+ch.Ref)
	return f.repRes, f.repErr
}

func TestPeerImageParity_VerbsAndToolNames(t *testing.T) {
	wantVerbs := []admincore.PeerImageVerb{
		admincore.PeerImageVerbPublish,
		admincore.PeerImageVerbMeshResolve,
		admincore.PeerImageVerbEnsure,
		admincore.PeerImageVerbReport,
	}

	if len(admincore.PeerImageVerbs) != len(wantVerbs) {
		t.Fatalf("PeerImageVerbs len = %d, want %d", len(admincore.PeerImageVerbs), len(wantVerbs))
	}
	for i, v := range wantVerbs {
		if admincore.PeerImageVerbs[i] != v {
			t.Errorf("PeerImageVerbs[%d] = %s, want %s", i, admincore.PeerImageVerbs[i], v)
		}
		toolName, ok := admincore.PeerImageToolNames[v]
		if !ok || toolName == "" {
			t.Errorf("PeerImageToolNames missing entry for verb %s", v)
		}
	}
}

func TestPeerImageParity_AdmincoreOpsDirectWiring(t *testing.T) {
	ctx := context.Background()
	fake := &fakePeerImageOps{
		pubRes:  peerimage.Publication{Name: "app1", Ref: "localhost/app1:latest"},
		pubsRes: []peerimage.Publication{{Name: "app1"}},
		meshRes: peerimage.ResolveResult{Service: "recipes", Distinct: 1},
		ensRes:  peerimage.EnsureResult{Node: "node1", Recipe: "app1", Built: true},
		repRes:  peerimage.Report{Node: "node1", Ref: "localhost/app1:latest", State: peerimage.StateResident},
	}

	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	srv, err := admincore.New(admincore.Deps{
		ConfigPath: cfgPath,
		PeerImage:  fake,
	})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}

	// 1. Publish
	pub, err := srv.PeerImagePublish(ctx, "app1", "name: app1\nlocal_ref: localhost/app1:latest\ncontext_type: local\ncontext_path: .\ndockerfile: Dockerfile\n")
	if err != nil {
		t.Fatalf("PeerImagePublish: %v", err)
	}
	if pub.Name != "app1" {
		t.Errorf("pub.Name = %q, want app1", pub.Name)
	}

	// 2. Publications
	pubs, err := srv.PeerImagePublications()
	if err != nil {
		t.Fatalf("PeerImagePublications: %v", err)
	}
	if len(pubs) != 1 {
		t.Errorf("pubs len = %d, want 1", len(pubs))
	}

	// 3. MeshResolve
	mesh, err := srv.PeerImageMeshResolve(ctx, "recipes", 1)
	if err != nil {
		t.Fatalf("PeerImageMeshResolve: %v", err)
	}
	if mesh.Service != "recipes" {
		t.Errorf("mesh.Service = %q, want recipes", mesh.Service)
	}

	// 4. Ensure
	ens, err := srv.PeerImageEnsure(ctx, "app1")
	if err != nil {
		t.Fatalf("PeerImageEnsure: %v", err)
	}
	if !ens.Built {
		t.Errorf("ens.Built = false, want true")
	}

	// 5. Report
	const validSha256 = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	rep, err := srv.PeerImageReport(ctx, peerimage.Challenge{
		Node:   "node1",
		Ref:    "localhost/app1:latest",
		Recipe: validSha256,
		Nonce:  "nonce123",
	})
	if err != nil {
		t.Fatalf("PeerImageReport: %v", err)
	}
	if rep.State != peerimage.StateResident {
		t.Errorf("rep.State = %s, want resident", rep.State)
	}

	// Verify all 5 calls reached fakePeerImageOps
	wantCalls := []string{
		"Publish:app1",
		"Publications",
		"MeshResolve:recipes",
		"Ensure:app1",
		"Report:node1:localhost/app1:latest",
	}
	if len(fake.calls) != len(wantCalls) {
		t.Fatalf("fake.calls len = %d, want %d. Got: %v", len(fake.calls), len(wantCalls), fake.calls)
	}
	for i, c := range wantCalls {
		if fake.calls[i] != c {
			t.Errorf("fake.calls[%d] = %q, want %q", i, fake.calls[i], c)
		}
	}
}

func TestPeerImageParity_DisabledReturnsBadRequest(t *testing.T) {
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	srv, err := admincore.New(admincore.Deps{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}

	_, err = srv.PeerImagePublish(ctx, "app1", "body")
	ae := admincore.AsAPIError(err)
	if ae == nil || ae.Status != 400 {
		t.Errorf("PeerImagePublish disabled err = %v (ae=%v), want HTTP 400 APIError", err, ae)
	}
}

func TestPeerImageParity_FileConfigDefaultService(t *testing.T) {
	fc := &conf.FileConfig{
		PeerImage: &conf.PeerImageConfig{
			Service: "",
		},
	}
	if fc.PeerImageServiceOrDefault() != conf.PeerImageDefaultService {
		t.Errorf("PeerImageServiceOrDefault() = %q, want %q", fc.PeerImageServiceOrDefault(), conf.PeerImageDefaultService)
	}

	fc.PeerImage.Service = "custom-recipes"
	if fc.PeerImageServiceOrDefault() != "custom-recipes" {
		t.Errorf("PeerImageServiceOrDefault() = %q, want custom-recipes", fc.PeerImageServiceOrDefault())
	}
}

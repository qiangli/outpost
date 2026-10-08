package main

import (
	"testing"
)

// Sprint 379 Y2: bashy's own engine inventory merges into the pool view.
func TestMergePoolModels(t *testing.T) {
	primary := []poolStatusModel{{Name: "host:1b", Size: 100}, {Name: "shared:7b", Size: 700}}
	extra := []poolStatusModel{{Name: "bashy:1b", Size: 200}, {Name: "shared:7b", Size: 777}, {Name: ""}}
	got := mergePoolModels(primary, extra)
	want := map[string]int64{"host:1b": 100, "bashy:1b": 200, "shared:7b": 700}
	if len(got) != len(want) {
		t.Fatalf("merged=%+v, want %d models", got, len(want))
	}
	for _, m := range got {
		if want[m.Name] != m.Size {
			t.Errorf("model %q size=%d, want %d (full=%v)", m.Name, m.Size, want[m.Name], got)
		}
	}
	// Sorted by name.
	for i := 1; i < len(got); i++ {
		if got[i-1].Name >= got[i].Name {
			t.Errorf("not sorted: %+v", got)
		}
	}
	// Empty extra returns the primary slice unchanged.
	if out := mergePoolModels(primary, nil); len(out) != len(primary) {
		t.Errorf("nil extra: got %+v, want primary unchanged", out)
	}
}

func TestSameBaseURL(t *testing.T) {
	if !sameBaseURL("http://127.0.0.1:11435", "http://127.0.0.1:11435/") {
		t.Error("trailing slash should not matter")
	}
	if sameBaseURL("http://127.0.0.1:11435", "http://127.0.0.1:11434") {
		t.Error("different ports must not compare equal")
	}
}

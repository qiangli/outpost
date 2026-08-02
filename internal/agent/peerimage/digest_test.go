package peerimage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidContentDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	if !ValidContentDigest(good) {
		t.Fatal("a well-formed sha256:<64 hex> was rejected")
	}
	for _, bad := range []string{
		"",
		strings.Repeat("a", 64),             // no prefix
		"sha256:" + strings.Repeat("a", 63), // short
		"sha256:" + strings.Repeat("a", 65), // long
		"sha256:" + strings.Repeat("z", 64), // non-hex
		"sha256:",                           // empty body
		"localhost/cluster/demo:v1",         // a ref, not a digest
	} {
		if ValidContentDigest(bad) {
			t.Errorf("invalid digest accepted: %q", bad)
		}
	}
}

func TestParseCtrImagesLs(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	ref := "localhost/cluster/demo:v1"
	out := "REF                            TYPE     DIGEST                                                                  SIZE      PLATFORMS   LABELS\n" +
		"localhost/cluster/demo:v1      application/vnd.oci.image.manifest.v1+json " + digest + " 12.3 MiB  linux/arm64 -\n"

	state, got, err := parseCtrImagesLs(out, ref)
	if err != nil || state != StateResident || got != digest {
		t.Fatalf("resident parse: state=%q digest=%q err=%v", state, got, err)
	}

	// Ref not listed → absent (the runtime answered; the image is not there).
	state, _, err = parseCtrImagesLs(out, "localhost/cluster/other:v9")
	if err != nil || state != StateAbsent {
		t.Fatalf("absent parse: state=%q err=%v", state, err)
	}

	// Ref listed but the digest column is malformed → unknown. NOT absent,
	// NOT resident with a garbage digest.
	bad := "REF                       TYPE     DIGEST     SIZE  PLATFORMS  LABELS\n" +
		"localhost/cluster/demo:v1 manifest not-a-digest 1MiB linux/arm64 -\n"
	state, got, err = parseCtrImagesLs(bad, ref)
	if err != nil || state != StateUnknown || got != "" {
		t.Fatalf("unreadable digest: state=%q digest=%q err=%v", state, got, err)
	}

	// A row with too few columns → unknown.
	short := "localhost/cluster/demo:v1\n"
	state, _, err = parseCtrImagesLs(short, ref)
	if err != nil || state != StateUnknown {
		t.Fatalf("short row: state=%q err=%v", state, err)
	}

	// Empty output entirely → absent (the store answered and listed nothing).
	state, _, err = parseCtrImagesLs("", ref)
	if err != nil || state != StateAbsent {
		t.Fatalf("empty listing: state=%q err=%v", state, err)
	}
}

func TestCtrRuntime_ExecFailureIsUnknownNotAbsent(t *testing.T) {
	rt := CtrRuntime{
		BashyBin:  "/bin/false",
		Container: "n-runtime",
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("exec boom")
		},
	}
	state, _, err := rt.ResidentDigest(context.Background(), "localhost/cluster/demo:v1")
	if err == nil {
		t.Fatal("an exec failure produced a clean answer")
	}
	if state != StateUnknown {
		t.Fatalf("exec failure = %q, want unknown (never absent)", state)
	}
}

func TestCtrRuntime_BashyPathResolutionFailureIsUnknown(t *testing.T) {
	rt := CtrRuntime{
		BashyPath: func(context.Context) (string, error) { return "", errors.New("no toolchain") },
		Container: "n-runtime",
		run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("exec ran despite the toolchain being unresolvable")
			return nil, nil
		},
	}
	state, _, err := rt.ResidentDigest(context.Background(), "localhost/cluster/demo:v1")
	if err == nil || state != StateUnknown {
		t.Fatalf("unresolvable toolchain: state=%q err=%v", state, err)
	}
}

func TestCtrRuntime_BashyPathTakesPrecedence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	ref := "localhost/cluster/demo:v1"
	var gotBin string
	rt := CtrRuntime{
		BashyBin:  "/should/not/be/used",
		Container: "n-runtime",
		BashyPath: func(context.Context) (string, error) { return "/resolved/bashy", nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotBin = name
			return []byte("REF TYPE DIGEST SIZE PLATFORMS LABELS\n" + ref + " manifest " + digest + " 1MiB linux/arm64 -\n"), nil
		},
	}
	state, got, err := rt.ResidentDigest(context.Background(), ref)
	if err != nil || state != StateResident || got != digest {
		t.Fatalf("resident via BashyPath: state=%q digest=%q err=%v", state, got, err)
	}
	if gotBin != "/resolved/bashy" {
		t.Fatalf("BashyPath did not take precedence: %q", gotBin)
	}
}

func TestCtrRuntime_RequiresRefAndContainer(t *testing.T) {
	rt := CtrRuntime{Container: "n-runtime", run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}}
	if _, _, err := rt.ResidentDigest(context.Background(), "  "); err == nil {
		t.Fatal("an empty ref was queried")
	}
	rt2 := CtrRuntime{run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}}
	if _, _, err := rt2.ResidentDigest(context.Background(), "localhost/cluster/demo:v1"); err == nil {
		t.Fatal("an empty container was queried")
	}
}

package main

import "testing"

type fakeUserServiceManager struct {
	installs   int
	uninstalls int
	err        error
}

func (f *fakeUserServiceManager) Install() error {
	f.installs++
	return f.err
}

func (f *fakeUserServiceManager) Uninstall() error {
	f.uninstalls++
	return f.err
}

func TestActivatePairedService(t *testing.T) {
	original := pairingServiceManager
	fake := &fakeUserServiceManager{}
	pairingServiceManager = fake
	t.Cleanup(func() { pairingServiceManager = original })

	for _, tt := range []struct {
		name, running, noService string
		want                     int
	}{
		{"dormant host installs service", "", "", 1},
		{"running daemon keeps restart path", "yes", "", 0},
		{"no-service escape hatch", "", "yes", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake.installs = 0
			if err := activatePairedService(tt.running != "", tt.noService != ""); err != nil {
				t.Fatal(err)
			}
			if fake.installs != tt.want {
				t.Fatalf("installs = %d, want %d", fake.installs, tt.want)
			}
		})
	}
}

func TestPairingState(t *testing.T) {
	if got := pairingState(""); got != "dormant (unpaired)" {
		t.Fatalf("unpaired state = %q", got)
	}
	if got := pairingState("moss"); got != "active (paired as moss)" {
		t.Fatalf("paired state = %q", got)
	}
}

func TestDeactivatePairedService(t *testing.T) {
	original := pairingServiceManager
	fake := &fakeUserServiceManager{}
	pairingServiceManager = fake
	t.Cleanup(func() { pairingServiceManager = original })
	if err := deactivatePairedService(); err != nil {
		t.Fatal(err)
	}
	if fake.uninstalls != 1 {
		t.Fatalf("uninstalls = %d, want 1", fake.uninstalls)
	}
}

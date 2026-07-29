package sysinfo

import "testing"

func TestGPUKindFromVendor(t *testing.T) {
	// Vendor strings as the platform probes actually report them.
	cases := []struct {
		name   string
		vendor string
		want   string
	}{
		// The regression this table exists for: "Intel Corporation"
		// contains "ati" (corporATIon), so an amd arm matching a bare
		// "ati" ahead of the intel arm labelled every Intel GPU as AMD.
		{"lspci intel", "Intel Corporation", GPUKindIntel},
		{"windows intel", "Intel Corporation", GPUKindIntel},
		{"lspci nvidia", "NVIDIA Corporation", GPUKindNVIDIA},
		{"windows nvidia", "NVIDIA", GPUKindNVIDIA},

		// AMD in both the post- and pre-acquisition spellings.
		{"lspci amd", "Advanced Micro Devices, Inc. [AMD/ATI]", GPUKindAMD},
		{"legacy ati", "ATI Technologies Inc", GPUKindAMD},
		{"windows amd", "Advanced Micro Devices, Inc.", GPUKindAMD},

		{"apple silicon", "sppci_vendor_Apple", GPUKindApple},
		{"apple plain", "Apple", GPUKindApple},

		// Unknown vendors classify as "" rather than guessing — the
		// model is still reported by the caller. "Matrox Electronic
		// Systems Ltd." is the useful negative: another vendor whose
		// real-world string is corporation-shaped.
		{"matrox", "Matrox Electronic Systems Ltd.", ""},
		{"empty", "", ""},
		{"unknown", "Some Unknown Vendor Corporation", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gpuKindFromVendor(c.vendor); got != c.want {
				t.Errorf("gpuKindFromVendor(%q) = %q, want %q", c.vendor, got, c.want)
			}
		})
	}
}

func TestGPUKindFromVendorIsCaseInsensitive(t *testing.T) {
	for _, v := range []string{"INTEL CORPORATION", "intel corporation", "Intel Corporation"} {
		if got := gpuKindFromVendor(v); got != GPUKindIntel {
			t.Errorf("gpuKindFromVendor(%q) = %q, want %q", v, got, GPUKindIntel)
		}
	}
}

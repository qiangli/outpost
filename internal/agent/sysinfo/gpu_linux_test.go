//go:build linux

package sysinfo

import (
	"reflect"
	"testing"
)

func TestLspciSplit(t *testing.T) {
	// Real-world lspci -mm output samples. We need fields[0..3]:
	// slot, class, vendor, model. Trailing per-device bits are
	// allowed but not asserted.
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "intel uhd",
			in:   `00:02.0 "VGA compatible controller" "Intel Corporation" "UHD Graphics 630" -r02 "Dell" "Device 0826"`,
			want: []string{"00:02.0", "VGA compatible controller", "Intel Corporation", "UHD Graphics 630"},
		},
		{
			name: "nvidia 3d",
			in:   `01:00.0 "3D controller" "NVIDIA Corporation" "GA106 [GeForce RTX 3060]" -ra1 "Gigabyte" "Device 4040"`,
			want: []string{"01:00.0", "3D controller", "NVIDIA Corporation", "GA106 [GeForce RTX 3060]"},
		},
		{
			name: "no quotes (malformed)",
			in:   `something without quotes`,
			want: []string{"something", "without", "quotes"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lspciSplit(c.in)
			if len(got) < len(c.want) {
				t.Fatalf("got %d fields, want >= %d: %v", len(got), len(c.want), got)
			}
			if !reflect.DeepEqual(got[:len(c.want)], c.want) {
				t.Errorf("lspciSplit = %v, want %v as prefix", got, c.want)
			}
		})
	}
}

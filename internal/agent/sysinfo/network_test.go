package sysinfo

import (
	"net"
	"reflect"
	"testing"
)

func TestPrivateIPs(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("192.168.1.20")},
		&net.IPAddr{IP: net.ParseIP("10.0.0.20")},
		&net.IPNet{IP: net.ParseIP("172.16.0.20")},
		&net.IPAddr{IP: net.ParseIP("fd00::20")},
		&net.IPAddr{IP: net.ParseIP("127.0.0.1")},
		&net.IPAddr{IP: net.ParseIP("::1")},
		&net.IPAddr{IP: net.ParseIP("169.254.1.20")},
		&net.IPAddr{IP: net.ParseIP("fe80::20"), Zone: "eth0"},
		&net.IPAddr{IP: net.ParseIP("203.0.113.20")},
		&net.IPAddr{IP: net.ParseIP("0.0.0.0")},
		&net.IPAddr{},
		&net.UnixAddr{Name: "ignored"},
	}
	want := []string{"192.168.1.20", "10.0.0.20", "172.16.0.20", "fd00::20"}
	if got := privateIPs(addrs); !reflect.DeepEqual(got, want) {
		t.Fatalf("privateIPs = %v, want %v", got, want)
	}
	if got := privateIPs(nil); len(got) != 0 {
		t.Fatalf("empty interfaces returned %v", got)
	}
}

package heartbeat

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestDiskHeartbeatWire(t *testing.T) {
	const private = "fixture-identity"
	collect := func(distro, display string) diskVolume {
		before := parseDiskRows("wsl", distro, display, "r a 1 100 24\ns a go-mod 70\ns a logs 5\ns a otel 1\nend go-mod\nend logs\nend otel\n")
		after := parseDiskRows("wsl", distro, display, "r a 1 100 16\ns a go-mod 70\ns a logs 12\ns a otel 2\nend go-mod\nend logs\nend otel\n")
		if len(before) != 1 || len(after) != 1 {
			t.Fatal(before, after)
		}
		return after[0].report(before[0])
	}
	a := collect(private+"-1", "WSL Fixture-A")
	b := collect(private+"-2", "WSL Fixture-B")
	if a.ID == b.ID || a.DisplayName != "WSL Fixture-A /" || b.DisplayName != "WSL Fixture-B /" {
		t.Fatal(a, b)
	}
	want := diskReport{At: time.Now().UTC(), Volumes: []diskVolume{a, b}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/host/heartbeat" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("unauthenticated or incorrect endpoint")
		}
		var body struct {
			Disk diskReport `json:"disk"`
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
			return
		}
		if !reflect.DeepEqual(body.Disk, want) {
			t.Errorf("wire: %+v", body)
		}
		for _, v := range body.Disk.Volumes {
			if v.Used != 84 || v.Total != 100 || !reflect.DeepEqual(v.Growers, []grower{{"logs", 7}, {"otel", 1}}) || strings.Contains(v.ID, private) {
				t.Error(v)
			}
		}
	}))
	defer srv.Close()
	worker := New(Config{CloudboxBase: srv.URL, AgentName: "fixture", AccessToken: "fixture"})
	worker.disks.Store(&want)
	if e := worker.postOne(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestDiskUnavailableAndBounds(t *testing.T) {
	for _, raw := range []string{"", "r x 1 4 2", "r a 1 0 0", "r a 1 3 4", "v ab 4096 18446744073709551615 4"} {
		if got := parseDiskRows("wsl", "fixture", "WSL Fixture", raw); len(got) != 0 {
			t.Fatal(raw, got)
		}
	}
	m := parseDiskRows("wsl", "fixture", "WSL Fixture", "r a 1 100 50\ns a private 9\ns a logs -1\n")
	if len(m) != 1 || len(m[0].sizes) != 0 {
		t.Fatal(m)
	}
	data := utf16.Encode([]rune("\ufeffFixture-A\r\nFixture-B\r\n"))
	raw := make([]byte, len(data)*2)
	for i, w := range data {
		binary.LittleEndian.PutUint16(raw[2*i:], w)
	}
	if names := runningDistros(raw); len(names) != 2 || !names["fixture-a"] || names["stopped"] {
		t.Fatal(names)
	}
	var buf probeBuffer
	buf.Write(make([]byte, 20000))
	if !buf.overflow || buf.buf.Len() > 16<<10 {
		t.Fatal("probe output unbounded")
	}
}

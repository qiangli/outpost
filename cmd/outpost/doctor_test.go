package main

import "testing"

func TestParseKV(t *testing.T) {
	m := parseKV("REG=1\r\nSTATE=Ready\n\nRUNAS=EXAMPLE\\Dev\nnot a kv line\n=noval\nLOGON=S4U\n")
	for k, want := range map[string]string{"REG": "1", "STATE": "Ready", "RUNAS": `EXAMPLE\Dev`, "LOGON": "S4U"} {
		if m[k] != want {
			t.Errorf("parseKV[%q] = %q, want %q", k, m[k], want)
		}
	}
	if _, ok := m["not a kv line"]; ok {
		t.Error("non-kv line should be ignored")
	}
	if _, ok := m[""]; ok {
		t.Error("leading-= line should be ignored (no key)")
	}
}

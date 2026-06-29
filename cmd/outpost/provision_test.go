package main

import "testing"

func TestParseModelRef(t *testing.T) {
	cases := []struct{ in, reg, ns, model, tag string }{
		{"llama3.3:70b", "registry.ollama.ai", "library", "llama3.3", "70b"},
		{"llama3.3", "registry.ollama.ai", "library", "llama3.3", "latest"},
		{"user/model:tag", "registry.ollama.ai", "user", "model", "tag"},
		{"hf.co/org/repo:Q4_K_M", "hf.co", "org", "repo", "Q4_K_M"},
	}
	for _, c := range cases {
		reg, ns, model, tag := parseModelRef(c.in)
		if reg != c.reg || ns != c.ns || model != c.model || tag != c.tag {
			t.Errorf("%q → %s/%s/%s:%s, want %s/%s/%s:%s",
				c.in, reg, ns, model, tag, c.reg, c.ns, c.model, c.tag)
		}
	}
}

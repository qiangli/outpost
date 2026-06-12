package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// splitConfigKey parses "section.name" or "section.subsection.name"
// (subsection may itself contain dots, e.g. branch.feat.x.merge —
// everything between the first and last dot is the subsection, same
// rule git uses).
func splitConfigKey(key string) (section, subsection, name string, err error) {
	first := strings.Index(key, ".")
	last := strings.LastIndex(key, ".")
	if first < 1 || last == len(key)-1 {
		return "", "", "", fmt.Errorf("config: invalid key %q (want section.name)", key)
	}
	section = key[:first]
	name = key[last+1:]
	if first != last {
		subsection = key[first+1 : last]
	}
	return section, subsection, name, nil
}

func rawLookup(cfg *config.Config, section, subsection, name string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	s := cfg.Raw.Section(section)
	if subsection != "" {
		ss := s.Subsection(subsection)
		if ss.HasOption(name) {
			return ss.Option(name), true
		}
		return "", false
	}
	if s.HasOption(name) {
		return s.Option(name), true
	}
	return "", false
}

// globalConfigPath returns the file global writes go to: the first
// existing global-scope config path, else ~/.gitconfig.
func globalConfigPath() (string, error) {
	paths, err := config.Paths(config.GlobalScope)
	if err != nil || len(paths) == 0 {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", fmt.Errorf("config: no global config path: %v", herr)
		}
		return filepath.Join(home, ".gitconfig"), nil
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return paths[0], nil
}

// ConfigGet reads a config key with git's precedence: repo-local value
// wins over global. found=false (with nil error) means the key is not
// set anywhere — `git config <key>` exits 1 for that case and the CLI
// mirrors it.
func ConfigGet(repoPath, key string) (value string, found bool, err error) {
	section, subsection, name, err := splitConfigKey(key)
	if err != nil {
		return "", false, err
	}
	if repoPath == "" {
		repoPath = "."
	}
	if r, rerr := gogit.PlainOpen(repoPath); rerr == nil {
		if cfg, cerr := r.Config(); cerr == nil {
			if v, ok := rawLookup(cfg, section, subsection, name); ok {
				return v, true, nil
			}
		}
	}
	if cfg, cerr := config.LoadConfig(config.GlobalScope); cerr == nil {
		if v, ok := rawLookup(cfg, section, subsection, name); ok {
			return v, true, nil
		}
	}
	return "", false, nil
}

// applyBranchOption mutates the typed Branches map (go-git regenerates
// the raw branch section from it on Marshal).
func applyBranchOption(cfg *config.Config, branch, name, value string, unset bool) error {
	b := cfg.Branches[branch]
	if b == nil {
		if unset {
			return nil
		}
		b = &config.Branch{Name: branch}
		if cfg.Branches == nil {
			cfg.Branches = map[string]*config.Branch{}
		}
		cfg.Branches[branch] = b
	}
	if unset {
		value = ""
	}
	switch name {
	case "remote":
		b.Remote = value
	case "merge":
		b.Merge = plumbing.ReferenceName(value)
	case "rebase":
		b.Rebase = value
	case "description":
		b.Description = value
	default:
		return fmt.Errorf("config: branch option %q not supported (remote, merge, rebase, description)", name)
	}
	if b.Remote == "" && b.Merge == "" && b.Rebase == "" && b.Description == "" {
		delete(cfg.Branches, branch)
	}
	return nil
}

// ConfigSet writes (or with unset, removes) a config key. global=false
// targets the repository's .git/config; global=true targets the user's
// global config file (~/.gitconfig or the XDG equivalent), created if
// missing.
func ConfigSet(repoPath, key, value string, global, unset bool) (*Result, error) {
	section, subsection, name, err := splitConfigKey(key)
	if err != nil {
		return nil, err
	}
	if !unset && value == "" {
		return nil, errors.New("config: value required (or pass --unset)")
	}

	apply := func(cfg *config.Config) error {
		// Sections go-git mirrors into typed maps get regenerated from
		// those maps on Marshal — a raw-only write would be silently
		// dropped. branch.* is useful enough (setting an upstream for
		// pull) to route through the typed map; the others have richer
		// invariants and no compelling config-verb use case.
		switch section {
		case "branch":
			if subsection == "" {
				return fmt.Errorf("config: %q — branch keys are branch.<name>.<option>", key)
			}
			return applyBranchOption(cfg, subsection, name, value, unset)
		case "remote", "submodule", "url":
			return fmt.Errorf("config: %s.* keys are not supported by outpost git config", section)
		}
		s := cfg.Raw.Section(section)
		if subsection != "" {
			ss := s.Subsection(subsection)
			if unset {
				ss.RemoveOption(name)
			} else {
				ss.SetOption(name, value)
			}
		} else {
			if unset {
				s.RemoveOption(name)
			} else {
				s.SetOption(name, value)
			}
		}
		// Mirror into the typed fields go-git itself consults (commit
		// signatures read cfg.User, and Marshal re-derives the raw user
		// section from the typed struct).
		if section == "user" && subsection == "" {
			switch name {
			case "name":
				if unset {
					cfg.User.Name = ""
				} else {
					cfg.User.Name = value
				}
			case "email":
				if unset {
					cfg.User.Email = ""
				} else {
					cfg.User.Email = value
				}
			}
		}
		return nil
	}

	verb := "Set"
	if unset {
		verb = "Unset"
	}

	if global {
		path, err := globalConfigPath()
		if err != nil {
			return nil, err
		}
		cfg := config.NewConfig()
		if data, err := os.ReadFile(path); err == nil {
			if cfg, err = config.ReadConfig(strings.NewReader(string(data))); err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
		}
		if err := apply(cfg); err != nil {
			return nil, err
		}
		out, err := cfg.Marshal()
		if err != nil {
			return nil, fmt.Errorf("config: marshal: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return nil, fmt.Errorf("config: write %s: %w", path, err)
		}
		return &Result{Success: true, Message: fmt.Sprintf("%s %s (global: %s)", verb, key, path)}, nil
	}

	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	cfg, err := r.Config()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := apply(cfg); err != nil {
		return nil, err
	}
	if err := r.SetConfig(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("%s %s", verb, key)}, nil
}

package main

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The creator's OMP model list is not a compiled-in guess about what a user
// runs: it is read from the models that host's omp is actually configured to
// use. omp assigns a model to each ROLE in ~/.omp/agent/config.yml — the named
// roles under `modelRoles` (default, smol, slow, task, vision, designer, plus
// any custom role) and the per-task-agent assignments under
// `task.agentModelOverrides`. Those selectors are exactly the models the user
// has authenticated, picked deliberately, and already runs work on, so they are
// the right candidate set for `omp --model`.
//
// Deliberately NOT read:
//
//   - `retry.fallbackChains` — recovery paths, keyed by role OR by model/provider,
//     whose values may be wildcards ("google/*") that are not launch selectors.
//   - `enabledModels` / `modelTags` — catalog filtering and labels, not role
//     assignments.
//
// The path mirrors syncOmpTheme's: ~/.omp/agent is omp's config dir on every
// host lasso drives (`omp config path`), and a host where omp has never run has
// no file to read — the compiled-in suggestions then stand in (hostHarnesses).

// ompConfigRoles is the slice of omp's config.yml that carries role→model
// assignments. Both fields decode as yaml.Node rather than map[string]string so
// the config's own key order survives: the roles a user put first are the ones
// the picker should offer first, which a Go map would shuffle on every request.
type ompConfigRoles struct {
	ModelRoles yaml.Node `yaml:"modelRoles"`
	Task       struct {
		AgentModelOverrides yaml.Node `yaml:"agentModelOverrides"`
	} `yaml:"task"`
}

// ompRoleModels extracts the deduplicated model selectors an omp config.yml
// assigns to roles, in config order. A file it can't parse yields nothing
// rather than a partial list — the caller then falls back to the compiled-in
// suggestions.
func ompRoleModels(data []byte) []string {
	var cfg ompConfigRoles
	if yaml.Unmarshal(data, &cfg) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range []*yaml.Node{&cfg.ModelRoles, &cfg.Task.AgentModelOverrides} {
		if n.Kind != yaml.MappingNode {
			continue
		}
		// A mapping node's Content is a flat [key, value, key, value, …]; the
		// role names are the keys and don't interest us, only the selectors.
		for i := 1; i < len(n.Content); i += 2 {
			v := n.Content[i]
			if v.Kind != yaml.ScalarNode {
				continue
			}
			sel := ompModelSelector(v.Value)
			if sel == "" || seen[sel] {
				continue
			}
			seen[sel] = true
			out = append(out, sel)
		}
	}
	return out
}

// ompModelSelector normalizes one role assignment into a `--model` value. omp's
// role syntax lets a selector pin a thinking level with a trailing ":<level>"
// ("anthropic/claude-opus-5:high"); lasso has its own effort field, so the level
// is dropped here rather than smuggled into --model where it would silently
// override — or fight — what the user picked in the creator. Only a suffix that
// IS one of omp's levels is stripped: model ids carry colons of their own
// (openrouter's "…:free"), and cutting one of those would break the selector.
func ompModelSelector(raw string) string {
	s := strings.TrimSpace(raw)
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return s
	}
	if !ompEffortLevel(s[i+1:]) {
		return s
	}
	return strings.TrimSpace(s[:i])
}

// ompEffortLevel reports whether s names a rung on omp's --thinking ladder (the
// harness registry's own list, so the two can't drift).
func ompEffortLevel(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, lvl := range harnessByID("omp").EffortLevels {
		if s == lvl {
			return true
		}
	}
	return false
}

// ompConfiguredModels reads host b's omp role assignments. Any failure — omp
// never installed, an unreadable or unparseable config — returns nothing, which
// the caller reads as "no host answer, keep the compiled-in list".
func ompConfiguredModels(b Backend, home string) []string {
	data, err := b.ReadFile(filepath.Join(home, ".omp", "agent", "config.yml"))
	if err != nil {
		return nil
	}
	return ompRoleModels(data)
}

// hostHarnesses is the harness registry as served for one host: the compiled-in
// table (harness.go) with omp's model suggestions replaced by the models that
// host's omp is configured to use for a role. Everything else in the registry —
// labels, effort ladders, plan-mode support — is compiled-in and host-independent.
//
// The registry slice and its entries are shared global state, so this copies
// before touching the omp entry. A host whose omp says nothing keeps the
// compiled-in suggestions.
func hostHarnesses(b Backend) []harnessDef {
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return harnesses
	}
	models := ompConfiguredModels(b, home)
	if len(models) == 0 {
		return harnesses
	}
	out := make([]harnessDef, len(harnesses))
	copy(out, harnesses)
	for i := range out {
		if out[i].ID == "omp" {
			out[i].ModelSuggestions = models
		}
	}
	return out
}

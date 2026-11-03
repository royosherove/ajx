package ci

import (
	"fmt"
	"os"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/royosherove/ajx/internal/util"
)

// Policy is the acceptance policy (acceptance.toml):
//
//	schema_version = 1
//	[acceptance]
//	min_success_rate = 1.0      # verified-success rate each cell needs (0..1)
//	min_repetitions = 1         # verified attempts each cell needs
//	require_cleanup = true      # unconfirmed cleanup fails the evaluation
//	require_reports = false     # an unfinished report makes it inconclusive
//	required_cells = ["cc"]     # cells the plan must include
type Policy struct {
	Path           string
	Sha256         string
	MinSuccessRate float64
	MinRepetitions int
	RequireCleanup bool
	RequireReports bool
	RequiredCells  []string
}

// DefaultPolicy: every planned attempt must verify succeeded.
func DefaultPolicy() Policy { return Policy{MinSuccessRate: 1} }

var policyKeys = map[string]bool{"min_success_rate": true, "min_repetitions": true, "require_cleanup": true,
	"require_reports": true, "required_cells": true}

// LoadPolicy reads path ("" = the default policy).
func LoadPolicy(path string) (Policy, error) {
	p := DefaultPolicy()
	if path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return p, fmt.Errorf("%s: %v", path, err)
	}
	p.Path = path
	p.Sha256 = util.Sha256Bytes(data)
	if v, ok := raw["schema_version"]; ok && util.Int(v, -1) != 1 {
		return p, fmt.Errorf("%s: schema_version must be 1", path)
	}
	for k := range raw {
		if k != "schema_version" && k != "acceptance" {
			return p, fmt.Errorf("%s: unknown key %q (expected schema_version and [acceptance])", path, k)
		}
	}
	acc, _ := raw["acceptance"].(map[string]any)
	var unknown []string
	for k := range acc {
		if !policyKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return p, fmt.Errorf("%s: unknown [acceptance] keys %v", path, unknown)
	}
	if v, ok := acc["min_success_rate"]; ok {
		f, isNum := util.Num(util.Normalize(v))
		if !isNum || f < 0 || f > 1 {
			return p, fmt.Errorf("%s: min_success_rate must be a number between 0 and 1", path)
		}
		p.MinSuccessRate = f
	}
	if v, ok := acc["min_repetitions"]; ok {
		n := util.Int(util.Normalize(v), -1)
		if n < 0 {
			return p, fmt.Errorf("%s: min_repetitions must be a whole number >= 0", path)
		}
		p.MinRepetitions = n
	}
	for key, dst := range map[string]*bool{"require_cleanup": &p.RequireCleanup, "require_reports": &p.RequireReports} {
		if v, ok := acc[key]; ok {
			b, isBool := v.(bool)
			if !isBool {
				return p, fmt.Errorf("%s: %s must be true or false", path, key)
			}
			*dst = b
		}
	}
	if v, ok := acc["required_cells"]; ok {
		list := util.List(util.Normalize(v))
		if list == nil {
			return p, fmt.Errorf("%s: required_cells must be a list of cell ids", path)
		}
		for _, c := range list {
			s, isStr := c.(string)
			if !isStr || s == "" {
				return p, fmt.Errorf("%s: required_cells must be a list of cell ids", path)
			}
			p.RequiredCells = append(p.RequiredCells, s)
		}
	}
	return p, nil
}

// Doc is the policy as recorded in ci-result.json.
func (p Policy) Doc() util.M {
	return util.M{"path": nilIfEmpty(p.Path), "sha256": nilIfEmpty(p.Sha256), "min_success_rate": p.MinSuccessRate,
		"min_repetitions": p.MinRepetitions, "require_cleanup": p.RequireCleanup, "require_reports": p.RequireReports,
		"required_cells": strs(p.RequiredCells)}
}

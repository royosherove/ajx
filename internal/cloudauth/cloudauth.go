// Package cloudauth ports lib/ajx/cloud_auth.py: bounded direct-env
// contracts for cloud auth in explicit execution environments.
//
// No provider discovery, credential-file reads, staging, or ambient
// credential forwarding. Legacy runs retain each provider's existing
// credential-chain behavior.
package cloudauth

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// Matches reports whether name equals one of patterns, or starts with a
// pattern's prefix when the pattern ends in '*'.
func Matches(name string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, p[:len(p)-1]) {
				return true
			}
		} else if name == p {
			return true
		}
	}
	return false
}

// Supplied reports whether env[name] is non-blank and contains no unresolved
// ${VAR} reference.
func Supplied(env map[string]string, name string) bool {
	value := env[name]
	return strings.TrimSpace(value) != "" && !envRef.MatchString(value)
}

// CloudAuth is the base of the cloud provider auth types. Subtypes declare
// complete supported routes; other routes need a custom auth plugin.
type CloudAuth struct {
	*plugin.AuthBase
	ProviderEnv      string
	CredentialRoutes [][]string
	OptionalEnv      []string
	EndpointRoutes   []string
	UnsupportedEnv   []string
	HomeCredentials  string
	// EnvAliases are (declared, alias) pairs. When the profile's env sets the
	// first name and nothing sets the second, the worker gets the alias with
	// the same value: Claude Code and the AWS CLI read AWS_REGION, but
	// botocore (a pip-installed boto3 in a setup script) reads only
	// AWS_DEFAULT_REGION.
	EnvAliases [][2]string
	// FixedEnv is the provider marker the subtype always sets, below the
	// profile's own env (Python {"MARKER": "1", **super().env()}).
	FixedEnv map[string]string
	// BaseUnset is what the subtype always unsets, before the profile's own
	// unset list (Python [...] + super().unset()).
	BaseUnset []string
}

// New builds a CloudAuth with the generic defaults for conf.
func New(conf util.M) *CloudAuth {
	return &CloudAuth{AuthBase: plugin.NewAuthBase(conf), HomeCredentials: "caller-home credentials",
		EnvAliases: [][2]string{{"AWS_REGION", "AWS_DEFAULT_REGION"}}}
}

func (c *CloudAuth) me() plugin.Auth {
	if c.Self != nil {
		return c.Self
	}
	return c
}

// Env is the fixed provider env overlaid by the profile's env plus aliases.
func (c *CloudAuth) Env() map[string]string {
	env := c.AuthBase.Env()
	unset := c.me().Unset()
	for _, pair := range c.EnvAliases {
		declared, alias := pair[0], pair[1]
		value := env[declared]
		if _, present := env[alias]; strings.TrimSpace(value) != "" && !present && !Matches(alias, unset) {
			env[alias] = value
		}
	}
	return util.MergeEnv(c.FixedEnv, env)
}

// Unset is the subtype's fixed unset list followed by the profile's.
func (c *CloudAuth) Unset() []string {
	out := append([]string{}, c.BaseUnset...)
	return append(out, c.AuthBase.Unset()...)
}

// ModelVars adds a mirrored alias: it is the model's variable too, so
// envcheck can tell it from a task variable.
func (c *CloudAuth) ModelVars(env map[string]string) []string {
	out := c.AuthBase.ModelVars(env)
	declared := util.Map(c.Conf["env"])
	for _, pair := range c.EnvAliases {
		if _, ok := declared[pair[0]]; ok {
			out = append(out, pair[1])
		}
	}
	return out
}

// Problems uses the environment readiness check bound by spec when there is
// one and no explicit env was given.
func (c *CloudAuth) Problems(env map[string]string) []string {
	if env == nil && c.EnvironmentCheck != nil {
		return c.EnvironmentCheck()
	}
	return c.AuthBase.Problems(env)
}

// EnvironmentProblems checks env (the worker's explicit environment) against
// the provider's supported direct-env routes. Values are never printed.
func (c *CloudAuth) EnvironmentProblems(env map[string]string, environment util.M) []string {
	unset := c.me().Unset()
	kept := map[string]string{}
	for k, v := range env {
		if !Matches(k, unset) {
			kept[k] = v
		}
	}
	env = kept
	out := c.AuthBase.Problems(env)
	var names []string
	names = append(names, c.RequiredEnv...)
	for _, route := range c.CredentialRoutes {
		names = append(names, route...)
	}
	names = append(names, c.EndpointRoutes...)
	names = append(names, c.OptionalEnv...)
	for _, name := range names {
		if env[name] != "" && !Supplied(env, name) {
			out = append(out, fmt.Sprintf("env var %s must be non-empty and resolved in the worker environment", name))
		}
	}
	if c.ProviderEnv != "" {
		if v := strings.ToLower(env[c.ProviderEnv]); v != "1" && v != "true" {
			out = append(out, fmt.Sprintf("set %s=1 (or true) and do not unset it", c.ProviderEnv))
		}
	}
	if len(c.EndpointRoutes) > 0 {
		any := false
		for _, name := range c.EndpointRoutes {
			if Supplied(env, name) {
				any = true
			}
		}
		if !any {
			out = append(out, "declare "+strings.Join(c.EndpointRoutes, " or ")+" in the worker env to select the endpoint")
		}
	}
	var unsupported []string
	for k, v := range env {
		if v != "" && Matches(k, c.UnsupportedEnv) {
			unsupported = append(unsupported, k)
		}
	}
	sort.Strings(unsupported)
	name := c.me().Name()
	if len(unsupported) > 0 {
		out = append(out, fmt.Sprintf("%s does not support %s in explicit environments; "+
			"remove/unset these controls and use a supported direct-env route, or use a custom auth plugin "+
			"that supplies credentials inside the selected environment and removes them before archival. "+
			"A caller file path is not staged or made available in a container.", name, strings.Join(unsupported, ", ")))
	}
	if len(c.CredentialRoutes) == 0 {
		out = append(out, fmt.Sprintf("%s has no supported direct-env credential route in explicit environments: "+
			"%s are not provisioned for a fresh HOME. Use a custom auth plugin that "+
			"stages credentials inside the selected environment and removes them before archival; "+
			"a preconfigured gateway can use type='env' with its complete explicit auth contract.", name, c.HomeCredentials))
	} else {
		complete := false
		for _, route := range c.CredentialRoutes {
			all := true
			for _, n := range route {
				if !Supplied(env, n) {
					all = false
					break
				}
			}
			if all {
				complete = true
				break
			}
		}
		if !complete {
			routes := make([]string, len(c.CredentialRoutes))
			for i, route := range c.CredentialRoutes {
				routes[i] = strings.Join(route, " + ")
			}
			out = append(out, fmt.Sprintf("%s requires explicitly declared %s in the worker env "+
				"(auth.env supports ${VAR} references); %s and ambient credentials "+
				"are not supported by this contract for a fresh HOME. Other routes require type='env' with a complete "+
				"independently supplied contract or a custom auth plugin with staging and cleanup.",
				name, strings.Join(routes, " or "), c.HomeCredentials))
		}
	}
	return out
}

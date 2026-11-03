package environments

import (
	"math"
	"os"
	"sort"
	"strings"

	"github.com/royosherove/ajx/internal/util"
)

// asTable returns a Python-dict-like value as util.M.
func asTable(v any) (util.M, bool) {
	switch t := v.(type) {
	case util.M:
		if t == nil {
			return util.M{}, true
		}
		return t, true
	case map[string]string:
		return util.AnyMap(t), true
	}
	return nil, false
}

// table is Python _table.
func table(value any, field string, allowed map[string]bool) (util.M, error) {
	m, ok := asTable(value)
	if !ok {
		return nil, fail(field, "expected a table")
	}
	var extra []string
	for k := range m {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fail(field+"."+extra[0], "unknown field; unsupported constraints are not ignored")
	}
	return m, nil
}

func keysOf(m util.M) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// choice is Python _choice.
func choice(value any, options []string, field string) (string, error) {
	s, ok := value.(string)
	if ok {
		for _, o := range options {
			if s == o {
				return s, nil
			}
		}
	}
	sorted := append([]string(nil), options...)
	sort.Strings(sorted)
	return "", fail(field, "expected "+strings.Join(sorted, " | "))
}

// asList is a Python list/tuple value.
func asList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string, []util.M:
		return util.List(t), true
	}
	return nil, false
}

// stringsOf is Python _strings.
func stringsOf(value any, field string) ([]string, error) {
	items, ok := asList(value)
	bad := !ok
	var out []string
	for _, v := range items {
		s, isStr := v.(string)
		if !isStr || s == "" || strings.Contains(s, "\x00") {
			bad = true
			break
		}
		out = append(out, s)
	}
	if bad {
		return nil, fail(field, "expected an array of nonempty strings without NUL bytes")
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// arguments is Python _arguments.
func arguments(value any, field string) ([]string, error) {
	items, ok := asList(value)
	bad := !ok
	var out []string
	for _, v := range items {
		s, isStr := v.(string)
		if !isStr || strings.Contains(s, "\x00") {
			bad = true
			break
		}
		out = append(out, s)
	}
	if bad {
		return nil, fail(field, "expected an argument array of strings without NUL bytes")
	}
	if len(out) == 0 || out[0] == "" || strings.HasPrefix(out[0], "-") {
		return nil, fail(field, "expected an executable followed by optional arguments")
	}
	// Empty argument values are significant, for example Claude's --tools "".
	return out, nil
}

// number is Python _number; returns int64 for integers, float64 otherwise.
func number(value any, field string, low, high float64, integer bool) (any, error) {
	if _, isBool := value.(bool); isBool || !util.IsNum(value) {
		return nil, fail(field, "expected a number")
	}
	f, _ := util.Num(value)
	if !(low <= f && f <= high) || math.IsNaN(f) || math.IsInf(f, 0) || (integer && math.Trunc(f) != f) {
		kind := "a number"
		if integer {
			kind = "an integer"
		}
		return nil, fail(field, "expected "+kind+" between "+pyNumber(low, false)+" and "+pyNumber(high, false))
	}
	if integer {
		return int64(f), nil
	}
	return f, nil
}

// NormalizeProfiles validates portable declarations without provisioning,
// probing tools, or contacting Docker (Python normalize_profiles). Errors are
// *Error with Kind "ValueError" (or "_InspectionChanged").
func NormalizeProfiles(raw util.M, baseDir string) (util.M, error) {
	return normalizeProfiles(raw, baseDir, true)
}

func normalizeProfiles(rawProfiles util.M, baseDir string, inspectSources bool) (util.M, error) {
	if rawProfiles == nil {
		rawProfiles = util.M{}
	}
	if _, err := table(rawProfiles, "environments", keysOf(rawProfiles)); err != nil {
		return nil, err
	}
	abs := baseDir
	if !isAbs(abs) {
		wd, _ := os.Getwd()
		abs = pathJoin(wd, baseDir)
	}
	base, err := directory(abs, "environments base_dir", true)
	if err != nil {
		return nil, err
	}
	out := util.M{}
	for _, name := range util.SortedKeys(rawProfiles) {
		item, err := normalizeOne(name, rawProfiles[name], base, inspectSources)
		if err != nil {
			return nil, err
		}
		out[name] = item
	}
	return out, nil
}

func normalizeOne(name string, raw any, base string, inspectSources bool) (util.M, error) {
	field := "environments." + name
	if !nameRE.MatchString(name) {
		return nil, fail(field, "profile name must contain 1-64 letters, digits, dots, underscores, or hyphens")
	}
	conf, err := table(raw, field, profileKeys)
	if err != nil {
		return nil, err
	}
	if id, ok := conf["id"]; ok {
		if s, isStr := id.(string); !isStr || s != name {
			return nil, fail(field+".id", "must match the named profile")
		}
	}
	backend, err := choice(conf["backend"], []string{"local", "container"}, field+".backend")
	if err != nil {
		return nil, err
	}
	container := backend == "container"
	item := util.M{"id": name, "backend": backend, "_base_dir": base}
	network, hasNetwork := conf["network"]
	if !hasNetwork {
		network = "inherit"
		if container {
			network = "none"
		}
	}
	options := []string{"inherit"}
	if container {
		options = []string{"none", "bridge"}
	}
	if item["network"], err = choice(network, options, field+".network"); err != nil {
		return nil, err
	}
	defaultKeys := []string{"filesystem", "root", "package_install"}
	defaults := map[string]string{"filesystem": "observed", "root": "inherit", "package_install": "unrestricted"}
	if container {
		defaults = map[string]string{"filesystem": "isolated", "root": "forbid", "package_install": "user"}
	}
	permRaw, ok := conf["permissions"]
	if !ok {
		permRaw = util.M{}
	}
	perm, err := table(permRaw, field+".permissions", setOf(defaultKeys...))
	if err != nil {
		return nil, err
	}
	for _, key := range defaultKeys {
		expected := defaults[key]
		if v, present := perm[key]; present {
			if s, isStr := v.(string); !isStr || s != expected {
				return nil, fail(field+".permissions."+key,
					backend+" supports only "+pyRepr(expected)+"; the requested restriction cannot be enforced")
			}
		}
	}
	item["permissions"] = util.M{"filesystem": defaults["filesystem"], "root": defaults["root"],
		"package_install": defaults["package_install"]}
	limitsRaw, ok := conf["limits"]
	if !ok {
		limitsRaw = util.M{}
	}
	limitKeys := map[string]bool{}
	for _, l := range limitSpecs {
		limitKeys[l.key] = true
	}
	limits, err := table(limitsRaw, field+".limits", limitKeys)
	if err != nil {
		return nil, err
	}
	if len(limits) > 0 && !container {
		return nil, fail(field+".limits", "local cannot enforce independent resource or environment lifetime limits")
	}
	normalizedLimits := util.M{}
	item["limits"] = normalizedLimits
	if container {
		for _, l := range limitSpecs {
			var v any = l.def
			if l.defIsInt {
				v = int64(l.def)
			}
			if given, present := limits[l.key]; present {
				v = given
			}
			n, err := number(v, field+".limits."+l.key, l.low, l.high, l.integer)
			if err != nil {
				return nil, err
			}
			normalizedLimits[l.key] = n
		}
		if util.Int(normalizedLimits["tmpfs_mb"], 0) > util.Int(normalizedLimits["memory_mb"], 0) {
			return nil, fail(field+".limits.tmpfs_mb", "cannot exceed memory_mb")
		}
		image, isStr := conf["image"].(string)
		if !isStr || !imageRE.MatchString(image) {
			return nil, fail(field+".image", "use a full sha256 image ID or repository@sha256:digest; "+
				"pull explicitly and pin it once for the matrix (mutable tags are rejected)")
		}
		item["image"] = image
		if p, present := conf["platform"]; present {
			s, isStr := p.(string)
			if !isStr || !platformRE.MatchString(s) {
				return nil, fail(field+".platform", "expected linux/architecture[/variant]")
			}
			item["platform"] = s
		}
		host, present := conf["docker_host"]
		if !present {
			host = "unix:///var/run/docker.sock"
		}
		if item["docker_host"], err = dockerHost(host, field+".docker_host"); err != nil {
			return nil, err
		}
	} else {
		for _, key := range []string{"image", "platform", "docker_host"} {
			if _, present := conf[key]; present {
				return nil, fail(field+"."+key, "supported only by the container backend")
			}
		}
	}
	requiredRaw, ok := conf["required"]
	if !ok {
		requiredRaw = []any{}
	}
	required, err := stringsOf(requiredRaw, field+".required")
	if err != nil {
		return nil, err
	}
	if len(util.Uniq(required)) != len(required) {
		return nil, fail(field+".required", "duplicate capability")
	}
	supported := setOf(RequiredCapabilities...)
	for _, capability := range required {
		if !supported[capability] {
			return nil, fail(field+".required", "unsupported capability "+pyRepr(capability))
		}
		if !container || (capability == "network_isolation" && item["network"] != "none") {
			return nil, fail(field+".required", backend+"/"+util.S(item["network"])+" cannot enforce "+capability)
		}
	}
	item["required"] = toAnyStrings(required)
	toolsRaw, ok := conf["tools"]
	if !ok {
		toolsRaw = []any{}
	}
	tools, isList := asList(toolsRaw)
	if !isList {
		return nil, fail(field+".tools", "expected an array of tool probe tables")
	}
	outTools := []any{}
	seen := map[string]bool{}
	for i, tool := range tools {
		tf := field + ".tools[" + itoa(i) + "]"
		t, err := table(tool, tf, setOf("name", "argv", "required"))
		if err != nil {
			return nil, err
		}
		toolName, isStr := t["name"].(string)
		if !isStr || !nameRE.MatchString(toolName) || seen[toolName] {
			return nil, fail(tf+".name", "expected a unique tool name")
		}
		argv, err := arguments(t["argv"], tf+".argv")
		if err != nil {
			return nil, err
		}
		req := true
		if r, present := t["required"]; present {
			b, isBool := r.(bool)
			if !isBool {
				return nil, fail(tf+".required", "expected a boolean")
			}
			req = b
		}
		seen[toolName] = true
		outTools = append(outTools, util.M{"name": toolName, "argv": toAnyStrings(argv), "required": req})
	}
	item["tools"] = outTools
	mountsRaw, ok := conf["mounts"]
	if !ok {
		mountsRaw = []any{}
	}
	mounts, isList := asList(mountsRaw)
	if !isList {
		return nil, fail(field+".mounts", "expected an array of directory mounts")
	}
	if len(mounts) > 0 && !container {
		return nil, fail(field+".mounts", "local does not implement directory mount boundaries")
	}
	outMounts := []any{}
	for i, mount := range mounts {
		mf := field + ".mounts[" + itoa(i) + "]"
		m, err := table(mount, mf, setOf("source", "target", "access"))
		if err != nil {
			return nil, err
		}
		text, err := pathText(m["source"], mf+".source")
		if err != nil {
			return nil, err
		}
		source := pyPath(text)
		if !isAbs(source) {
			source = pathJoin(base, source)
		}
		if inspectSources {
			if source, err = directory(source, mf+".source", true); err != nil {
				return nil, err
			}
		}
		if source == base || !relativeTo(source, base) || forbidden(relPath(source, base)) {
			return nil, fail(mf+".source", "must be a dedicated fixture directory inside the trial directory; "+
				"host configuration, credentials, reports, and sibling paths cannot be mounted")
		}
		tgt, err := target(m["target"], mf+".target")
		if err != nil {
			return nil, err
		}
		access, err := choice(m["access"], []string{"read", "write"}, mf+".access")
		if err != nil {
			return nil, err
		}
		for _, prev := range outMounts {
			p := prev.(util.M)
			if overlaps(source, util.S(p["source"])) || overlaps(tgt, util.S(p["target"])) {
				return nil, fail(mf, "mount sources and targets must not overlap other declared mounts")
			}
		}
		if inspectSources {
			if _, err := treeManifest(source, mf+".source", treeOpts{fixtures: true}); err != nil {
				return nil, err
			}
		}
		outMounts = append(outMounts, util.M{"source": source, "target": tgt, "access": access})
	}
	item["mounts"] = outMounts
	return item, nil
}

func itoa(i int) string { return util.Str(int64(i)) }

func toAnyStrings(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// envMap reads a Python env mapping (util.M of strings or map[string]string).
func envMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case nil:
		return map[string]any{}, true
	case util.M:
		return t, true
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = x
		}
		return out, true
	}
	return nil, false
}

// ValidateForCell rejects cell-level controls that bypass the explicit
// environment contract (Python validate_for_cell). The cell's "env" may be a
// util.M or map[string]string, "unset_env" a list of names or prefixes.
func ValidateForCell(profile, cell util.M) error {
	id := "<unnamed>"
	if v, ok := cell["id"]; ok {
		id = util.Str(v)
	}
	field := "cells." + id
	if util.Truthy(cell["runner"]) {
		return fail(field+".runner", "cannot combine a legacy runner with an explicit environment")
	}
	rawEnv, present := cell["env"]
	if !present {
		rawEnv = util.M{}
	}
	env, ok := envMap(rawEnv)
	if !ok || rawEnv == nil {
		return fail(field+".env", "expected a table")
	}
	for _, key := range util.SortedKeys(env) {
		if isolationEnv[key] {
			return fail(field+".env."+key, "controlled by the per-attempt environment")
		}
	}
	if _, err := checkedEnvAny(env, field+".env"); err != nil {
		return err
	}
	drops := cell["unset_env"]
	if util.Truthy(drops) {
		list, err := validateUnsetAny(drops, field+".unset_env")
		if err != nil {
			return err
		}
		if len(list) > 0 {
			for _, key := range util.SortedKeys(isolationEnv) {
				if dropped(key, list) {
					return fail(field+".unset_env", "cannot unset the isolated "+key+" path")
				}
			}
			var conflicts []string
			for key := range env {
				if dropped(key, list) {
					conflicts = append(conflicts, key)
				}
			}
			if len(conflicts) > 0 {
				sort.Strings(conflicts)
				return fail(field+".env", "variables are both supplied and unset: "+strings.Join(conflicts, ", ")+
					"; remove the overlapping env declarations or narrow auth.unset before running")
			}
		}
	}
	return nil
}

// checkedEnv is Python _checked_env for a Go environment.
func checkedEnv(overrides map[string]string, field string) (map[string]string, error) {
	if field == "" {
		field = "environment.env"
	}
	for _, key := range util.SortedKeys(overrides) {
		if !envNameRE.MatchString(key) {
			return nil, fail(field, "invalid environment variable name")
		}
		if unsafeEnv[key] || hasAnyPrefix(key, unsafePrefixes) {
			return nil, fail(field+"."+key, "process-loader, shell-startup, agent socket, or Docker control override is unsupported")
		}
		if strings.Contains(overrides[key], "\x00") {
			return nil, fail(field+"."+key, "expected a string without NUL bytes")
		}
	}
	out := make(map[string]string, len(overrides))
	for k, v := range overrides {
		out[k] = v
	}
	return out, nil
}

// checkedEnvAny is _checked_env for a mapping whose values may not be strings.
func checkedEnvAny(overrides map[string]any, field string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range util.SortedKeys(overrides) {
		if !envNameRE.MatchString(key) {
			return nil, fail(field, "invalid environment variable name")
		}
		if unsafeEnv[key] || hasAnyPrefix(key, unsafePrefixes) {
			return nil, fail(field+"."+key, "process-loader, shell-startup, agent socket, or Docker control override is unsupported")
		}
		s, isStr := overrides[key].(string)
		if !isStr || strings.Contains(s, "\x00") {
			return nil, fail(field+"."+key, "expected a string without NUL bytes")
		}
		out[key] = s
	}
	return out, nil
}

func validUnsetName(v string) bool {
	if strings.HasSuffix(v, "*") {
		v = v[:len(v)-1]
	}
	return envNameRE.MatchString(v)
}

// validateUnset is Python _validate_unset.
func validateUnset(unset []string, field string) error {
	if field == "" {
		field = "environment.unset_env"
	}
	for _, v := range unset {
		if !validUnsetName(v) {
			return fail(field, "expected variable names or trailing-star prefixes")
		}
	}
	return nil
}

func validateUnsetAny(value any, field string) ([]string, error) {
	items, ok := asList(value)
	if !ok {
		return nil, fail(field, "expected variable names or trailing-star prefixes")
	}
	var out []string
	for _, v := range items {
		s, isStr := v.(string)
		if !isStr || !validUnsetName(s) {
			return nil, fail(field, "expected variable names or trailing-star prefixes")
		}
		out = append(out, s)
	}
	return out, nil
}

// dropped is Python _dropped.
func dropped(key string, unset []string) bool { return util.Dropped(key, unset) }

// redactEnvOutput is Python _redact_env_output: inventory is a durable record,
// so mask known supplied values; never serialize its env.
func redactEnvOutput(text string, env map[string]string) string {
	type pair struct{ key, value string }
	var replacements []pair
	for _, key := range util.SortedKeys(env) {
		value := env[key]
		if _, base := baseEnv[key]; base || isolationEnv[key] || value == "" {
			continue
		}
		if util.RuneLen(value) >= 8 || secretNameRE.MatchString(key) {
			replacements = append(replacements, pair{key, value})
		}
	}
	sort.SliceStable(replacements, func(i, j int) bool {
		return util.RuneLen(replacements[i].value) > util.RuneLen(replacements[j].value)
	})
	for _, r := range replacements {
		text = strings.ReplaceAll(text, r.value, "<redacted env:"+r.key+">")
	}
	return text
}

// pathEnvironmentKeys is the key order of Python's _path_environment dict.
var pathEnvironmentKeys = []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	"TMPDIR", "TMP", "TEMP", "PYTHONUSERBASE", "npm_config_cache", "npm_config_prefix", "CARGO_HOME", "RUSTUP_HOME",
	"GOPATH", "GOCACHE", "PIP_CACHE_DIR", "UV_CACHE_DIR", "PATH"}

// pathEnvironment is Python _path_environment: the path-only environment
// shared by isolated workers and separate host check contexts.
func pathEnvironment(home, config, cache string) map[string]string {
	home, config, cache = pyPath(home), pyPath(config), pyPath(cache)
	local := pathJoin(home, ".local")
	tmp := pathJoin(cache, "tmp")
	return map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": config, "XDG_CACHE_HOME": cache,
		"XDG_DATA_HOME": pathJoin(local, "share"), "XDG_STATE_HOME": pathJoin(local, "state"),
		"TMPDIR": tmp, "TMP": tmp, "TEMP": tmp,
		"PYTHONUSERBASE": local, "npm_config_cache": pathJoin(cache, "npm"),
		"npm_config_prefix": local, "CARGO_HOME": pathJoin(home, ".cargo"),
		"RUSTUP_HOME": pathJoin(home, ".rustup"), "GOPATH": pathJoin(home, "go"),
		"GOCACHE": pathJoin(cache, "go-build"), "PIP_CACHE_DIR": pathJoin(cache, "pip"),
		"UV_CACHE_DIR": pathJoin(cache, "uv"),
		"PATH": strings.Join([]string{pathJoin(local, "bin"), pathJoin(home, ".cargo", "bin"),
			pathJoin(home, "go", "bin"), containerPath}, ":"),
	}
}

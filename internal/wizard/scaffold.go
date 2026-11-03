// Package wizard scaffolds a trial (`ajx init`): the same trial.toml and
// task-prompt.md as the Python CLI, optionally filled in from flags or from
// an interactive form (huh) that asks for the product, the task, materials,
// harness, model, reporter, success check, limits, fixture and environment.
package wizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/spec"
)

// Options are everything `ajx init` can write. Zero values keep the Python
// scaffold's defaults and placeholders.
type Options struct {
	Dir, Product, ID     string
	Harness, Reporter    string
	Model, ReporterModel string
	Task                 string
	Materials            []string
	Checks               []string // shell command, "file:GLOB", "http:URL", "shell:CMD"
	Timeout, Repetitions int
	Fixture              string // as given (relative to the working directory or absolute)
	Environment, Image   string // "" | "local" | "container"
	Force                bool
}

// Check is one parsed success check.
type Check struct {
	Type, Name, Value string
}

// ParseCheck reads a --check value.
func ParseCheck(raw string) (Check, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Check{}, errors.New("empty --check")
	}
	kind, rest, ok := strings.Cut(raw, ":")
	switch {
	case ok && kind == "file":
		rest = strings.TrimSpace(rest)
		if rest == "" || filepath.IsAbs(rest) || strings.Contains("/"+rest+"/", "/../") {
			return Check{}, fmt.Errorf("--check file:%s must be a glob beneath the workspace, without '..'", rest)
		}
		return Check{"file_exists", rest + " exists", rest}, nil
	case ok && kind == "http" && (strings.HasPrefix(rest, "//") || strings.HasPrefix(rest, "http")):
		url := raw
		if strings.HasPrefix(rest, "http") { // "http:https://..." form
			url = rest
		}
		return Check{"http", "responds at " + url, url}, nil
	case ok && kind == "https" && strings.HasPrefix(rest, "//"):
		return Check{"http", "responds at " + raw, raw}, nil
	case ok && kind == "shell":
		raw = strings.TrimSpace(rest)
		if raw == "" {
			return Check{}, errors.New("empty --check shell: command")
		}
	}
	name := raw
	if r := []rune(name); len(r) > 60 {
		name = string(r[:60])
	}
	return Check{"shell", name, raw}, nil
}

// HarnessInfo describes a registered harness for the form and errors.
type HarnessInfo struct {
	Name      string
	Installed bool
	Version   string
	CanReport bool
	Verified  bool
}

// Harnesses lists the registered harnesses (versions only when probe is set).
func Harnesses(probe bool) []HarnessInfo {
	var out []HarnessInfo
	for _, name := range plugin.Names("harness") {
		h, err := plugin.NewHarness(name)
		if err != nil {
			continue
		}
		b := h.Base()
		info := HarnessInfo{Name: name, CanReport: b.CanReport, Verified: b.VerifiedLive}
		if probe {
			info.Installed = h.Available()
			if info.Installed {
				info.Version = h.Version()
			}
		}
		out = append(out, info)
	}
	return out
}

func canReport(name string) (bool, bool) {
	h, err := plugin.NewHarness(name)
	if err != nil {
		return false, false
	}
	return true, h.Base().CanReport
}

// Resolve checks the options and fills derived values (reporter, id).
// Errors are spec.Error so the CLI exits 2, like the Python CLI.
func Resolve(o *Options) error {
	if o.Harness == "" {
		o.Harness = "claude-code"
	}
	if ok, _ := canReport(o.Harness); !ok {
		return spec.Errorf("unknown harness %s; run `ajx plugins`", pyRepr(o.Harness))
	}
	if o.Reporter == "" {
		if _, cr := canReport(o.Harness); cr {
			o.Reporter = o.Harness
		} else {
			o.Reporter = "claude-code"
		}
	}
	if ok, cr := canReport(o.Reporter); !ok || !cr {
		return spec.Errorf("%s has no restricted reporter adapter", pyRepr(o.Reporter))
	}
	for _, c := range o.Checks {
		if _, err := ParseCheck(c); err != nil {
			return spec.Errorf("%s", err.Error())
		}
	}
	switch o.Environment {
	case "", "local":
	case "container":
		if strings.TrimSpace(o.Image) == "" {
			return spec.Errorf("--environment container needs --image: an immutable local image id (sha256:...) or name@sha256:digest")
		}
	default:
		return spec.Errorf("unknown environment %s; choose local or container", pyRepr(o.Environment))
	}
	o.Product = strings.TrimSpace(o.Product)
	for _, m := range []string{o.Model, o.ReporterModel} {
		if err := ModelName(m); err != nil {
			return spec.Errorf("%s", err.Error())
		}
	}
	o.Model, o.ReporterModel = strings.TrimSpace(o.Model), strings.TrimSpace(o.ReporterModel)
	if o.Timeout < 0 || o.Repetitions < 0 {
		return spec.Errorf("--timeout and --repetitions must be positive")
	}
	return nil
}

func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

var trialSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)

// TrialID is the default trial id for a directory: its name lowercased with
// spaces as dashes (as the Python CLI does) when that is a valid trial id,
// else Slug(name), so the scaffold always loads.
func TrialID(dir string) string {
	if id := strings.ReplaceAll(strings.ToLower(filepath.Base(dir)), " ", "-"); trialSlug.MatchString(id) {
		return id
	}
	return Slug(filepath.Base(dir))
}

// Slug turns any text into a valid trial id: lowercase letters, digits and
// . _ -, starting with a letter or digit, at most 48 characters (cut at a
// dash when possible), "my-trial" when nothing is left.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.TrimLeft(b.String(), "-._")
	if len(out) > 48 {
		out = out[:48]
		if i := strings.LastIndexByte(out, '-'); i >= 24 {
			out = out[:i]
		}
	}
	out = strings.TrimRight(out, "-._")
	if out == "" {
		return "my-trial"
	}
	return out
}

// ModelName refuses what no agent CLI accepts as a model (same rule as the
// console's editor).
func ModelName(v string) error {
	name := strings.TrimSpace(v)
	if name == "" {
		return nil
	}
	if strings.ContainsAny(name, " \t\n\r") || strings.HasPrefix(name, "-") || len(name) > 200 {
		return fmt.Errorf("%s is not a model name an agent CLI accepts: use an alias such as sonnet, or the "+
			"provider's model id (no spaces)", pyRepr(name))
	}
	return nil
}

// TrivialCheck reports a shell check that passes whatever the worker did
// (echo, true, :), so it cannot show whether the task succeeded.
func TrivialCheck(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if strings.ContainsAny(c, "|&;><$`") {
		return false
	}
	first := strings.Fields(c + " ")[0]
	return first == "echo" || first == "true" || first == ":" || first == "printf"
}

// Warnings are reasons a scaffold may not test what the user wants.
func Warnings(o Options) []string {
	var out []string
	for _, raw := range o.Checks {
		if c, err := ParseCheck(raw); err == nil && c.Type == "shell" && TrivialCheck(c.Value) {
			out = append(out, fmt.Sprintf("the check %s always passes, so it cannot show whether the task succeeded; "+
				"check the outcome instead (for example a file the task creates, or a command that tests it)", pyRepr(c.Value)))
		}
	}
	return out
}

// tomlString quotes a TOML basic string. Plain values match the Python
// template's raw substitution.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlArray(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// replaceLine swaps the first line starting with prefix (after trimming the
// leading spaces) for repl, keeping its trailing comment when keepComment.
func replaceLine(text, prefix, repl string, keepComment bool) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " "), prefix) {
			if keepComment {
				if j := strings.Index(line, "  #"); j >= 0 {
					comment := strings.TrimLeft(line[j:], " ")
					pad := j + 2 - len(repl)
					if pad < 1 {
						pad = 1
					}
					repl = repl + strings.Repeat(" ", pad) + comment
				}
			}
			lines[i] = repl
			return strings.Join(lines, "\n")
		}
	}
	return text
}

// Render returns trial.toml and task-prompt.md for resolved options.
func Render(o Options, trialDir string) (string, string, error) {
	id := o.ID
	if id == "" {
		id = TrialID(trialDir)
	}
	product := o.Product
	quote := func(s string) string {
		if strings.ContainsAny(s, "\"\\\n\r\t") {
			return tomlString(s)
		}
		return `"` + s + `"`
	}
	t := strings.NewReplacer("\"{id}\"", quote(id), "\"{product}\"", quote(product),
		"\"{harness}-default\"", quote(o.Harness+"-default"), "\"{harness}\"", quote(o.Harness),
		"\"{reporter}\"", quote(o.Reporter)).Replace(trialTemplate)
	if o.Repetitions > 0 {
		t = replaceLine(t, "repetitions = ", "repetitions = "+strconv.Itoa(o.Repetitions), false)
	}
	if o.Timeout > 0 {
		t = replaceLine(t, "timeout_seconds = ", "timeout_seconds = "+strconv.Itoa(o.Timeout), false)
	}
	if len(o.Materials) > 0 {
		t = replaceLine(t, "materials = []", "materials = "+tomlArray(o.Materials), true)
	}
	if o.Fixture != "" {
		rel, err := fixturePath(o.Fixture, trialDir)
		if err != nil {
			return "", "", err
		}
		t = replaceLine(t, "# fixture_dir = ", "fixture_dir = "+tomlString(rel), true)
	}
	if len(o.Checks) > 0 {
		var blocks []string
		for _, raw := range o.Checks {
			c, err := ParseCheck(raw)
			if err != nil {
				return "", "", spec.Errorf("%s", err.Error())
			}
			b := "[[verify]]\nname = " + tomlString(c.Name) + "\n"
			switch c.Type {
			case "file_exists":
				b += "type = \"file_exists\"\npath = " + tomlString(c.Value) + "\n"
			case "http":
				b += "type = \"http\"\nurl = " + tomlString(c.Value) + "\n"
			default:
				b += "run = " + tomlString(c.Value) + "\n"
			}
			blocks = append(blocks, b)
		}
		marker := "# [[teardown]]"
		t = strings.Replace(t, marker, strings.Join(blocks, "\n")+"\n"+marker, 1)
	}
	if o.ReporterModel != "" {
		t = strings.Replace(t, "# model = \"your-model\"\n", "model = "+tomlString(o.ReporterModel)+"\n", 1)
	}
	if o.Model != "" {
		t = replaceLine(t, "# model = \"your-model\"            # or models", "model = "+tomlString(o.Model), false)
	}
	if o.Environment == "container" {
		t = replaceLine(t, "# environment = \"developer\"", "environment = \"container\"            # [environments.container] above", false)
		block := containerProfile(o.Image)
		t = strings.Replace(t, "[reporter]", block+"\n[reporter]", 1)
	}
	return t, renderPrompt(o), nil
}

func containerProfile(image string) string {
	return "[environments.container]          # the worker runs in a local Docker container (see references/environments.md)\n" +
		"backend = \"container\"\n" +
		"image = " + tomlString(image) + "\n" +
		"network = \"bridge\"\n" +
		"required = [\"filesystem_isolation\", \"non_root\", \"resource_limits\", \"lifetime\"]\n" +
		"permissions = { filesystem = \"isolated\", root = \"forbid\", package_install = \"user\" }\n" +
		"limits = { cpus = 1, memory_mb = 1024, pids = 256, tmpfs_mb = 64, lifetime_seconds = 3600 }\n"
}

func renderPrompt(o Options) string {
	if strings.TrimSpace(o.Task) == "" {
		return strings.ReplaceAll(promptTemplate, "{product}", o.Product)
	}
	var b strings.Builder
	b.WriteString("I want to try " + o.Product + ".\n\n")
	b.WriteString(strings.TrimSpace(o.Task) + "\n")
	if len(o.Materials) > 0 {
		b.WriteString("\nMaterials you can use:\n")
		for _, m := range o.Materials {
			b.WriteString("- " + m + "\n")
		}
	}
	return b.String()
}

// fixturePath is the fixture_dir value: relative to the trial directory
// when inside it, else absolute. It must exist.
func fixturePath(given, trialDir string) (string, error) {
	abs, err := filepath.Abs(given)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", spec.Errorf("fixture directory not found: %s", abs)
	}
	if rel, err := filepath.Rel(trialDir, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel), nil
	}
	return abs, nil
}

// ResolvePath is Python's Path.resolve(strict=False): absolute, with the
// symlinks of the longest existing prefix resolved (/tmp -> /private/tmp).
func ResolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Result says what Write did.
type Result struct {
	Dir, TrialFile, PromptFile string
	Written, Kept              []string
	ID                         string
	Valid                      bool
	Problem                    string // why the written trial does not load, when !Valid
}

// Write renders and writes the files (existing files are kept unless Force).
func Write(o Options) (*Result, error) {
	dir, err := ResolvePath(o.Dir)
	if err != nil {
		return nil, err
	}
	trialText, promptText, err := Render(o, dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{Dir: dir, TrialFile: filepath.Join(dir, "trial.toml"), PromptFile: filepath.Join(dir, "task-prompt.md"),
		ID: o.ID}
	if res.ID == "" {
		res.ID = TrialID(dir)
	}
	for _, f := range []struct{ path, text string }{{res.TrialFile, trialText}, {res.PromptFile, promptText}} {
		if _, err := os.Stat(f.path); err == nil && !o.Force {
			res.Kept = append(res.Kept, f.path)
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.text), 0o644); err != nil {
			return nil, err
		}
		res.Written = append(res.Written, f.path)
	}
	if _, err := spec.Load(res.TrialFile, spec.LoadOpts{SkipReadiness: true}); err != nil {
		res.Problem = err.Error()
	} else {
		res.Valid = true
	}
	return res, nil
}

// Missing lists the required settings that are still empty (flag names).
func Missing(o Options) []string {
	var out []string
	if o.Dir == "" {
		out = append(out, "dir")
	}
	if o.Product == "" {
		out = append(out, "--product")
	}
	return out
}

// ReporterChoices are the harnesses that can report, sorted.
func ReporterChoices(all []HarnessInfo) []string {
	var out []string
	for _, h := range all {
		if h.CanReport {
			out = append(out, h.Name)
		}
	}
	sort.Strings(out)
	return out
}

const trialTemplate = `# AJX trial file. Everything the matrix needs, in one place. See examples/trial.toml.
[trial]
id = "{id}"
product = "{product}"
product_version_cmd = ""          # e.g. "mytool --version"; recorded before and after each run
workspace_root = "/tmp"           # fresh unnamed workspace per run is created here (outside the report dir)
repetitions = 1
parallel = 1                      # >1 makes wall-clock incomparable (recorded)
timeout_seconds = 1800
narrate_timeout_seconds = 900     # for the resumed self-narration and reporter calls
# max_budget_usd = 5              # enforced by harnesses that support it (claude-code)
order_seed = 0
cache = "isolated"                # isolated | shared package caches
shell_startup = "inherit"         # skip: the worker's tool shell reads no ~/.zshenv or BASH_ENV (local runs)

[task]
prompt_file = "task-prompt.md"    # exact bytes are hashed; never edited by ajx
materials = []                    # URLs/docs the prompt points the agent to (recorded)
human = "unavailable"             # headless: a required human action is recorded as a Gate
# fixture_dir = "fixture"         # copied into every fresh workspace before setup
# identity_cmd = "aws sts get-caller-identity --profile my-sandbox"   # the account the worker acts in;
                                  # doctor runs it in the worker's own tool shell and fails if it errors

# [env]                           # worker env. Not for credentials the model also uses (Bedrock reads
# MY_VAR = "x"                    # AWS_*): doctor warns on collisions and on values the shell overrides

# [[preflight]]                   # must pass before each worker starts; same env as verify/teardown
# name = "task credentials work"
# run = "aws sts get-caller-identity --profile my-sandbox"

# [[setup]]                       # before the clock; recorded as preinstalled setup
# run = "git init -q"

# [[verify]]                      # deterministic checks run by ajx AFTER the agent stops
# name = "project created"
# type = "file_exists"            # shell | file_exists | http (plugins can add more)
# path = "*/package.json"

# [[teardown]]                    # always runs; $AJX_RUN_TOKEN is available for tagging/finding resources
# run = "true"

[reporter]                        # fixed across cells so report quality is not a confounder
harness = "{reporter}"
# model = "your-model"
# auth = "inherit"

[[cells]]
id = "{harness}-default"
harness = "{harness}"
# model = "your-model"            # or models = ["model-a", "model-b"]: one cell per model
config = "clean"                  # request supported isolation; inspect doctor/report limits; user = normal config
# auth = "inherit"                # or a [auth.*] profile name, or an auth type (ajx plugins)
# environment = "developer"      # named [environments.developer] profile
# agent_configuration = "plain"  # named [agent_configurations.plain] profile
`

const promptTemplate = `I want to try {product}.

<Describe the real outcome you want, as a customer would. Name the materials the agent may use
(for example the GitHub README URL). State the boundaries (account/profile/region, spend limit,
what to clean up). Do not mention evaluation, AJX, or expected problems.>
`

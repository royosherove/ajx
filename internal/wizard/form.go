package wizard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/royosherove/ajx/internal/modelcatalog"
)

// ErrAborted is returned when the user leaves the form (Esc / Ctrl-C).
var ErrAborted = errors.New("init cancelled; nothing was written")

// Next steps offered after writing.
const (
	NextConsole = "console"
	NextDoctor  = "doctor"
	NextExit    = "exit"
)

// IO lets tests drive the form in accessible (line-based) mode.
type IO struct {
	In         io.Reader
	Out        io.Writer
	Accessible bool
}

// apply sets the theme and IO. huh v2 (Bubble Tea v2) asks the terminal for
// its background only while a form runs and never blocks on the answer; the
// v1 libraries queried it in a package init on every start of the binary,
// which stalled each command 5 s in terminals that never reply.
func (x IO) apply(f *huh.Form) *huh.Form {
	keys := huh.NewDefaultKeyMap()
	keys.Text.NewLine.SetKeys("enter", "alt+enter", "ctrl+j")
	keys.Text.NewLine.SetHelp("enter", "new line")
	keys.Text.Next.SetKeys("tab")
	keys.Text.Next.SetHelp("tab", "next")
	keys.Text.Submit.SetKeys("tab")
	keys.Text.Submit.SetHelp("tab", "continue")
	f = f.WithTheme(huh.ThemeFunc(huh.ThemeCharm)).WithKeyMap(keys)
	if x.Accessible || os.Getenv("ACCESSIBLE") != "" {
		f = f.WithAccessible(true)
	}
	if x.In != nil {
		f = f.WithInput(x.In)
	}
	if x.Out != nil {
		f = f.WithOutput(x.Out)
	}
	return f
}

func intValidator(label string, allowEmpty bool) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" && allowEmpty {
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return fmt.Errorf("%s must be a whole number of at least 1", label)
		}
		return nil
	}
}

// productName keeps the product to a name; the task is asked separately.
func productName(s string) error {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return errors.New("the product is required")
	case len([]rune(s)) > 60:
		return errors.New("keep this to the product's name (for example Vercel CLI); describe the task in the next step")
	}
	return nil
}

func lines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.Contains(l, "://") {
			// Several links on one line are several materials; a line of prose with a link stays whole.
			parts := strings.FieldsFunc(l, func(r rune) bool { return r == ' ' || r == '\t' || r == ',' })
			allLinks := true
			for _, p := range parts {
				allLinks = allLinks && strings.Contains(p, "://")
			}
			if allLinks {
				out = append(out, parts...)
			} else {
				out = append(out, l)
			}
			continue
		}
		for _, p := range strings.Split(l, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// Ask fills o interactively. Values already set (from flags) are the
// defaults shown in the form. Each step is its own form, so steps that do
// not apply (a check value after "no check", an image for a local run) are
// skipped in both the full-screen and the accessible (line-based) mode.
func Ask(o *Options, x IO) error {
	all := Harnesses(true)
	if o.Harness == "" {
		o.Harness = "claude-code"
		for _, h := range all {
			if h.Installed && h.CanReport {
				o.Harness = h.Name
				break
			}
		}
	}
	harnessOpts := make([]huh.Option[string], 0, len(all))
	for _, h := range all {
		state := "not installed"
		if h.Installed {
			state = "installed"
			if h.Version != "" {
				state += " " + h.Version
			}
		}
		tags := []string{state}
		if h.Verified {
			tags = append(tags, "verified live")
		}
		if h.CanReport {
			tags = append(tags, "can report")
		}
		harnessOpts = append(harnessOpts, huh.NewOption(fmt.Sprintf("%-14s %s", h.Name, strings.Join(tags, ", ")), h.Name))
	}
	materials := strings.Join(o.Materials, "\n")
	checkKind, checkValue := "shell", ""
	if len(o.Checks) > 0 {
		c, _ := ParseCheck(o.Checks[0])
		checkValue = c.Value
		checkKind = map[string]string{"file_exists": "file", "http": "http"}[c.Type]
		if checkKind == "" {
			checkKind = "shell"
		}
	}
	timeout, reps := "", ""
	if o.Timeout > 0 {
		timeout = strconv.Itoa(o.Timeout)
	}
	if o.Repetitions > 0 {
		reps = strconv.Itoa(o.Repetitions)
	}
	env := o.Environment
	if env == "" {
		env = "local"
	}
	required := func(label string) func(string) error {
		return func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s is required", label)
			}
			return nil
		}
	}
	run := func(fields ...huh.Field) error {
		err := x.apply(huh.NewForm(huh.NewGroup(fields...))).Run()
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrAborted
		}
		return err
	}
	if err := run(
		huh.NewNote().Title("New AJX trial").
			Description("A fresh agent will attempt one real task with your product.\nAJX checks the result and reports what got in its way."),
		huh.NewInput().Title("Product").
			Description("The name of the CLI, SDK, API or project the agent will try, e.g. jq or Vercel CLI. The task comes next.").
			Value(&o.Product).Validate(productName),
	); err != nil {
		return err
	}
	if o.Dir == "" {
		def := filepath.Join("trials", slug(o.Product))
		if err := run(huh.NewInput().Title("Folder for this trial").
			Description("trial.toml and task-prompt.md are written here (default " + def + ")").Placeholder(def).Value(&o.Dir)); err != nil {
			return err
		}
		if strings.TrimSpace(o.Dir) == "" {
			o.Dir = def
		}
	}
	var taskField huh.Field
	if o.TaskFile != "" {
		taskField = huh.NewNote().Title("Task file").
			Description(o.TaskFile + "\nThe supplied prompt will be copied verbatim.")
	} else {
		taskField = huh.NewText().Title("What should the user accomplish?").
			Description("Write it as the customer would ask. Do not mention evaluation, AJX or expected problems.\nEnter: new line · Tab: continue").
			Lines(4).Value(&o.Task).Validate(required("the task"))
	}
	if err := run(
		taskField,
		huh.NewText().Title("Materials the worker may use").
			Description("One URL or document per line (optional). Paste a list or type it here.\nEnter: new line · Tab: continue").Lines(3).Value(&materials),
	); err != nil {
		return err
	}
	if err := run(
		huh.NewSelect[string]().Title("Worker agent").Description("The CLI that attempts the task").
			Options(harnessOpts...).Value(&o.Harness),
	); err != nil {
		return err
	}
	if err := askModel("Worker model", o.Harness, &o.Model, x); err != nil {
		return err
	}
	if o.Reporter == "" {
		o.Reporter = "claude-code"
		if _, cr := canReport(o.Harness); cr {
			o.Reporter = o.Harness
		}
	}
	var reporterOpts []huh.Option[string]
	for _, name := range ReporterChoices(all) {
		reporterOpts = append(reporterOpts, huh.NewOption(name, name))
	}
	if err := run(
		huh.NewSelect[string]().Title("Reporter").Description("Writes the report after the attempt; fixed across cells").
			Options(reporterOpts...).Value(&o.Reporter),
	); err != nil {
		return err
	}
	if err := askModel("Reporter model", o.Reporter, &o.ReporterModel, x); err != nil {
		return err
	}
	if err := run(
		huh.NewSelect[string]().Title("How does AJX check success?").Options(
			huh.NewOption("a shell command exits 0 (in the workspace)", "shell"),
			huh.NewOption("a file exists (glob beneath the workspace)", "file"),
			huh.NewOption("a URL responds", "http"),
			huh.NewOption("no check yet (add [[verify]] later)", "none"),
		).Value(&checkKind),
	); err != nil {
		return err
	}
	if checkKind != "none" {
		title := map[string]string{"shell": "Command", "file": "File glob", "http": "URL"}[checkKind]
		if err := run(huh.NewInput().Title(title).Value(&checkValue).Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("enter the check (or Esc and start again to choose no check)")
			}
			_, err := ParseCheck(checkPrefix(checkKind) + s)
			return err
		})); err != nil {
			return err
		}
	}
	if err := run(
		huh.NewInput().Title("Time limit for the worker (seconds)").Placeholder("1800").
			Value(&timeout).Validate(intValidator("the time limit", true)),
		huh.NewInput().Title("Attempts per cell").Placeholder("1").
			Value(&reps).Validate(intValidator("attempts", true)),
		huh.NewInput().Title("Starting files (optional)").Description("A folder copied into every fresh workspace").
			Value(&o.Fixture).Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return nil
			}
			if st, err := os.Stat(strings.TrimSpace(s)); err != nil || !st.IsDir() {
				return fmt.Errorf("%s is not a folder", s)
			}
			return nil
		}),
		huh.NewSelect[string]().Title("Where does the worker run?").Options(
			huh.NewOption("local: a fresh directory on this machine (no sandbox)", "local"),
			huh.NewOption("container: a prepared local Docker image", "container"),
		).Value(&env),
	); err != nil {
		return err
	}
	if env == "container" {
		if err := run(huh.NewInput().Title("Container image").
			Description("An immutable local image id (sha256:...) or name@sha256:digest").
			Value(&o.Image).Validate(required("the image"))); err != nil {
			return err
		}
	}
	o.Materials = lines(materials)
	o.Checks = nil
	if checkKind != "none" && strings.TrimSpace(checkValue) != "" {
		o.Checks = []string{checkPrefix(checkKind) + strings.TrimSpace(checkValue)}
	}
	o.Timeout, _ = strconv.Atoi(strings.TrimSpace(timeout))
	o.Repetitions, _ = strconv.Atoi(strings.TrimSpace(reps))
	o.Fixture = strings.TrimSpace(o.Fixture)
	o.Environment = env
	if env == "local" {
		o.Environment, o.Image = "", ""
	}
	return nil
}

func askModel(title, harness string, value *string, x IO) error {
	const custom = "__ajx_custom_model__"
	selected := strings.TrimSpace(*value)
	if resolved, err := modelcatalog.Resolve(harness, selected); err == nil {
		selected = resolved
	}
	options := []huh.Option[string]{huh.NewOption("Harness default (uses the agent's configured model)", "")}
	found := selected == ""
	for _, id := range modelcatalog.Models(harness) {
		options = append(options, huh.NewOption(id, id))
		found = found || id == selected
	}
	if !found {
		options = append(options, huh.NewOption("Keep current: "+selected, selected))
	}
	options = append(options, huh.NewOption("Custom model ID or alias…", custom))
	field := huh.NewSelect[string]().Title(title + " · " + harness).
		Description("Choose with ↑/↓, / to filter, Enter to continue. Suggestions depend on account access.\n" + modelcatalog.Hint(harness)).
		Height(12).Options(options...).Value(&selected).Validate(func(name string) error {
		if name == custom {
			return nil
		}
		if err := ModelName(name); err != nil {
			return err
		}
		_, err := modelcatalog.Resolve(harness, name)
		return err
	})
	if err := x.apply(huh.NewForm(huh.NewGroup(field))).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrAborted
		}
		return err
	}
	if selected == custom {
		field := huh.NewInput().Title(title + " · custom ID").
			Description(modelcatalog.Hint(harness)).Value(value).
			Validate(func(name string) error {
				if err := ModelName(name); err != nil {
					return err
				}
				_, err := modelcatalog.Resolve(harness, name)
				return err
			})
		if err := x.apply(huh.NewForm(huh.NewGroup(field))).Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return ErrAborted
			}
			return err
		}
		selected = *value
	}
	resolved, err := modelcatalog.Resolve(harness, selected)
	if err == nil {
		*value = resolved
	}
	return err
}

func checkPrefix(kind string) string {
	switch kind {
	case "file":
		return "file:"
	case "http":
		return "http:"
	}
	return "shell:"
}

func slug(s string) string { return Slug(s) }

// Confirm shows the summary and asks to write the files.
func Confirm(o Options, x IO) (bool, error) {
	ok := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Review").Description(Summary(o, false)),
		huh.NewConfirm().Title("Write trial.toml and task-prompt.md?").Affirmative("Write").Negative("Cancel").Value(&ok),
	))
	if err := x.apply(form).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// NextStep asks what to do after writing.
func NextStep(x IO) (string, error) {
	choice := NextConsole
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Next").Options(
			huh.NewOption("Open the AJX Console to review and start it", NextConsole),
			huh.NewOption("Run ajx doctor (readiness check, no model calls)", NextDoctor),
			huh.NewOption("Exit", NextExit),
		).Value(&choice),
	))
	if err := x.apply(form).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return NextExit, nil
		}
		return "", err
	}
	return choice, nil
}

// Summary is the plan as aligned text (styled when color is true).
func Summary(o Options, color bool) string {
	key := lipgloss.NewStyle().Bold(true)
	if !color {
		key = lipgloss.NewStyle()
	}
	orDefault := func(v, def string) string {
		if strings.TrimSpace(v) == "" {
			return def
		}
		return v
	}
	checks := "none yet: add a [[verify]] check before running"
	if len(o.Checks) > 0 {
		var parts []string
		for _, raw := range o.Checks {
			c, err := ParseCheck(raw)
			if err == nil {
				parts = append(parts, c.Type+": "+c.Value)
			}
		}
		checks = strings.Join(parts, "; ")
	}
	where := "local (fresh directory; not a sandbox)"
	if o.Environment == "container" {
		where = "container " + o.Image
	}
	rows := [][2]string{
		{"product", o.Product},
		{"folder", orDefault(o.Dir, filepath.Join("trials", slug(o.Product)))},
		{"task", orDefault(oneLine(o.Task, 70), "placeholder: describe it in task-prompt.md")},
		{"materials", orDefault(strings.Join(o.Materials, ", "), "none listed")},
		{"worker", o.Harness + " model=" + orDefault(o.Model, "harness default")},
		{"reporter", orDefault(o.Reporter, o.Harness) + " model=" + orDefault(o.ReporterModel, "harness default")},
		{"check", checks},
		{"time limit", orDefault(itoa(o.Timeout), "1800") + "s"},
		{"attempts", orDefault(itoa(o.Repetitions), "1")},
		{"start files", orDefault(o.Fixture, "none")},
		{"runs in", where},
	}
	if o.MaxBudgetUSD != nil {
		rows = append(rows, [2]string{"budget", "$" + strconv.FormatFloat(*o.MaxBudgetUSD, 'g', -1, 64) +
			" per worker; harness-dependent, excludes reporting"})
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(key.Render(fmt.Sprintf("%-12s", r[0])) + " " + r[1] + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func itoa(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit-3]) + "..."
	}
	return s
}

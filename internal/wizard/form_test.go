package wizard

import (
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/royosherove/ajx/internal/modelcatalog"
)

func TestMultilineFormEnterAddsLineAndTabContinues(t *testing.T) {
	for _, newline := range []struct {
		name, keys string
	}{
		{"enter", "\r"},
		{"ctrl-j", "\n"},
		{"alt-enter", "\x1b\r"},
	} {
		t.Run(newline.name, func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("ACCESSIBLE", "")
			var product, task, materials string
			productField := huh.NewInput().Value(&product)
			taskField := huh.NewText().Value(&task)
			materialsField := huh.NewText().Value(&materials)
			in, writer := io.Pipe()
			defer in.Close()
			defer writer.Close()
			inputs := make(chan string, 1)
			defer close(inputs)
			go func() {
				for keys := range inputs {
					if _, err := io.WriteString(writer, keys); err != nil {
						return
					}
				}
			}()
			f := (IO{In: in, Out: io.Discard}).apply(huh.NewForm(
				huh.NewGroup(productField, taskField, materialsField),
			).WithProgramOptions(tea.WithoutRenderer(), tea.WithoutSignalHandler())).
				WithTimeout(5 * time.Second)
			paste := func(s string) string { return "\x1b[200~" + s + "\x1b[201~" }
			steps := []struct {
				field huh.Field
				value string
				keys  string
			}{
				{productField, "", "orders\r"}, // Single-line Enter still advances.
				{taskField, "", paste("Export the orders.")},
				{taskField, "Export the orders.", newline.keys},
				{taskField, "Export the orders.\n", paste("Check the row count.")},
				{taskField, "Export the orders.\nCheck the row count.", "\t"},
				{materialsField, "", paste("https://example.com/quickstart")},
				{materialsField, "https://example.com/quickstart", newline.keys},
				{materialsField, "https://example.com/quickstart\n", paste("https://example.com/api")},
				{materialsField, "https://example.com/quickstart\nhttps://example.com/api", "\x1b[Z"},
				{taskField, "Export the orders.\nCheck the row count.", "\t"}, // Shift+Tab preserves input.
				{materialsField, "https://example.com/quickstart\nhttps://example.com/api", paste("\nhttps://example.com/faq\nhttps://example.com/examples")},
				{materialsField, "https://example.com/quickstart\nhttps://example.com/api\nhttps://example.com/faq\nhttps://example.com/examples", "\t"},
			}
			step := 0
			// Drive real terminal bytes only after the previous edit/navigation
			// has taken effect, without sleeps or depending on render timing.
			f.WithViewHook(func(v tea.View) tea.View {
				if step < len(steps) {
					want := steps[step]
					if f.GetFocusedField() == want.field && want.field.GetValue() == want.value {
						step++
						inputs <- want.keys
					}
				}
				return v
			})
			if err := f.Run(); err != nil {
				t.Fatalf("form stopped at step %d/%d: %v (task %q, materials %q)", step, len(steps), err, task, materials)
			}
			if step != len(steps) || f.State != huh.StateCompleted {
				t.Fatalf("form submitted early at step %d/%d", step, len(steps))
			}
			if product != "orders" || task != "Export the orders.\nCheck the row count." {
				t.Fatalf("input lost: product %q, task %q", product, task)
			}
			if got, want := lines(materials), []string{
				"https://example.com/quickstart", "https://example.com/api",
				"https://example.com/faq", "https://example.com/examples",
			}; !slices.Equal(got, want) {
				t.Fatalf("materials: got %q, want %q", got, want)
			}
		})
	}
}

func TestModelPickerOffersHarnessChoicesAndAcceptsCustomAliases(t *testing.T) {
	for _, harness := range []string{"codex", "kiro-cli", "gemini-cli", "claude-code", "cursor-agent", "copilot-cli"} {
		t.Run(harness, func(t *testing.T) {
			var out strings.Builder
			value := ""
			x := IO{In: &lineReader{lines: []string{""}}, Out: &out, Accessible: true}
			if err := askModel("Worker model", harness, &value, x); err != nil {
				t.Fatal(err)
			}
			for _, model := range modelcatalog.Models(harness) {
				if !strings.Contains(out.String(), model) {
					t.Errorf("picker did not show %s", model)
				}
			}
			if value != "" {
				t.Fatalf("agent default became %q", value)
			}
		})
	}
	for _, tc := range []struct{ harness, input, want string }{
		{"codex", "astra", "gpt-6-astra"},
		{"kiro-cli", "opus", "claude-opus-5.5"},
		{"claude-code", "gateway/MyModel", "gateway/MyModel"},
	} {
		var out strings.Builder
		value := ""
		x := IO{In: &lineReader{lines: []string{strconv.Itoa(len(modelcatalog.Models(tc.harness)) + 2), tc.input}},
			Out: &out, Accessible: true}
		if err := askModel("Reporter model", tc.harness, &value, x); err != nil || value != tc.want {
			t.Fatalf("%s custom model: got %q, %v; want %q\n%s", tc.harness, value, err, tc.want, out.String())
		}
	}
}

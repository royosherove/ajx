package reportui

import (
	"regexp"
	"strings"

	"github.com/royosherove/ajx/internal/render/pyfmt"
)

var numberedItem = regexp.MustCompile(`^[` + pyfmt.WS + `]*` + pyfmt.Digit + `+[.)][` + pyfmt.WS + `]+`)

// ChronologicalSteps returns the numbered steps in chronology sections, keeping
// each line unchanged so callers can retain or collapse whitespace for display.
func ChronologicalSteps(markdown string) [][]string {
	var steps [][]string
	var block []string
	inChronology := false
	flush := func() {
		if len(block) > 0 {
			steps = append(steps, block)
			block = nil
		}
	}
	for _, line := range pyfmt.SplitLines(markdown) {
		if strings.HasPrefix(line, "## ") {
			flush()
			inChronology = strings.Contains(strings.ToLower(line), "chronolog")
		} else if inChronology {
			numbered := numberedItem.MatchString(line)
			if numbered {
				flush()
			}
			if len(block) > 0 || numbered {
				block = append(block, line)
			}
		}
	}
	flush()
	return steps
}

// StepContent removes a step's number and leading Markdown emphasis markers.
func StepContent(step string) string {
	return strings.TrimLeft(numberedItem.ReplaceAllString(step, ""), "* ")
}

// StartsWithLabel recognizes an optional icon and bold marker before a label,
// using the same Unicode word boundary as the journey's Python implementation.
func StartsWithLabel(text, icon, name string) bool {
	try := func(s string) bool {
		s = strings.TrimPrefix(s, "**")
		if !strings.HasPrefix(s, name) {
			return false
		}
		r, size := pyfmt.RuneAt(s, len(name))
		return size == 0 || !pyfmt.IsWord(r)
	}
	if rest, ok := strings.CutPrefix(text, icon); ok && try(pyfmt.LStrip(rest)) {
		return true
	}
	return try(text)
}

// EventReferences finds E- references with at least three Unicode decimal
// digits, bounded by non-word characters. It preserves order and duplicates.
func EventReferences(text string) []string {
	var out []string
	for i := 0; i+1 < len(text); i++ {
		if text[i] != 'E' || text[i+1] != '-' {
			continue
		}
		if r, size := pyfmt.RuneBefore(text, i); size > 0 && pyfmt.IsWord(r) {
			continue
		}
		j, count := i+2, 0
		for j < len(text) {
			r, size := pyfmt.RuneAt(text, j)
			if !pyfmt.IsDigit(r) {
				break
			}
			j += size
			count++
		}
		if count < 3 {
			continue
		}
		if r, size := pyfmt.RuneAt(text, j); size > 0 && pyfmt.IsWord(r) {
			continue
		}
		out = append(out, text[i:j])
		i = j - 1
	}
	return out
}

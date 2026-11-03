package cli

import (
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// pythonPluginProblems names the Python plugins a trial needs: the Go CLI
// registers them as placeholders that cannot run (see plugin.Load).
func pythonPluginProblems(sp util.M, cell util.M) []string {
	var out []string
	add := func(kind, name string) {
		src := plugin.Source(kind, name)
		if path, ok := strings.CutPrefix(src, "python:"); ok && name != "" {
			msg := (&plugin.PythonPluginError{Kind: kind, Name: name, Path: path}).Error()
			if !util.Contains(out, msg) {
				out = append(out, msg)
			}
		}
	}
	cells := util.Maps(sp["cells"])
	if cell != nil {
		cells = []util.M{cell}
	} else {
		add("harness", util.S(util.Get(sp, "reporter", "harness")))
	}
	for _, c := range cells {
		add("harness", util.S(c["harness"]))
	}
	if cell == nil {
		for _, v := range util.Maps(sp["verify"]) {
			add("check", util.S(v["type"]))
		}
	}
	return out
}

package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/util"
)

var documentationTopics = []struct{ name, path string }{
	{"guide", "SKILL.md"},
	{"trial", "examples/trial.toml"},
	{"checks", "references/headless.md"},
	{"reporting", "references/reporting.md"},
	{"measurement", "references/measurement.md"},
}

func cmdDocs(ns *Namespace) (int, error) {
	topic := ns.Str("topic")
	if topic == "" {
		topics := []any{}
		for _, item := range documentationTopics {
			topics = append(topics, util.M{"topic": item.name, "path": item.path})
			if !ns.Bool("json") {
				outf("ajx docs %-12s %s\n", item.name, item.path)
			}
		}
		if ns.Bool("json") {
			emitJSON(util.M{"schema_version": SchemaVersion, "command": "docs", "topics": topics,
				"source": res.Root(), "quickstart": "https://ajx.dev/agents/"})
		} else {
			outln("Documents come from this CLI's bundled skill. Public quickstart: https://ajx.dev/agents/")
		}
		return 0, nil
	}
	for _, item := range documentationTopics {
		if item.name != topic {
			continue
		}
		content, err := res.ReadFile(item.path)
		if err != nil {
			return 1, fmt.Errorf("read documentation %s: %w", topic, err)
		}
		source := strings.TrimRight(res.Root(), "/") + "/" + filepath.ToSlash(item.path)
		if ns.Bool("json") {
			emitJSON(util.M{"schema_version": SchemaVersion, "command": "docs", "topic": topic,
				"source": source, "content": string(content)})
		} else {
			fmt.Fprint(Stdout, string(content))
		}
		return 0, nil
	}
	return 2, usageErr(ns, "unknown documentation topic "+topic+"; run `ajx docs` to list topics")
}

package cli

import (
	"fmt"
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func cmdPlugins(ns *Namespace) (int, error) {
	plugin.Load("")
	jsonMode := ns.Bool("json")
	doc := util.M{"schema_version": SchemaVersion, "command": "plugins", "notes": strs(plugin.LoadNotes())}
	for _, kind := range plugin.Kinds {
		if !jsonMode {
			outf("%s:\n", kind)
		}
		items := []any{}
		for _, name := range plugin.Names(kind) {
			extra := ""
			item := util.M{"name": name, "source": plugin.Source(kind, name)}
			switch kind {
			case "harness":
				h, err := plugin.NewHarness(name)
				if err != nil {
					return 1, err
				}
				b := h.Base()
				avail := h.Available()
				extra = fmt.Sprintf("  installed=%s verified_live=%s resume=%s reporter=%s clean=%s default_auth=%s",
					yesNo(avail, "no"), pyBool(b.VerifiedLive), pyBool(b.CanResume), pyBool(b.CanReport),
					pyBool(b.CleanSupported), b.DefaultAuthName())
				item["installed"] = avail
				item["verified_live"] = b.VerifiedLive
				item["resume"] = b.CanResume
				item["reporter"] = b.CanReport
				item["clean"] = b.CleanSupported
				item["default_auth"] = b.DefaultAuthName()
				item["telemetry"] = b.TelemetrySummary()
			case "auth":
				a, err := plugin.NewAuth(name, util.M{})
				if err != nil {
					return 1, err
				}
				b := a.Base()
				extra = fmt.Sprintf("  for=%s  %s", strings.Join(b.Harnesses, ","), b.Description)
				item["for"] = strs(b.Harnesses)
				item["description"] = b.Description
			}
			if !jsonMode {
				outf("  %-22s%s\n", name, extra)
			}
			items = append(items, item)
		}
		doc[kind] = items
	}
	if jsonMode {
		emitJSON(doc)
	}
	return 0, nil
}

// ---------------------------------------------------------------- completion

func cmdCompletion(ns *Namespace) (int, error) {
	shell := ns.Str("shell")
	p := BuildParser()
	script, ok := completionScript(p, shell)
	if !ok {
		return 2, usageErr(ns, fmt.Sprintf("argument shell: invalid choice: %s (choose from 'bash', 'zsh', 'fish')", pyRepr(shell)))
	}
	fmt.Fprint(Stdout, script)
	return 0, nil
}

type compCmd struct {
	path  []string
	flags []string
	subs  []string
}

func walk(p *Parser, path []string, out *[]compCmd) {
	c := compCmd{path: path}
	for _, a := range p.args {
		if a.Hidden {
			continue
		}
		if a.Kind == parsers {
			c.subs = append(c.subs, a.sub.names...)
			for _, name := range a.sub.names {
				walk(a.sub.parsers[name], append(append([]string{}, path...), name), out)
			}
			continue
		}
		c.flags = append(c.flags, a.Flags...)
	}
	*out = append(*out, c)
}

func completionScript(p *Parser, shell string) (string, bool) {
	var cmds []compCmd
	walk(p, nil, &cmds)
	words := func(c compCmd) string { return strings.Join(append(append([]string{}, c.subs...), c.flags...), " ") }
	switch shell {
	case "bash", "zsh":
		var b strings.Builder
		if shell == "zsh" {
			b.WriteString("#compdef ajx\nautoload -U +X bashcompinit && bashcompinit\n")
		}
		b.WriteString("# ajx completion. Load with: source <(ajx completion " + shell + ")\n")
		b.WriteString("_ajx() {\n  local cur=${COMP_WORDS[COMP_CWORD]} path=\"\" w i\n")
		b.WriteString("  for ((i=1; i<COMP_CWORD; i++)); do\n    w=${COMP_WORDS[i]}\n    case \"${path:+$path }$w\" in\n")
		var known []string
		for _, c := range cmds {
			if len(c.path) > 0 {
				known = append(known, "\""+strings.Join(c.path, " ")+"\"")
			}
		}
		b.WriteString("      " + strings.Join(known, "|") + ") path=\"${path:+$path }$w\";;\n")
		b.WriteString("    esac\n  done\n  local opts=\"\"\n  case \"$path\" in\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "    \"%s\") opts=\"%s\";;\n", strings.Join(c.path, " "), words(c))
		}
		b.WriteString("  esac\n  COMPREPLY=($(compgen -W \"$opts\" -- \"$cur\"))\n")
		b.WriteString("  if [[ ${#COMPREPLY[@]} -eq 0 ]]; then COMPREPLY=($(compgen -f -- \"$cur\")); fi\n  return 0\n}\n")
		b.WriteString("complete -o filenames -F _ajx ajx\n")
		return b.String(), true
	case "fish":
		var b strings.Builder
		b.WriteString("# ajx completion (fish). Load with: ajx completion fish | source\n")
		b.WriteString("complete -c ajx -f\n")
		for _, c := range cmds {
			cond := "__fish_use_subcommand"
			if len(c.path) > 0 {
				var parts []string
				for _, w := range c.path {
					parts = append(parts, "__fish_seen_subcommand_from "+w)
				}
				cond = strings.Join(parts, "; and ")
			}
			for _, s := range c.subs {
				fmt.Fprintf(&b, "complete -c ajx -n '%s' -a %s\n", cond, s)
			}
			for _, f := range c.flags {
				if strings.HasPrefix(f, "--") {
					fmt.Fprintf(&b, "complete -c ajx -n '%s' -l %s\n", cond, strings.TrimPrefix(f, "--"))
				}
			}
			if len(c.path) > 0 && len(c.subs) == 0 {
				fmt.Fprintf(&b, "complete -c ajx -n '%s' -F\n", cond)
			}
		}
		return b.String(), true
	}
	return "", false
}

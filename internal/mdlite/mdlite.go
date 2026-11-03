// Package mdlite ports lib/ajx/mdlite.py: a minimal, dependency-free markdown
// to HTML renderer for AJX reports.
//
// Supports headings, paragraphs, fenced code, blockquotes, ordered/unordered
// lists, pipe tables, horizontal rules, inline code, bold, italic, and links.
// All text is escaped. Python's \s, \w and \d are Unicode-aware; the patterns
// here use the same character classes, and the italic rule (which needs
// lookbehind/lookahead) is matched by hand.
package mdlite

import (
	"fmt"
	"html"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/royosherove/ajx/internal/render/pyfmt"
)

var (
	inlineCode = regexp.MustCompile("`([^`]+)`")
	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\(([^)` + pyfmt.WS + `]+)\)`)
	boldRe     = regexp.MustCompile(`\*\*(.+?)\*\*`)
	stashRe    = regexp.MustCompile("\x00([0-9]+)\x00")
	headingRe  = regexp.MustCompile(`^(#{1,6})[` + pyfmt.WS + `]+(.*)$`)
	ruleRe     = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
	tableSep   = regexp.MustCompile(`^\|?[` + pyfmt.WS + `]*:?-{2,}`)
	itemRe     = regexp.MustCompile(`^([` + pyfmt.WS + `]*)([-*+]|` + pyfmt.Digit + `+[.)])[` + pyfmt.WS + `]+(.*)$`)
	slugRe     = regexp.MustCompile(`[^a-z0-9]+`)
)

func slug(text string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(text), "-"), "-")
}

// Inline renders inline markdown (code, links, bold, italic); all text is escaped.
func Inline(text string) string {
	var codes []string
	text = inlineCode.ReplaceAllStringFunc(text, func(m string) string {
		codes = append(codes, "<code>"+pyfmt.Escape(m[1:len(m)-1], true)+"</code>")
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})
	text = pyfmt.Escape(text, false)
	text = linkRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		href := sub[2]
		if !allowedHref(href) {
			href = "#"
		}
		return `<a href="` + pyfmt.Escape(href, true) + `">` + sub[1] + "</a>"
	})
	text = boldRe.ReplaceAllString(text, "<strong>$1</strong>")
	text = italic(text)
	return stashRe.ReplaceAllStringFunc(text, func(m string) string {
		i, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || i >= len(codes) {
			return m
		}
		return codes[i]
	})
}

// italic is re.sub(r"(?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*])", r"<em>\1</em>", text).
func italic(text string) string {
	r := []rune(text)
	n := len(r)
	wordOrStar := func(c rune) bool { return c == '*' || pyfmt.IsWord(c) }
	var b strings.Builder
	last := 0
	for i := 0; i < n; i++ {
		if r[i] != '*' || (i > 0 && wordOrStar(r[i-1])) || i+1 >= n || pyfmt.IsSpace(r[i+1]) {
			continue
		}
		end := -1
		for j := i + 2; j < n; j++ {
			if r[j-1] == '\n' {
				break // '.' does not cross a newline
			}
			if r[j] == '*' && !pyfmt.IsSpace(r[j-1]) && (j+1 >= n || !wordOrStar(r[j+1])) {
				end = j
				break
			}
		}
		if end < 0 {
			continue
		}
		b.WriteString(string(r[last:i]))
		b.WriteString("<em>" + string(r[i+1:end]) + "</em>")
		last = end + 1
		i = end
	}
	if last == 0 {
		return text
	}
	b.WriteString(string(r[last:]))
	return b.String()
}

// allowedHref is urlsplit(html.unescape(href)).scheme.lower() in ("", "http",
// "https"), with the ValueError cases (malformed IPv6 hosts) refused.
func allowedHref(href string) bool {
	scheme, err := urlScheme(html.UnescapeString(href))
	if err != nil {
		return false
	}
	return scheme == "" || scheme == "http" || scheme == "https"
}

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// urlScheme follows Python's urllib.parse.urlsplit far enough to return the
// scheme and the errors it raises for a bad network location.
func urlScheme(url string) (string, error) {
	url = strings.TrimLeftFunc(url, func(r rune) bool { return r <= ' ' })
	for _, c := range []string{"\t", "\r", "\n"} {
		url = strings.ReplaceAll(url, c, "")
	}
	scheme := ""
	if i := strings.Index(url, ":"); i > 0 && url[0] < 0x80 &&
		(url[0] >= 'a' && url[0] <= 'z' || url[0] >= 'A' && url[0] <= 'Z') {
		ok := true
		for _, c := range url[:i] {
			if !strings.ContainsRune(schemeChars, c) {
				ok = false
				break
			}
		}
		if ok {
			scheme, url = strings.ToLower(url[:i]), url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		rest := url[2:]
		netloc := rest
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			netloc = rest[:j]
		}
		open, closed := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
		if open != closed {
			return "", fmt.Errorf("Invalid IPv6 URL")
		}
		if open && closed {
			if err := checkBracketedNetloc(netloc); err != nil {
				return "", err
			}
		}
	}
	return scheme, nil
}

var ipvFuture = regexp.MustCompile(`^[vV][a-fA-F0-9]+\.(?s:.+)$`)

func checkBracketedNetloc(netloc string) error {
	hostPort := netloc
	if i := strings.LastIndex(netloc, "@"); i >= 0 {
		hostPort = netloc[i+1:]
	}
	var hostname string
	if before, bracketed, ok := strings.Cut(hostPort, "["); ok {
		if before != "" {
			return fmt.Errorf("Invalid IPv6 URL")
		}
		host, port, _ := strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return fmt.Errorf("Invalid IPv6 URL")
		}
		hostname = host
	} else {
		hostname, _, _ = strings.Cut(hostPort, ":")
	}
	if strings.HasPrefix(hostname, "v") || strings.HasPrefix(hostname, "V") {
		if !ipvFuture.MatchString(hostname) {
			return fmt.Errorf("IPvFuture address is invalid")
		}
		return nil
	}
	addr, err := netip.ParseAddr(hostname)
	if err != nil {
		return fmt.Errorf("%s does not appear to be an IPv4 or IPv6 address", pyfmt.StrRepr(hostname))
	}
	if addr.Is4() {
		return fmt.Errorf("An IPv4 address cannot be in brackets")
	}
	return nil
}

func splitCells(row string) []string {
	row = strings.Trim(pyfmt.Strip(row), "|")
	parts := strings.Split(row, "|")
	for i, c := range parts {
		parts[i] = pyfmt.Strip(c)
	}
	return parts
}

func table(rows []string) string {
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = splitCells(r)
	}
	var b strings.Builder
	b.WriteString(`<div class="table-wrap"><table><thead><tr>`)
	for _, c := range cells[0] {
		b.WriteString("<th>" + Inline(c) + "</th>")
	}
	b.WriteString("</tr></thead><tbody>")
	if len(cells) > 2 {
		for _, row := range cells[2:] {
			b.WriteString("<tr>")
			for _, c := range row {
				b.WriteString("<td>" + Inline(c) + "</td>")
			}
			b.WriteString("</tr>")
		}
	}
	b.WriteString("</tbody></table></div>")
	return b.String()
}

func isDigitRune(s string) bool {
	for _, r := range s {
		return pyfmt.IsDigit(r)
	}
	return false
}

// Render converts markdown to HTML.
func Render(markdown string) string {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var out, para []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, "<p>"+Inline(strings.Join(para, " "))+"</p>")
			para = para[:0]
		}
	}
	i := 0
	for i < len(lines) {
		line := lines[i]
		stripped := pyfmt.Strip(line)
		if strings.HasPrefix(stripped, "<!--") && strings.HasSuffix(stripped, "-->") {
			flush()
			i++
			continue
		}
		if strings.HasPrefix(stripped, "```") {
			flush()
			lang := pyfmt.Strip(stripped[3:])
			var block []string
			i++
			for i < len(lines) && !strings.HasPrefix(pyfmt.Strip(lines[i]), "```") {
				block = append(block, lines[i])
				i++
			}
			cls := ""
			if lang != "" {
				cls = ` class="lang-` + pyfmt.Escape(lang, true) + `"`
			}
			out = append(out, "<pre><code"+cls+">"+pyfmt.Escape(strings.Join(block, "\n"), true)+"</code></pre>")
			i++
			continue
		}
		if m := headingRe.FindStringSubmatch(stripped); m != nil {
			flush()
			level, text := len(m[1]), pyfmt.Strip(m[2])
			out = append(out, fmt.Sprintf(`<h%d id="%s">%s</h%d>`, level, slug(text), Inline(text), level))
			i++
			continue
		}
		if ruleRe.MatchString(stripped) {
			flush()
			out = append(out, "<hr>")
			i++
			continue
		}
		if strings.HasPrefix(stripped, "|") && i+1 < len(lines) && tableSep.MatchString(pyfmt.Strip(lines[i+1])) {
			flush()
			var rows []string
			for i < len(lines) && strings.HasPrefix(pyfmt.Strip(lines[i]), "|") {
				rows = append(rows, lines[i])
				i++
			}
			out = append(out, table(rows))
			continue
		}
		if strings.HasPrefix(stripped, ">") {
			flush()
			var quote []string
			for i < len(lines) && strings.HasPrefix(pyfmt.Strip(lines[i]), ">") {
				quote = append(quote, pyfmt.LStrip(pyfmt.Strip(lines[i])[1:]))
				i++
			}
			out = append(out, "<blockquote>"+Render(strings.Join(quote, "\n"))+"</blockquote>")
			continue
		}
		if m := itemRe.FindStringSubmatch(line); m != nil {
			flush()
			tag := "ul"
			if isDigitRune(m[2]) {
				tag = "ol"
			}
			var items []string
			for i < len(lines) {
				if mm := itemRe.FindStringSubmatch(lines[i]); mm != nil {
					items = append(items, mm[3])
				} else if pyfmt.Strip(lines[i]) != "" && (strings.HasPrefix(lines[i], "  ") || strings.HasPrefix(lines[i], "\t")) && len(items) > 0 {
					items[len(items)-1] += " " + pyfmt.Strip(lines[i])
				} else {
					break
				}
				i++
			}
			var b strings.Builder
			b.WriteString("<" + tag + ">")
			for _, t := range items {
				b.WriteString("<li>" + Inline(t) + "</li>")
			}
			b.WriteString("</" + tag + ">")
			out = append(out, b.String())
			continue
		}
		if stripped == "" {
			flush()
		} else {
			para = append(para, stripped)
		}
		i++
	}
	flush()
	return strings.Join(out, "\n")
}

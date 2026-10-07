"""Minimal, dependency-free markdown to HTML renderer for AJX reports.

Supports headings, paragraphs, fenced code, blockquotes, ordered/unordered lists,
pipe tables, horizontal rules, inline code, bold, italic, and links. All text is escaped.
"""

import html
import re
from urllib.parse import urlsplit

_INLINE_CODE = re.compile(r"`([^`]+)`")
_LINK = re.compile(r"\[([^\]]+)\]\(([^)\s]+)\)")
_BOLD = re.compile(r"\*\*(.+?)\*\*")
_ITALIC = re.compile(r"(?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*])")


def _slug(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def inline(text):
    codes = []

    def stash(match):
        codes.append("<code>" + html.escape(match.group(1)) + "</code>")
        return f"\x00{len(codes) - 1}\x00"

    text = _INLINE_CODE.sub(stash, text)
    text = html.escape(text, quote=False)

    def link(match):
        href = match.group(2)
        try:
            allowed = urlsplit(html.unescape(href)).scheme.lower() in ("", "http", "https")
        except ValueError:
            allowed = False
        if not allowed:
            href = "#"
        return f'<a href="{html.escape(href)}">{match.group(1)}</a>'

    text = _LINK.sub(link, text)
    text = _BOLD.sub(r"<strong>\1</strong>", text)
    text = _ITALIC.sub(r"<em>\1</em>", text)
    return re.sub("\x00(\\d+)\x00", lambda m: codes[int(m.group(1))], text)


def _table(rows):
    cells = [[c.strip() for c in r.strip().strip("|").split("|")] for r in rows]
    head, body = cells[0], cells[2:]
    out = ["<div class=\"table-wrap\"><table><thead><tr>"]
    out += [f"<th>{inline(c)}</th>" for c in head]
    out.append("</tr></thead><tbody>")
    for row in body:
        out.append("<tr>" + "".join(f"<td>{inline(c)}</td>" for c in row) + "</tr>")
    out.append("</tbody></table></div>")
    return "".join(out)


def render(markdown):
    lines = markdown.replace("\r\n", "\n").split("\n")
    out, i = [], 0
    para = []

    def flush():
        if para:
            out.append("<p>" + inline(" ".join(para)) + "</p>")
            para.clear()

    while i < len(lines):
        line = lines[i]
        stripped = line.strip()
        if stripped.startswith("<!--") and stripped.endswith("-->"):
            flush()
            i += 1
            continue
        if stripped.startswith("```"):
            flush()
            lang = stripped[3:].strip()
            block = []
            i += 1
            while i < len(lines) and not lines[i].strip().startswith("```"):
                block.append(lines[i])
                i += 1
            cls = f' class="lang-{html.escape(lang)}"' if lang else ""
            out.append(f"<pre><code{cls}>" + html.escape("\n".join(block)) + "</code></pre>")
            i += 1
            continue
        heading = re.match(r"^(#{1,6})\s+(.*)$", stripped)
        if heading:
            flush()
            level, text = len(heading.group(1)), heading.group(2).strip()
            out.append(f'<h{level} id="{_slug(text)}">{inline(text)}</h{level}>')
            i += 1
            continue
        if re.match(r"^(-{3,}|\*{3,}|_{3,})$", stripped):
            flush()
            out.append("<hr>")
            i += 1
            continue
        if stripped.startswith("|") and i + 1 < len(lines) and re.match(
                r"^\|?\s*:?-{2,}", lines[i + 1].strip()):
            flush()
            rows = []
            while i < len(lines) and lines[i].strip().startswith("|"):
                rows.append(lines[i])
                i += 1
            out.append(_table(rows))
            continue
        if stripped.startswith(">"):
            flush()
            quote = []
            while i < len(lines) and lines[i].strip().startswith(">"):
                quote.append(lines[i].strip()[1:].lstrip())
                i += 1
            out.append("<blockquote>" + render("\n".join(quote)) + "</blockquote>")
            continue
        item = re.match(r"^(\s*)([-*+]|\d+[.)])\s+(.*)$", line)
        if item:
            flush()
            ordered = item.group(2)[0].isdigit()
            tag = "ol" if ordered else "ul"
            items = []
            while i < len(lines):
                m = re.match(r"^(\s*)([-*+]|\d+[.)])\s+(.*)$", lines[i])
                if m:
                    items.append(m.group(3))
                elif lines[i].strip() and lines[i].startswith(("  ", "\t")) and items:
                    items[-1] += " " + lines[i].strip()
                else:
                    break
                i += 1
            out.append(f"<{tag}>" + "".join(f"<li>{inline(t)}</li>" for t in items) + f"</{tag}>")
            continue
        if not stripped:
            flush()
        else:
            para.append(stripped)
        i += 1
    flush()
    return "\n".join(out)

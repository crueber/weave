package cards

import (
	"html"
	"html/template"
	"regexp"
	"strings"

	"github.com/crueber/weave/internal/realtime"
)

func rtEvent(name, room string, data any) realtime.Event {
	return realtime.Event{Name: name, Room: room, Data: data}
}

var (
	reCode   = regexp.MustCompile("`([^`\n]+)`")
	reBold   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reItalic = regexp.MustCompile(`(^|[^*\w])[*_]([^*_\n]+)[*_]($|[^*\w])`)
	reStrike = regexp.MustCompile(`~~([^~\n]+)~~`)
	reLink   = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+|mailto:[^\s)]+)\)`)
	reHead   = regexp.MustCompile(`^(#{1,3})\s+(.*)$`)
	reUL     = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	reOL     = regexp.MustCompile(`^\s*\d+[.)]\s+(.*)$`)
	reTask   = regexp.MustCompile(`^\[([ xX])\]\s+(.*)$`)
)

func inline(s string) string {
	s = html.EscapeString(s)
	s = reCode.ReplaceAllString(s, "<code>$1</code>")
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reItalic.ReplaceAllString(s, "$1<em>$2</em>$3")
	s = reStrike.ReplaceAllString(s, "<del>$1</del>")
	s = reLink.ReplaceAllString(s, `<a href="$2" target="_blank" rel="noopener nofollow ugc">$1</a>`)
	return s
}

// RenderMarkdown renders the markdown-lite subset used by card descriptions:
// headings, bold, italic, strike, inline code, fenced code, links (http/https/mailto),
// bullet/numbered/task lists and quotes. Input is HTML-escaped first, so the output is safe.
func RenderMarkdown(src string) template.HTML {
	var b strings.Builder
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var para []string
	list := ""
	flushPara := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = nil
		}
	}
	closeList := func() {
		if list != "" {
			b.WriteString("</" + list + ">")
			list = ""
		}
	}
	openList := func(t string) {
		if list != t {
			closeList()
			b.WriteString("<" + t + ">")
			list = t
		}
	}
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			flushPara()
			closeList()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
			continue
		}
		if strings.TrimSpace(ln) == "" {
			flushPara()
			closeList()
			continue
		}
		if m := reHead.FindStringSubmatch(ln); m != nil {
			flushPara()
			closeList()
			n := string(rune('0' + len(m[1]) + 2))
			b.WriteString("<h" + n + ">" + inline(m[2]) + "</h" + n + ">")
			continue
		}
		if m := reUL.FindStringSubmatch(ln); m != nil {
			flushPara()
			openList("ul")
			if t := reTask.FindStringSubmatch(m[1]); t != nil {
				chk := ""
				if t[1] != " " {
					chk = " checked"
				}
				b.WriteString(`<li class="cards-task"><input type="checkbox" disabled` + chk + "> " + inline(t[2]) + "</li>")
			} else {
				b.WriteString("<li>" + inline(m[1]) + "</li>")
			}
			continue
		}
		if m := reOL.FindStringSubmatch(ln); m != nil {
			flushPara()
			openList("ol")
			b.WriteString("<li>" + inline(m[1]) + "</li>")
			continue
		}
		if strings.HasPrefix(ln, ">") {
			flushPara()
			closeList()
			b.WriteString("<blockquote>" + inline(strings.TrimSpace(strings.TrimPrefix(ln, ">"))) + "</blockquote>")
			continue
		}
		closeList()
		para = append(para, inline(ln))
	}
	flushPara()
	closeList()
	return template.HTML(b.String())
}

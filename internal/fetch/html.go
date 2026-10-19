package fetch

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// skipped elements hold no readable content.
var skipped = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "template": true, "iframe": true, "canvas": true, "head": true}

// blocks start a new line in the text.
var blocks = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "header": true, "footer": true, "main": true, "aside": true, "nav": true,
	"ul": true, "ol": true, "li": true, "table": true, "tr": true, "blockquote": true, "pre": true, "figure": true, "figcaption": true,
	"form": true, "fieldset": true, "details": true, "summary": true, "dl": true, "dt": true, "dd": true, "address": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "br": true, "hr": true,
}

// HTMLText extracts a page's title and readable text, keeping headings and
// list items recognizable (as Markdown-like "#" and "-").
func HTMLText(doc string) (title, text string) {
	title, text, _ = htmlDoc(doc)
	return title, text
}

// seeRE matches text that introduces a cross-reference ("See", "refer to",
// "described in"...), so the link after it points to where a rule continues.
var seeRE = regexp.MustCompile(`(?i)\b(see|refer to|found in|described in|listed in|defined in|details in|per)(\s+the)?[\s:]*$`)

// htmlDoc is HTMLText plus the page's cross-references: the links (as
// written, not resolved) that follow "See ..." in the text. Navigation links
// are not preceded by such words, so they are left out.
func htmlDoc(doc string) (title, text string, refs []string) {
	z := html.NewTokenizer(strings.NewReader(doc))
	var b strings.Builder
	href, anchorAt := "", -1 // the open <a>'s address and where its text starts
	skip := 0                // depth inside skipped elements
	svg := 0                 // depth inside <svg>: icons carry their own <title>
	inTitle, inPre := false, false
	newline := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return strings.TrimSpace(title), tidy(b.String()), refs
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			if tag == "title" {
				// Only the document's own title, not an icon's.
				inTitle = svg == 0 && title == "" && tt == html.StartTagToken
				continue
			}
			if skipped[tag] {
				if tt == html.StartTagToken {
					skip++
					if tag == "svg" {
						svg++
					}
				}
				continue
			}
			if skip > 0 {
				continue
			}
			if blocks[tag] {
				newline()
			}
			switch tag {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString(strings.Repeat("#", int(tag[1]-'0')) + " ")
			case "li":
				b.WriteString("- ")
			case "pre":
				inPre = true
			case "a":
				href, anchorAt = "", -1
				for more := hasAttr; more; {
					var k, v []byte
					k, v, more = z.TagAttr()
					if string(k) == "href" {
						href = strings.TrimSpace(string(v))
					}
				}
				if href != "" {
					before := b.String()
					before = before[max(len(before)-40, 0):]
					if seeRE.MatchString(before) {
						anchorAt = b.Len()
					}
				}
			case "td", "th":
				b.WriteString(" | ")
			case "img":
				for more := hasAttr; more; {
					var k, v []byte
					k, v, more = z.TagAttr()
					if string(k) == "alt" && strings.TrimSpace(string(v)) != "" {
						b.WriteString("[image: " + strings.TrimSpace(string(v)) + "] ")
					}
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case tag == "a":
				if anchorAt >= 0 && skip == 0 && strings.TrimSpace(b.String()[anchorAt:]) != "" {
					refs = append(refs, href)
				}
				href, anchorAt = "", -1
			case tag == "title":
				inTitle = false
			case skipped[tag]:
				if skip > 0 {
					skip--
				}
				if tag == "svg" && svg > 0 {
					svg--
				}
			case skip == 0 && blocks[tag]:
				if tag == "pre" {
					inPre = false
				}
				newline()
			}
		case html.TextToken:
			t := string(z.Text())
			if inTitle {
				title += t
				continue
			}
			if skip > 0 {
				continue
			}
			if inPre {
				b.WriteString(t)
				continue
			}
			t = strings.Join(strings.Fields(t), " ")
			if t == "" {
				continue
			}
			if s := b.String(); len(s) > 0 && !strings.HasSuffix(s, "\n") && !strings.HasSuffix(s, " ") {
				b.WriteString(" ")
			}
			b.WriteString(t)
		}
	}
}

// tidy trims lines and collapses runs of blank lines.
func tidy(s string) string {
	var out []string
	blank := false
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, " ")
		if strings.TrimSpace(l) == "" || strings.TrimSpace(l) == "-" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

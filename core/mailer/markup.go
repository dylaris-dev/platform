package mailer

import (
	"html"
	"strings"
)

// The body markup an operator writes.
//
// Deliberately NOT markdown and deliberately NOT raw HTML.
//
// Raw HTML would need a sanitiser, which is a dependency and a permanent source
// of "why does this look wrong in Outlook" - email HTML is its own dialect of
// tables and inline styles, and a paste from a web page produces none of it.
// Markdown would need a parser dependency for a vocabulary far larger than an
// operational email uses. What is actually needed is paragraphs, a heading, a
// link and a button.
//
// So: everything is escaped, and the only tags that appear are ones this file
// emits. That makes the output safe by construction rather than by filtering,
// and it is why an operator cannot break the layout no matter what they type.
//
//	# Heading            a line starting with "# "
//	blank line           separates paragraphs
//	[Label](url)         alone on a line: a button. Inline: a link.
//
// Placeholders are left ALONE here. Rendering happens first and substitution
// second, so a value that happens to contain "[x](y)" or "# " arrives as those
// characters instead of becoming markup. See Render.

// renderHTML turns the markup into the body of an email, without the layout.
func renderHTML(src string) string {
	var b strings.Builder
	for _, block := range blocks(src) {
		switch {
		case strings.HasPrefix(block, "# "):
			b.WriteString(`<h1 style="margin:0 0 16px;font-size:20px;line-height:1.3;font-weight:600;color:#1a1a1a;">`)
			b.WriteString(inlineHTML(strings.TrimSpace(block[2:])))
			b.WriteString("</h1>\n")
		case isLoneLink(block):
			label, url := splitLink(block)
			b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 16px;"><tr><td style="border-radius:6px;background:#7048C8;"><a href="`)
			b.WriteString(html.EscapeString(url))
			b.WriteString(`" style="display:inline-block;padding:12px 22px;font-family:inherit;font-size:15px;font-weight:600;color:#ffffff;text-decoration:none;">`)
			b.WriteString(html.EscapeString(label))
			b.WriteString("</a></td></tr></table>\n")
		default:
			b.WriteString(`<p style="margin:0 0 16px;font-size:15px;line-height:1.6;color:#333333;">`)
			b.WriteString(inlineHTML(block))
			b.WriteString("</p>\n")
		}
	}
	return b.String()
}

// renderText turns the markup into the plain-text part.
//
// A button becomes "Label: url" rather than disappearing: the link IS the mail
// in a verification or a reset, so losing it in the text part would make that
// part useless exactly when somebody is reading it because the HTML failed.
func renderText(src string) string {
	var out []string
	for _, block := range blocks(src) {
		switch {
		case strings.HasPrefix(block, "# "):
			out = append(out, strings.TrimSpace(block[2:]))
		case isLoneLink(block):
			label, url := splitLink(block)
			out = append(out, label+": "+url)
		default:
			out = append(out, inlineText(block))
		}
	}
	return strings.Join(out, "\n\n")
}

// blocks splits on blank lines and drops empties. Newlines inside a block are
// normalised to spaces, because a hard-wrapped paragraph that keeps its breaks
// renders ragged on a phone.
func blocks(src string) []string {
	var out []string
	for _, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n\n") {
		b := strings.TrimSpace(strings.Join(strings.Fields(raw), " "))
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

func isLoneLink(block string) bool {
	if !strings.HasPrefix(block, "[") || !strings.HasSuffix(block, ")") {
		return false
	}
	label, url := splitLink(block)
	return label != "" && url != "" && "["+label+"]("+url+")" == block
}

// splitLink pulls "[label](url)" apart. Returns empties when it is not one.
func splitLink(s string) (label, url string) {
	i := strings.Index(s, "](")
	if !strings.HasPrefix(s, "[") || i < 0 || !strings.HasSuffix(s, ")") {
		return "", ""
	}
	return s[1:i], s[i+2 : len(s)-1]
}

// inlineHTML escapes a block and turns any [label](url) inside it into a link.
func inlineHTML(block string) string {
	return rewriteLinks(block, func(label, url string) string {
		return `<a href="` + html.EscapeString(url) + `" style="color:#7048C8;">` + html.EscapeString(label) + `</a>`
	}, html.EscapeString)
}

// inlineText flattens any [label](url) to "label (url)".
func inlineText(block string) string {
	return rewriteLinks(block, func(label, url string) string {
		return label + " (" + url + ")"
	}, func(s string) string { return s })
}

// rewriteLinks walks a block once, handing link spans to link() and everything
// else to plain(). One pass rather than a regex so the escaping and the
// rewriting cannot disagree about what is a link.
func rewriteLinks(block string, link func(label, url string) string, plain func(string) string) string {
	var b strings.Builder
	for {
		open := strings.Index(block, "[")
		if open < 0 {
			b.WriteString(plain(block))
			return b.String()
		}
		mid := strings.Index(block[open:], "](")
		if mid < 0 {
			b.WriteString(plain(block))
			return b.String()
		}
		shut := strings.Index(block[open+mid:], ")")
		if shut < 0 {
			b.WriteString(plain(block))
			return b.String()
		}
		end := open + mid + shut + 1
		label, url := splitLink(block[open:end])
		if label == "" || url == "" {
			// Not a link after all; keep the bracket as text and move past it.
			b.WriteString(plain(block[:open+1]))
			block = block[open+1:]
			continue
		}
		b.WriteString(plain(block[:open]))
		b.WriteString(link(label, url))
		block = block[end:]
	}
}

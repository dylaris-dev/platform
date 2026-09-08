package mailer

import (
	"errors"
	"html"
	"sort"
	"strings"
)

// Editable outgoing mail.
//
// A Definition is what the platform CAN send: its key, which variables exist,
// and the wording it ships with. A Template is an operator's override of the
// subject and body. No row means the built-in default, which is why an install
// that never opens this screen keeps sending exactly what it sent before, and
// why "reset to default" is a DELETE rather than a copy of the original text
// back into the row - a copy would freeze today's wording and never pick up an
// improvement.
//
// Variables are declared PER DEFINITION rather than globally. Offering
// {{reset_link}} while editing a billing mail would be a trap: it renders as
// nothing, in a mail nobody tests, discovered by a customer.

// Variable is one placeholder an operator may use in a template.
type Variable struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
}

// Definition is a mail the platform can send.
type Definition struct {
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Purpose     string     `json:"purpose"`
	Variables   []Variable `json:"variables"`
	Subject     string     `json:"subject"`
	Body        string     `json:"body"`
}

// Template is an operator's override. Empty fields fall back to the definition,
// so clearing the subject restores the default subject without clearing the
// body with it.
type Template struct {
	Key     string `json:"key"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Rendered is one mail, ready to send.
type Rendered struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
}

// Brand is what the layout needs. Passed in rather than read here so this
// package keeps taking a SettingsReader only where it already does.
type Brand struct {
	SiteName string
	PanelURL string
}

const (
	KeyVerifyEmail   = "auth.verify_email"
	KeyPasswordReset = "auth.password_reset"
)

var definitions = []Definition{
	{
		Key:         KeyVerifyEmail,
		Name:        "Confirm your email address",
		Description: "Sent when somebody registers. Until it is opened the account cannot be used.",
		Purpose:     "auth",
		Variables: []Variable{
			{Name: "username", Description: "The name the account was registered with", Example: "alex"},
			{Name: "verify_link", Description: "Single-use confirmation link", Example: "https://panel.example.com/verify-email?token=..."},
			{Name: "site_name", Description: "The platform's name", Example: "DYLARIS"},
		},
		Subject: "Confirm your {{site_name}} account",
		Body: `Hi {{username}},

Welcome to {{site_name}}. Please confirm your email address:

[Confirm my email]({{verify_link}})

This link is single-use and was issued just now. If you did not register, you can safely ignore this message.`,
	},
	{
		Key:         KeyPasswordReset,
		Name:        "Reset your password",
		Description: "Sent when somebody asks for a password reset. Also sent when the address is not registered is NOT done - no mail goes out in that case.",
		Purpose:     "auth",
		Variables: []Variable{
			{Name: "username", Description: "The account's name", Example: "alex"},
			{Name: "reset_link", Description: "Single-use reset link", Example: "https://panel.example.com/reset-password?token=..."},
			{Name: "ttl_minutes", Description: "How long the link stays valid, in minutes", Example: "30"},
			{Name: "site_name", Description: "The platform's name", Example: "DYLARIS"},
		},
		Subject: "Reset your {{site_name}} password",
		Body: `Hi {{username}},

We received a request to reset your {{site_name}} password. Choose a new one here:

[Choose a new password]({{reset_link}})

This link is valid for {{ttl_minutes}} minutes and works exactly once. If you did not ask for a reset, you can ignore this email - your password stays as it is.`,
	},
}

// Definitions returns every mail the platform can send, by key.
func Definitions() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// DefinitionByKey looks one up.
func DefinitionByKey(key string) (Definition, bool) {
	for _, d := range definitions {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}

// ErrUnknownVariable names a placeholder the definition does not declare, so an
// operator finds a typo while editing instead of shipping an empty gap.
var ErrUnknownVariable = errors.New("unknown variable")

// Validate reports placeholders the definition does not declare.
//
// Refusing rather than silently blanking, because a blank is invisible: a
// {{reset_lnik}} would send a mail with a missing link and nothing anywhere
// would say why.
func (d Definition) Validate(t Template) error {
	known := map[string]bool{}
	for _, v := range d.Variables {
		known[v.Name] = true
	}
	var bad []string
	for _, name := range placeholders(t.Subject + "\n" + t.Body) {
		if !known[name] {
			bad = append(bad, "{{"+name+"}}")
		}
	}
	if len(bad) > 0 {
		return errors.New(ErrUnknownVariable.Error() + ": " + strings.Join(bad, ", "))
	}
	return nil
}

// placeholders lists every {{name}} in s, in order, with duplicates.
func placeholders(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return out
		}
		name := strings.TrimSpace(s[i+2 : i+j])
		if name != "" {
			out = append(out, name)
		}
		s = s[i+j+2:]
	}
}

// Render produces the mail to send.
//
// Order matters and is the whole reason this is not a string replace: the
// MARKUP is rendered first, with the placeholders still in it, and the VALUES
// are substituted afterwards. A username of "[Click here](http://evil)" is
// therefore printed as those characters rather than becoming a button, and the
// same value is HTML-escaped on the way into the HTML part and left alone on
// the way into the text part.
func Render(def Definition, override *Template, brand Brand, vars map[string]string) Rendered {
	subject, body := def.Subject, def.Body
	if override != nil {
		if s := strings.TrimSpace(override.Subject); s != "" {
			subject = s
		}
		if b := strings.TrimSpace(override.Body); b != "" {
			body = b
		}
	}

	all := map[string]string{"site_name": brand.SiteName}
	for k, v := range vars {
		all[k] = v
	}

	return Rendered{
		Subject: substitute(subject, all, nil),
		Text:    substitute(renderText(body), all, nil),
		HTML:    layout(substitute(renderHTML(body), all, html.EscapeString), brand),
	}
}

// substitute replaces {{name}} with its value, passing each value through
// esc first when one is given. An undeclared name becomes empty rather than
// staying as "{{name}}": a visible placeholder in a customer's inbox reads as
// a broken system, and Validate is where a typo is meant to be caught.
func substitute(s string, vars map[string]string, esc func(string) string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		v := vars[strings.TrimSpace(s[i+2:i+j])]
		if esc != nil {
			v = esc(v)
		}
		b.WriteString(v)
		s = s[i+j+2:]
	}
}

// layout wraps a rendered body in the shell every mail shares.
//
// Tables and inline styles on purpose: that is what mail clients render, and a
// <style> block is stripped by several of them. A max width keeps a line
// readable on a desktop client that would otherwise run it edge to edge.
func layout(body string, brand Brand) string {
	name := html.EscapeString(brand.SiteName)
	var footer string
	if brand.PanelURL != "" {
		footer = `<a href="` + html.EscapeString(brand.PanelURL) + `" style="color:#7048C8;text-decoration:none;">` + name + `</a>`
	} else {
		footer = name
	}
	return `<!doctype html><html><body style="margin:0;padding:0;background:#f4f4f6;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f4f6;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border-radius:10px;padding:32px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
<tr><td>
<div style="font-size:15px;font-weight:700;letter-spacing:0.04em;color:#7048C8;margin:0 0 24px;">` + name + `</div>
` + body + `<div style="margin:24px 0 0;padding:16px 0 0;border-top:1px solid #ececf0;font-size:12px;line-height:1.5;color:#8b8d98;">
Sent by ` + footer + `. If this was not you, no action is needed.
</div>
</td></tr></table>
</td></tr></table>
</body></html>`
}

package mailer

import (
	"strings"
	"testing"
)

func brand() Brand { return Brand{SiteName: "DYLARIS", PanelURL: "https://panel.example.com"} }

func verifyDef(t *testing.T) Definition {
	t.Helper()
	d, ok := DefinitionByKey(KeyVerifyEmail)
	if !ok {
		t.Fatal("the verification definition is missing")
	}
	return d
}

// The link IS the mail. If the text part loses it, the reader who fell back to
// text - images off, a text-only client, a screen reader - has nothing to click
// and no way to finish registering.
func TestRenderKeepsTheLinkInBothParts(t *testing.T) {
	r := Render(verifyDef(t), nil, brand(), map[string]string{
		"username":    "alex",
		"verify_link": "https://panel.example.com/verify-email?token=abc",
	})
	if !strings.Contains(r.HTML, "https://panel.example.com/verify-email?token=abc") {
		t.Error("the HTML part has no link")
	}
	if !strings.Contains(r.Text, "https://panel.example.com/verify-email?token=abc") {
		t.Errorf("the text part has no link:\n%s", r.Text)
	}
	if !strings.Contains(r.Subject, "DYLARIS") {
		t.Errorf("site_name did not reach the subject: %q", r.Subject)
	}
	if strings.Contains(r.Subject+r.Text+r.HTML, "{{") {
		t.Error("an unsubstituted placeholder survived into the message")
	}
}

// A value must never become markup. Markup is rendered FIRST and values are
// substituted after, so a username that looks like a button is printed as
// characters. Getting this backwards would let a display name inject a link
// into a mail somebody else receives.
func TestRenderNeverLetsAValueBecomeMarkup(t *testing.T) {
	r := Render(verifyDef(t), nil, brand(), map[string]string{
		"username":    "[Click here](https://evil.example.com)",
		"verify_link": "https://panel.example.com/verify-email?token=abc",
	})
	if strings.Contains(r.HTML, "evil.example.com\"") || strings.Contains(r.HTML, `href="https://evil.example.com"`) {
		t.Errorf("a value became a link:\n%s", r.HTML)
	}
	if !strings.Contains(r.HTML, "[Click here]") {
		t.Errorf("the value should appear as literal characters:\n%s", r.HTML)
	}
}

func TestRenderEscapesValuesInHTMLAndLeavesThemAloneInText(t *testing.T) {
	r := Render(verifyDef(t), nil, brand(), map[string]string{
		"username":    `Alex <script>alert(1)</script>`,
		"verify_link": "https://panel.example.com/x",
	})
	if strings.Contains(r.HTML, "<script>") {
		t.Errorf("a value reached the HTML unescaped:\n%s", r.HTML)
	}
	if !strings.Contains(r.HTML, "&lt;script&gt;") {
		t.Errorf("expected the value escaped:\n%s", r.HTML)
	}
	if !strings.Contains(r.Text, "<script>") {
		t.Error("the text part should carry the value as typed; escaping there is just noise")
	}
}

// Clearing the subject must restore the default subject WITHOUT taking the
// edited body with it, or an operator loses their work by emptying one field.
func TestRenderFallsBackPerField(t *testing.T) {
	def := verifyDef(t)
	r := Render(def, &Template{Key: def.Key, Subject: "", Body: "# Custom\n\nOnly the body changed."}, brand(), nil)
	if !strings.Contains(r.Subject, "DYLARIS") {
		t.Errorf("an empty subject should fall back to the default, got %q", r.Subject)
	}
	if !strings.Contains(r.Text, "Only the body changed.") {
		t.Errorf("the overridden body was dropped:\n%s", r.Text)
	}
	if strings.Contains(r.Text, "Welcome to") {
		t.Error("the default body is still being used even though one was set")
	}
}

func TestValidateRefusesAVariableTheDefinitionDoesNotHave(t *testing.T) {
	def := verifyDef(t)
	if err := def.Validate(Template{Subject: "Hi", Body: "Use {{reset_lnik}} now"}); err == nil {
		t.Fatal("a typo in a variable name was accepted; it would send a mail with a silent gap")
	}
	if err := def.Validate(Template{Subject: "Hi {{site_name}}", Body: "Go to {{verify_link}}, {{username}}"}); err != nil {
		t.Errorf("declared variables were refused: %v", err)
	}
}

// Every shipped default must render with the variables it declares, or the
// first send after an upgrade is the one that finds the mistake.
func TestEveryDefinitionRendersFromItsOwnDeclaredVariables(t *testing.T) {
	for _, d := range Definitions() {
		t.Run(d.Key, func(t *testing.T) {
			if err := d.Validate(Template{Subject: d.Subject, Body: d.Body}); err != nil {
				t.Fatalf("the shipped default uses a variable it does not declare: %v", err)
			}
			vars := map[string]string{}
			for _, v := range d.Variables {
				vars[v.Name] = v.Example
			}
			r := Render(d, nil, brand(), vars)
			if strings.TrimSpace(r.Subject) == "" {
				t.Error("empty subject")
			}
			if strings.TrimSpace(r.Text) == "" {
				t.Error("empty text part")
			}
			if !strings.Contains(r.HTML, "<html>") {
				t.Error("the HTML part is not a whole document, so some clients will not render it")
			}
			if strings.Contains(r.Subject+r.Text+r.HTML, "{{") {
				t.Error("a placeholder survived a render that supplied every declared variable")
			}
		})
	}
}

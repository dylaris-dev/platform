package mailer

import (
	"strings"
	"testing"
)

func TestBuildRawKeepsAPlainMessagePlain(t *testing.T) {
	raw, err := buildRaw("DYLARIS <no-reply@example.com>", Message{
		To: "u@example.com", Subject: "Confirm your account", Body: "Hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `Content-Type: text/plain; charset="utf-8"`) {
		t.Errorf("a message with no HTML should stay text/plain:\n%s", s)
	}
	if strings.Contains(s, "multipart") {
		t.Error("a message with no HTML must not become multipart")
	}
	if !strings.HasSuffix(s, "Hello") {
		t.Errorf("body missing:\n%s", s)
	}
}

// The order is the contract: a client renders the LAST part it understands, so
// text must come first or every HTML-capable reader is shown the plain text.
func TestBuildRawPutsTextBeforeHTML(t *testing.T) {
	raw, err := buildRaw("DYLARIS <no-reply@example.com>", Message{
		To: "u@example.com", Subject: "Hi", Body: "plain version", HTML: "<p>rich version</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "multipart/alternative; boundary=") {
		t.Fatalf("expected multipart/alternative:\n%s", s)
	}
	text := strings.Index(s, "plain version")
	html := strings.Index(s, "rich version")
	if text < 0 || html < 0 {
		t.Fatalf("both parts must be present:\n%s", s)
	}
	if text > html {
		t.Error("the HTML part comes first, so every rich client shows the plain text")
	}

	// The closing delimiter is what tells the server the body ended. Without it
	// some servers treat the last part as truncated and drop it.
	i := strings.Index(s, "boundary=\"")
	b := s[i+len("boundary=\"") : i+len("boundary=\"")+strings.Index(s[i+len("boundary=\""):], "\"")]
	if !strings.Contains(s, "--"+b+"--\r\n") {
		t.Error("the closing boundary is missing")
	}
}

func TestBuildRawEncodesOnlyANonASCIISubject(t *testing.T) {
	// A subject can carry {{username}} now, so this is reachable rather than
	// theoretical: a raw non-ASCII header byte is undefined and arrives as
	// mojibake or gets the message refused.
	raw, err := buildRaw("f <f@example.com>", Message{
		To: "u@example.com", Subject: "Willkommen, Jörg", Body: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Subject: Willkommen, Jörg") {
		t.Error("a non-ASCII subject went out raw")
	}
	if !strings.Contains(string(raw), "Subject: =?utf-8?") {
		t.Errorf("expected an RFC 2047 encoded subject:\n%s", string(raw))
	}

	plain, err := buildRaw("f <f@example.com>", Message{
		To: "u@example.com", Subject: "Reset your password", Body: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), "Subject: Reset your password\r\n") {
		t.Error("an ASCII subject should travel untouched")
	}
}

// A bare LF inside a part is what makes some servers reject the whole message.
func TestBuildRawNormalisesLineEndingsInParts(t *testing.T) {
	raw, err := buildRaw("f <f@example.com>", Message{
		To: "u@example.com", Subject: "Hi", Body: "one\ntwo", HTML: "<p>one</p>\n<p>two</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' && (i == 0 || s[i-1] != '\r') {
			t.Fatalf("bare LF at byte %d:\n%q", i, s)
		}
	}
}

// A boundary that occurs inside a part ends it early. Random rather than
// constant is what makes that unreachable for operator-authored bodies.
func TestBuildRawUsesAFreshBoundaryEachTime(t *testing.T) {
	m := Message{To: "u@example.com", Subject: "Hi", Body: "a", HTML: "<p>a</p>"}
	first, err := buildRaw("f <f@example.com>", m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRaw("f <f@example.com>", m)
	if err != nil {
		t.Fatal(err)
	}
	if boundaryOf(t, string(first)) == boundaryOf(t, string(second)) {
		t.Error("the boundary is constant, so a body containing it would truncate the mail")
	}
}

func boundaryOf(t *testing.T, s string) string {
	t.Helper()
	const marker = "boundary=\""
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("no boundary in:\n%s", s)
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, "\"")
	if j < 0 {
		t.Fatalf("unterminated boundary in:\n%s", s)
	}
	return rest[:j]
}

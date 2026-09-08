package mailer

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// MIME helpers for the multipart/alternative body.
//
// Hand-built rather than via mime/multipart because the SMTP path already
// assembles its own headers and hands smtp.SendMail one []byte. Introducing a
// writer here would mean two things building the same message.

// isASCII reports whether s can travel in a raw header untouched.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// mimeBoundary returns a delimiter that cannot occur in the parts.
//
// Random rather than a constant: a boundary that appears inside a body ends
// that part early, and a body is operator-authored text now. 128 bits of
// randomness makes that unreachable rather than unlikely.
func mimeBoundary() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dylaris-" + hex.EncodeToString(b), nil
}

// writePart appends one part of a multipart/alternative body.
//
// quoted-printable is deliberately NOT used: 8bit with a UTF-8 charset is what
// every mail server this platform can reach accepts, and encoding by hand is a
// second place to get line lengths wrong. Bare newlines are normalised to CRLF
// because a bare LF inside a part is what makes some servers reject the whole
// message.
func writePart(b *strings.Builder, boundary, contentType, body string) {
	b.WriteString("--")
	b.WriteString(boundary)
	b.WriteString("\r\n")
	b.WriteString("Content-Type: ")
	b.WriteString(contentType)
	b.WriteString("; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(normaliseCRLF(body))
	b.WriteString("\r\n")
}

// normaliseCRLF turns every line ending into CRLF without doubling the ones
// that already are.
func normaliseCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

package services

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestTemplateRelPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"config.yml", "config.yml", true},
		{"plugins/x/config.yml", "plugins/x/config.yml", true},
		{"./a//b", "a/b", true},
		{"../x", "", false},
		{"/etc/x", "", false},
		{"a/../../x", "", false},
		{"a/../x", "", false},
		{"C:/x", "", false},
		{"c:x", "", false},
		{"a\\b", "", false},
		{"", "", false},
		{".", "", false},
		{".upload.zip", "", false},
		{"a/.upload.zip.part", "", false},
		{"a\x00b", "", false},
		{strings.Repeat("d/", 47) + "f", strings.Repeat("d/", 47) + "f", true},
		{strings.Repeat("d/", 48) + "f", "", false},
	}
	for _, c := range cases {
		got, ok := TemplateRelPath(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("TemplateRelPath(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTemplateFileAllowed(t *testing.T) {
	cases := []struct {
		rel, policy string
		ok          bool
	}{
		{"plugins/Plugin.JAR", TemplatePolicyNoJars, false},
		{"plugins/Plugin.JAR", TemplatePolicyUploads, true},
		{"server.properties", TemplatePolicyNoJars, true},
		{"server.properties", TemplatePolicyUploads, true},
	}
	for _, c := range cases {
		ok, reason := TemplateFileAllowed(c.rel, c.policy)
		if ok != c.ok || (!ok && reason != "jar files are not allowed in templates on this platform") {
			t.Errorf("TemplateFileAllowed(%q, %q) = %v, %q", c.rel, c.policy, ok, reason)
		}
	}
}

func makeZip(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTemplateZipAllowed(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		ok    bool
	}{
		{"jar", []string{"cfg.yml", "a.JaR"}, false},
		{"nested zip", []string{"inner.zip"}, false},
		{"nested zip upper", []string{"d/INNER.ZIP"}, false},
		{"classes", []string{"META-INF/MANIFEST.MF", "com/x/Main.class"}, false},
		{"clean", []string{"cfg.yml", "d/x.txt"}, true},
	}
	for _, c := range cases {
		b := makeZip(t, c.names...)
		ok, reason := TemplateZipAllowed(bytes.NewReader(b), int64(len(b)), TemplatePolicyNoJars)
		if ok != c.ok || (!ok && reason != templateZipReason) {
			t.Errorf("%s: no-jars = %v, %q", c.name, ok, reason)
		}
		if ok, _ := TemplateZipAllowed(bytes.NewReader(b), int64(len(b)), TemplatePolicyUploads); !ok {
			t.Errorf("%s: uploads policy must allow", c.name)
		}
	}
	// A corrupt archive cannot be inspected, so it is refused under no-jars.
	if ok, _ := TemplateZipAllowed(bytes.NewReader([]byte("nope")), 4, TemplatePolicyNoJars); ok {
		t.Error("corrupt zip must be refused under no-jars")
	}
}

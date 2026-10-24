package tabs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every loader that produces a *proxyTab has to fill the two columns the
// sub-server pin is decided from.
//
// allowHostRequest refuses a tab whose sub_server_name names something other
// than the server's active_sub_server. getTabByHostLabel - the loader behind
// the ENTIRE content data plane - selected neither column, so both fields were
// the empty string on every request and the guard was permanently false: a tab
// pinned to one world proxied whatever the other world had on that port.
// Nothing failed, because the only tests for the gate built the struct by hand.
//
// The gate lives in one place; the columns feeding it do not. So this asserts
// the property at the loaders instead of at the gate - whoever adds a third one
// gets told, rather than shipping a third silently-false predicate.
func TestEveryProxyTabLoaderSelectsTheSubServerColumns(t *testing.T) {
	loader := regexp.MustCompile(`(?s)func \(h \*ProxyHandler\) (\w+)\([^)]*\) \(\*proxyTab, error\) \{(.*?)\n\}`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range loader.FindAllStringSubmatch(string(src), -1) {
			name, fnBody := m[1], m[2]
			found++
			for _, col := range []string{"sub_server_name", "active_sub_server"} {
				if !strings.Contains(fnBody, col) {
					t.Errorf("%s in %s does not select %s, so proxyTab.SubServerName/ActiveSubServer stay empty and the sub-server pin in allowHostRequest never fires", name, f, col)
				}
			}
			for _, field := range []string{"&t.SubServerName", "&t.ActiveSubServer"} {
				if !strings.Contains(fnBody, field) {
					t.Errorf("%s in %s never scans into %s", name, f, field)
				}
			}
		}
	}
	// The extraction is the part that rots silently: a signature change would
	// otherwise turn this into a test that asserts nothing and stays green.
	if found < 2 {
		t.Fatalf("found %d proxyTab loaders, expected at least 2 - the regex stopped matching, the loaders did not disappear", found)
	}
}

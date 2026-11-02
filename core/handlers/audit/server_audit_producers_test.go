package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// noProducerByDesign lists audit event types that are declared but deliberately
// never written, with the reason. Everything else in the constant block must
// have a producer - a declared event nobody emits reads as "we audit that" and
// silently does not.
var noProducerByDesign = map[string]string{
	// server_audit_events.server_id is ON DELETE CASCADE: a row written while
	// the server is being deleted is removed by the same statement.
	"ServerAuditEventDeleted": "audit rows cascade away with the server",
}

// Five of the thirteen declared event types had no producer anywhere: setup,
// reinstall, subserver_deleted, subserver_switched and deleted. The audit log
// therefore recorded power actions and member changes but stayed silent on
// every action that reinstalls a server or destroys a world - the ones an
// owner most needs to see. The constants, the vocabulary comment and the
// LogServer helper (then LogServerAudit) all existed; only the calls were missing.
func TestEveryDeclaredAuditEventHasAProducer(t *testing.T) {
	root := filepath.Join("..", "..") // all of core

	declared := regexp.MustCompile(`(?m)^\s*(ServerAuditEvent\w+)\s+=`)
	src, err := os.ReadFile("server_audit.go")
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var names []string
	for _, m := range declared.FindAllStringSubmatch(string(src), -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatal("no ServerAuditEvent constants found - did the catalog move?")
	}

	// Collect every constant referenced outside its own declaration.
	used := map[string]bool{}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, line := range strings.Split(string(body), "\n") {
			if declared.MatchString(line) {
				continue // the declaration itself is not a use
			}
			for _, n := range names {
				if strings.Contains(line, n) {
					used[n] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, n := range names {
		if used[n] {
			if reason, exempt := noProducerByDesign[n]; exempt {
				t.Errorf("%s is listed as having no producer by design (%s) but something emits it now - drop the exemption", n, reason)
			}
			continue
		}
		if _, exempt := noProducerByDesign[n]; !exempt {
			t.Errorf("%s is declared but never emitted: the audit log claims to cover it and does not", n)
		}
	}
}

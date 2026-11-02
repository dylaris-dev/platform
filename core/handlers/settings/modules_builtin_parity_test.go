package settings

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The delete guard exists TWICE and both copies have to agree.
//
// Core rejects the request; the panel hides the button. Only one of those is
// security, but a disagreement is a defect either way: a name in Core and not
// in the panel offers an admin a button that always fails, and a name in the
// panel and not in Core hides a delete that would actually go through.
//
// Reading the panel's source is the only way to check it from here - there is
// no shared artefact - and it is worth it because the lists are edited months
// apart and nothing else compares them.
func TestBuiltInModulesMatchThePanel(t *testing.T) {
	b, err := os.ReadFile("../../panel/src/components/settings/ModulesTab.tsx")
	if err != nil {
		t.Skipf("panel source not available here: %v", err)
	}
	m := regexp.MustCompile(`BUILTIN_MODULES\s*=\s*new Set\(\[(.*?)\]\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("BUILTIN_MODULES is gone or reshaped; move this assertion with it")
	}
	var panel []string
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(string(m[1]), -1) {
		panel = append(panel, q[1])
	}

	var core []string
	for name := range builtInModules {
		core = append(core, name)
	}

	// Modpacks is deliberately panel-only: its row is DERIVED from the feature
	// flags (handlers/modpack_module.go), so Core owns its whole lifecycle and
	// the panel only has to keep the button away from it.
	panelOnlyByDesign := map[string]bool{"Modpacks": true}

	sort.Strings(panel)
	sort.Strings(core)
	inCore := map[string]bool{}
	for _, n := range core {
		inCore[n] = true
	}
	inPanel := map[string]bool{}
	for _, n := range panel {
		inPanel[n] = true
	}

	for _, n := range core {
		if !inPanel[n] {
			t.Errorf("%q is protected in Core but not in the panel: the admin gets a Delete button that always fails", n)
		}
	}
	for _, n := range panel {
		if !inCore[n] && !panelOnlyByDesign[n] {
			t.Errorf("%q is hidden in the panel but NOT protected in Core: a direct DELETE would succeed", n)
		}
	}
	t.Logf("core=%s panel=%s", strings.Join(core, ","), strings.Join(panel, ","))
}

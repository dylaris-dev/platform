package servers

import (
	"testing"

	"dylaris-core/models"
)

func TestNode_IsExternal(t *testing.T) {
	if !(&models.Node{Tags: "eu,external"}).IsExternal() {
		t.Fatal("expected external for tags 'eu,external'")
	}
	if (&models.Node{Tags: "eu,fast"}).IsExternal() {
		t.Fatal("did not expect external for 'eu,fast'")
	}
	if (&models.Node{Tags: ""}).IsExternal() {
		t.Fatal("empty tags must not be external")
	}
}

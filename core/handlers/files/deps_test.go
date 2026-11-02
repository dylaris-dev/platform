package files

import (
	"net/http/httptest"
	"testing"
)

// The two guards that used to open with `h.state == nil ||` now ask only for
// the store; a zero Deps (no store) must take the same branch the nil state
// took: no suspension refusal and no server lookup. With a store, the guards
// run (TestEveryFileWriteRefusesASuspendedTenant, TestFilesAndBeamAskTheResolverForAnAdmin).
func TestAFileHandlerWithoutAStoreSkipsTheServerGuards(t *testing.T) {
	h := NewFileHandler(Deps{})
	rw := httptest.NewRecorder()
	if h.refuseSuspendedWrite(rw, httptest.NewRequest("POST", "/", nil), "srv-1") || rw.Code != 200 || rw.Body.Len() != 0 {
		t.Fatalf("refused without a store: %d %q", rw.Code, rw.Body.String())
	}
	uuid, viaDemo, err := h.resolveServerUUID(httptest.NewRequest("GET", "/?server_uuid=srv-1", nil), true, "files.read")
	if uuid != "srv-1" || viaDemo || err != nil {
		t.Fatalf("resolveServerUUID = %q, %v, %v", uuid, viaDemo, err)
	}
}

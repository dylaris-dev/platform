package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
)

// One owner, one server they own, so every capability resolves through the
// owner short-circuit and the tests are about the RECORDING, not the decision.
func writeAuditResolver(t *testing.T, rec *[]string) *Resolver {
	t.Helper()
	st := &mwFakeStore{resolverFakeStore{
		servers: map[int]*models.Server{7: {ID: 7, OwnerID: "owner-1"}},
	}}
	r := NewResolver(st)
	r.SetServerWriteAudit(func(req *http.Request, serverID int, capID string, status int) {
		*rec = append(*rec, capID)
	})
	return r
}

func writeAuditReq(method string) *http.Request {
	req := httptest.NewRequest(method, "/api/servers/7/x", nil)
	ctx := context.WithValue(req.Context(), "userID", "owner-1")
	ctx = context.WithValue(ctx, "isAdmin", false)
	return mux.SetURLVars(req.WithContext(ctx), map[string]string{"id": "7"})
}

// The whole point of moving the record to the chokepoint: a capability nobody
// remembered to log is recorded anyway, because it passes through here.
func TestServerWriteIsRecordedAtTheChokepoint(t *testing.T) {
	var got []string
	r := writeAuditResolver(t, &got)
	// files.delete had no audit producer anywhere in the tree. Measured on
	// production: a delegate deleted a file and the owner's trail stayed empty.
	wrapped := r.RequireCap("files.delete")(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped(httptest.NewRecorder(), writeAuditReq("POST"))
	if len(got) != 1 || got[0] != "files.delete" {
		t.Fatalf("recorded %v, want one files.delete row", got)
	}
}

func TestReadsAreNotRecorded(t *testing.T) {
	var got []string
	r := writeAuditResolver(t, &got)
	wrapped := r.RequireCap("files.read")(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped(httptest.NewRecorder(), writeAuditReq("GET"))
	if len(got) != 0 {
		t.Fatalf("recorded %v; browsing a directory is not an action done TO the server", got)
	}
}

// A row for something that was refused describes an event that never happened,
// which in an accountability log is worse than no row.
func TestARefusedActionIsNotRecorded(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		var got []string
		r := writeAuditResolver(t, &got)
		wrapped := r.RequireCap("files.delete")(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		wrapped(httptest.NewRecorder(), writeAuditReq("POST"))
		if len(got) != 0 {
			t.Fatalf("status %d recorded %v, want nothing", status, got)
		}
	}
}

// A handler that never touches WriteHeader still succeeded: net/http sends 200
// on the first Write, and so must the wrapper, or every handler that just
// encodes JSON would go unrecorded.
func TestAnImplicit200IsRecorded(t *testing.T) {
	var got []string
	r := writeAuditResolver(t, &got)
	wrapped := r.RequireCap("files.write")(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	rec := httptest.NewRecorder()
	wrapped(rec, writeAuditReq("POST"))
	if len(got) != 1 {
		t.Fatalf("recorded %v, want one row for an implicit 200", got)
	}
	if rec.Body.String() != `{"success":true}` {
		t.Fatalf("body = %q; the wrapper must pass writes through untouched", rec.Body.String())
	}
}

// A handler that writes its own richer row says so, and the blunt one is
// skipped - otherwise a rename would appear twice, once as "name_changed" with
// the old and new name and once as "server.settings.write".
func TestAHandlersOwnRowSuppressesTheGenericOne(t *testing.T) {
	var got []string
	r := writeAuditResolver(t, &got)
	wrapped := r.RequireCap("server.settings.write")(func(w http.ResponseWriter, req *http.Request) {
		MarkAudited(req.Context())
		w.WriteHeader(http.StatusOK)
	})
	wrapped(httptest.NewRecorder(), writeAuditReq("PATCH"))
	if len(got) != 0 {
		t.Fatalf("recorded %v; the handler already wrote its own row", got)
	}
}

// MarkAudited must be harmless on a request that never went through the
// middleware - unit tests call handlers directly all over this tree.
func TestMarkAuditedWithoutASlotDoesNotPanic(t *testing.T) {
	MarkAudited(context.Background())
	if WasAudited(context.Background()) {
		t.Fatal("WasAudited = true without a slot")
	}
}

// The handler-side variant, which is what puts the file manager back into the
// trail. The capability and the server come from the handler's own resolution,
// not from parsing the request again out here.
func TestAuditResolvedWriteRecordsWhatTheHandlerAuthorized(t *testing.T) {
	var got []string
	r := writeAuditResolver(t, &got)
	wrapped := r.AuditResolvedWrite(func(w http.ResponseWriter, req *http.Request) {
		StashServerWrite(req.Context(), 7, "files.delete")
		w.WriteHeader(http.StatusOK)
	})
	wrapped(httptest.NewRecorder(), writeAuditReq("POST"))
	if len(got) != 1 || got[0] != "files.delete" {
		t.Fatalf("recorded %v, want one files.delete row", got)
	}
}

func TestAuditResolvedWriteIgnoresReadsAndRefusals(t *testing.T) {
	cases := []struct {
		name   string
		capID  string
		status int
		stash  bool
	}{
		{"a read", "files.read", http.StatusOK, true},
		{"a refusal", "files.delete", http.StatusForbidden, true},
		// Access denied: the handler never got as far as authorizing anything,
		// so there is nothing to record and no server id to record it against.
		{"nothing authorized", "", http.StatusForbidden, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			r := writeAuditResolver(t, &got)
			wrapped := r.AuditResolvedWrite(func(w http.ResponseWriter, req *http.Request) {
				if tc.stash {
					StashServerWrite(req.Context(), 7, tc.capID)
				}
				w.WriteHeader(tc.status)
			})
			wrapped(httptest.NewRecorder(), writeAuditReq("POST"))
			if len(got) != 0 {
				t.Fatalf("recorded %v, want nothing", got)
			}
		})
	}
}

// StashServerWrite has to be harmless without the wrapper: these handlers are
// called directly by plenty of tests in this tree.
func TestStashWithoutTheWrapperDoesNotPanic(t *testing.T) {
	StashServerWrite(context.Background(), 7, "files.write")
}

// A GET is never an action, whatever the capability's verb says. Spark has one
// "use" capability covering its listing AND its recording, so the verb alone
// would file a profile LIST as something done to the server - and a websocket
// upgrade, also a GET, would be handed a wrapped ResponseWriter for nothing.
func TestAGetIsNeverRecordedEvenOnANonReadCapability(t *testing.T) {
	var got []string
	r := writeAuditResolver(t, &got)
	wrapped := r.RequireCap("spark.use")(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped(httptest.NewRecorder(), writeAuditReq("GET"))
	if len(got) != 0 {
		t.Fatalf("recorded %v for a GET, want nothing", got)
	}
	wrapped(httptest.NewRecorder(), writeAuditReq("POST"))
	if len(got) != 1 {
		t.Fatalf("recorded %v for the POST, want one row", got)
	}
}

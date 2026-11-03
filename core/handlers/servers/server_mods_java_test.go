package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// Install queues the build the Java check picked, refuses when it picked none,
// and skips the check on an image that does not say its Java.
func TestInstallHonoursTheJavaCheck(t *testing.T) {
	older := fakeVersion("P1", []string{"paper"}, []string{"1.21.11"})
	older.ID, older.VersionNum = "V0", "7.4.5"
	older.Files[1].Filename = "older.jar"
	for _, tt := range []struct {
		name      string
		image     string
		pick      *services.ModrinthVersion
		refuse    bool
		wantCode  int
		wantVer   string
		wantFile  string
		wantAsked bool
	}{
		{"fits", "ghcr.io/dylaris-dev/platform-mc-java21:latest", nil, false, http.StatusOK, "V1", "real.jar", true},
		{"older build substituted", "ghcr.io/dylaris-dev/platform-mc-java21:latest", older, false, http.StatusOK, "V0", "older.jar", true},
		{"nothing loads", "ghcr.io/dylaris-dev/platform-mc-java21:latest", nil, true, http.StatusConflict, "", "", true},
		{"custom image unchecked", "eclipse-temurin:21-jre", nil, false, http.StatusOK, "V1", "real.jar", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := &models.Server{ID: 7, UUID: "srv-uuid", OwnerID: "alice", NodeID: 1, InstallerType: "paper",
				MinecraftVersion: "1.21.11", ActiveSubServer: "survival", GameImage: tt.image}
			oldFetch, oldFit := fetchInstallVersion, javaFit
			fetchInstallVersion = func(context.Context, string) (*services.ModrinthVersion, error) {
				return fakeVersion("P1", []string{"paper"}, []string{"1.21.11"}), nil
			}
			asked := false
			javaFit = func(_ context.Context, root *services.ModrinthVersion, _, _, _ string, java int, _ string) (*services.ModrinthVersion, error) {
				asked = true
				if java != 21 {
					t.Errorf("java = %d, want 21", java)
				}
				if tt.refuse {
					return nil, &services.ModFitError{Reason: "X 1 needs Java 25; this server runs Java 21."}
				}
				if tt.pick != nil {
					return tt.pick, nil
				}
				return root, nil
			}
			t.Cleanup(func() { fetchInstallVersion, javaFit = oldFetch, oldFit })

			m := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
			fs := &modFitFakeStore{installStatusFakeStore: installStatusFakeStore{mr: m}, server: srv}
			h := &ServerModsHandler{d: testDeps(Deps{Store: fs, Redis: rdb, Queue: services.NewQueueService(rdb)})}

			body := `{"projectId":"P1","versionId":"V1","title":"WorldEdit","fileName":"spark.jar","downloadUrl":"https://cdn.modrinth.com/data/P1/versions/V1/spark.jar"}`
			req := httptest.NewRequest(http.MethodPost, "/api/servers/7/mods", strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), "userID", "alice"))
			rw := httptest.NewRecorder()
			h.Install(rw, mux.SetURLVars(req, map[string]string{"id": "7"}))

			if rw.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rw.Code, tt.wantCode, rw.Body.String())
			}
			if asked != tt.wantAsked {
				t.Errorf("java check asked = %v, want %v", asked, tt.wantAsked)
			}
			if tt.wantFile == "" {
				if fs.upserted != nil {
					t.Errorf("a refused install wrote row %+v", fs.upserted)
				}
				if !strings.Contains(rw.Body.String(), "needs Java 25") {
					t.Errorf("refusal body %s does not carry the reason", rw.Body.String())
				}
				return
			}
			if fs.upserted == nil || fs.upserted.FileName != tt.wantFile || fs.upserted.ModrinthVersionID != tt.wantVer {
				t.Fatalf("row = %+v, want %s / %s", fs.upserted, tt.wantVer, tt.wantFile)
			}
			var reply struct {
				VersionID string `json:"versionId"`
			}
			json.Unmarshal(rw.Body.Bytes(), &reply)
			if reply.VersionID != tt.wantVer {
				t.Errorf("reply versionId = %q, want %q", reply.VersionID, tt.wantVer)
			}
		})
	}
}

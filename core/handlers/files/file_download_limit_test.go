package files

import (
	"context"
	"dylaris-core/handlers/handlertest"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// fm.user_download_limit / fm.admin_download_limit have been writable from the
// settings page, readable back from it, and mapped in getTransferLimit since
// the file manager existed - and nothing ever passed "download" to that
// function. The panel offered the operator a download ceiling that did
// nothing, while the upload ceiling from the same function and the same
// settings page was enforced on both write paths.
//
// These pin the two halves: the limit lookup resolves the download keys, and
// the budget that now spends it refuses correctly at the boundary.

func downloadLimitRequest(isAdmin bool) *http.Request {
	r := httptest.NewRequest("GET", "/api/files/download?server_uuid=x&path=world", nil)
	ctx := context.WithValue(r.Context(), "isAdmin", isAdmin)
	ctx = context.WithValue(ctx, "username", "u")
	ctx = context.WithValue(ctx, "userID", "u-id")
	return r.WithContext(ctx)
}

func TestGetTransferLimit_ResolvesTheDownloadKeys(t *testing.T) {
	const (
		userDownload  = 7 * 1024 * 1024
		adminDownload = 9 * 1024 * 1024
		userUpload    = 3 * 1024 * 1024
	)
	fs := handlertest.NewSettingsStore()
	fs.KV["fm.user_download_limit"] = strconv.Itoa(userDownload)
	fs.KV["fm.admin_download_limit"] = strconv.Itoa(adminDownload)
	fs.KV["fm.user_upload_limit"] = strconv.Itoa(userUpload)
	h := &FileHandler{d: Deps{Store: fs}}

	cases := []struct {
		name      string
		isAdmin   bool
		limitType string
		want      int64
	}{
		{"user download", false, "download", userDownload},
		{"admin download", true, "download", adminDownload},
		{"user upload is unaffected", false, "upload", userUpload},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := h.getTransferLimit(downloadLimitRequest(c.isAdmin), c.limitType)
			if got == nil || *got != c.want {
				t.Fatalf("getTransferLimit(%q, admin=%v) = %v, want %d", c.limitType, c.isAdmin, got, c.want)
			}
		})
	}
}

// An unset or unparseable setting must fall back to the compiled default, not
// to zero - a zero budget would refuse every download outright.
func TestGetTransferLimit_DownloadFallsBackToTheDefault(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	fs.KV["fm.user_download_limit"] = "not-a-number"
	h := &FileHandler{d: Deps{Store: fs}}

	if got := h.getTransferLimit(downloadLimitRequest(false), "download"); got == nil || *got != 1*1024*1024*1024 {
		t.Fatalf("unparseable setting gave %v, want the 1 GiB user default", got)
	}
	if got := h.getTransferLimit(downloadLimitRequest(true), "download"); got == nil || *got != 5*1024*1024*1024 {
		t.Fatalf("unset setting gave %v, want the 5 GiB admin default", got)
	}
}

func TestDownloadBudget_Take(t *testing.T) {
	cases := []struct {
		name  string
		start int64
		takes []int
		want  []bool
		left  int64
	}{
		{"under the limit", 100, []int{40, 40}, []bool{true, true}, 20},
		{"exactly the limit is allowed", 100, []int{100}, []bool{true}, 0},
		{"one byte over is refused", 100, []int{101}, []bool{false}, 100},
		{"refusal does not consume", 100, []int{60, 60, 40}, []bool{true, false, true}, 0},
		{"a spent budget refuses everything further", 10, []int{10, 1}, []bool{true, false}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := downloadBudget{left: c.start}
			for i, n := range c.takes {
				if got := b.take(n); got != c.want[i] {
					t.Fatalf("take #%d(%d) = %v, want %v", i, n, got, c.want[i])
				}
			}
			if b.left != c.left {
				t.Fatalf("left = %d, want %d", b.left, c.left)
			}
		})
	}
}

// Before a byte of body exists there is still a status code to send, and the
// attachment headers staged from the node's metadata frame have to come off or
// the browser offers to SAVE the error text as the requested file.
func TestDownloadBudget_RefuseBeforeAnyBody(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Disposition", `attachment; filename="world.zip"`)
	rec.Header().Set("Content-Type", "application/octet-stream")

	(&downloadBudget{}).refuse(rec, false)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Fatalf("Content-Disposition survived the refusal: %q", cd)
	}
}

// Once bytes are on the wire the status code is gone, so the only honest end
// is to abort the connection - net/http understands ErrAbortHandler and the
// browser reports a failed download instead of saving a truncated file.
func TestDownloadBudget_RefuseMidStreamAborts(t *testing.T) {
	defer func() {
		r := recover()
		if r != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", r)
		}
	}()
	(&downloadBudget{}).refuse(httptest.NewRecorder(), true)
	t.Fatal("refuse returned instead of aborting a started response")
}

// The two states the old int64 could not hold, and the reason this became a
// pointer. A stored 0 used to fall through to the built-in default, so the one
// value an operator could type to mean "nobody may transfer" granted them the
// default allowance instead - and "no limit" was not expressible at all.
func TestGetTransferLimit_ZeroIsNoneAndUnlimitedIsNoCap(t *testing.T) {
	fs := handlertest.NewSettingsStore()
	fs.KV["fm.user_upload_limit"] = "0"
	fs.KV["fm.user_download_limit"] = "unlimited"
	h := &FileHandler{d: Deps{Store: fs}}

	got := h.getTransferLimit(downloadLimitRequest(false), "upload")
	if got == nil || *got != 0 {
		t.Fatalf("a stored 0 gave %v, want a cap of none", got)
	}
	if got := h.getTransferLimit(downloadLimitRequest(false), "download"); got != nil {
		t.Fatalf("a stored \"unlimited\" gave %v, want no cap", got)
	}
}

// A nil cap must not become a budget of zero, which would refuse the first byte
// of every download - the exact inversion this convention exists to prevent.
func TestNewDownloadBudget(t *testing.T) {
	b := newDownloadBudget(nil)
	if !b.take(1 << 30) {
		t.Error("an unlimited budget refused a transfer")
	}

	none := int64(0)
	b = newDownloadBudget(&none)
	if b.take(1) {
		t.Error("a cap of none allowed a byte through")
	}

	cap := int64(10)
	b = newDownloadBudget(&cap)
	if !b.take(10) || b.take(1) {
		t.Error("a real cap did not behave like one")
	}
}

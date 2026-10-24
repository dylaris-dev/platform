package beam

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseBeamSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"1.2.3", [3]int{1, 2, 3}, true},
		{"v1.2.3", [3]int{1, 2, 3}, true},
		{"1.2.3-rc1", [3]int{1, 2, 3}, true},
		{"1.2.3+build9", [3]int{1, 2, 3}, true},
		{"1.10.0", [3]int{1, 10, 0}, true},
		{"", [3]int{}, false},
		{"dev", [3]int{}, false},
		{"1.2", [3]int{}, false},
		{"1.2.3.4", [3]int{}, false},
		{"1.-2.3", [3]int{}, false},
		{"a.b.c", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseBeamSemver(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseBeamSemver(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestBeamClientBelowMin(t *testing.T) {
	cases := []struct {
		name              string
		clientVer, minVer string
		want              bool
	}{
		{"gating off empty min", "1.0.0", "", false},
		{"gating off empty min empty client", "", "", false},
		{"malformed min fails safe to allow", "1.0.0", "not-a-version", false},
		{"below blocked", "1.2.2", "1.2.3", true},
		{"below on minor", "1.1.9", "1.2.0", true},
		{"below on major", "0.9.9", "1.0.0", true},
		{"above on major", "1.0.0", "0.9.9", false},
		{"equal allowed", "1.2.3", "1.2.3", false},
		{"above allowed", "1.3.0", "1.2.3", false},
		{"above with prerelease allowed", "1.3.0-rc1", "1.2.9", false},
		{"v-prefixed client below", "v1.2.2", "1.2.3", true},
		{"absent client fail-closed", "", "1.0.0", true},
		{"dev client fail-closed", "dev", "1.0.0", true},
		{"garbage client fail-closed", "1.2", "1.0.0", true},
	}
	for _, c := range cases {
		if got := beamClientBelowMin(c.clientVer, c.minVer); got != c.want {
			t.Errorf("%s: beamClientBelowMin(%q,%q) = %v want %v", c.name, c.clientVer, c.minVer, got, c.want)
		}
	}
}

func TestSendBeamUpdateRequired(t *testing.T) {
	rec := httptest.NewRecorder()
	sendBeamUpdateRequired(rec, "1.2.3")

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var body struct {
		Success    bool   `json:"success"`
		Reason     string `json:"reason"`
		MinVersion string `json:"min_version"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body.Success {
		t.Error("success = true, want false")
	}
	if body.Reason != "update_required" {
		t.Errorf("reason = %q, want update_required", body.Reason)
	}
	if body.MinVersion != "1.2.3" {
		t.Errorf("min_version = %q, want 1.2.3", body.MinVersion)
	}
	if body.Message == "" {
		t.Error("message is empty")
	}
}

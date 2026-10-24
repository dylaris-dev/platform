package backup

import (
	"encoding/json"
	"testing"
)

// applyBackupJobPatch is not a function - the merge lives inline in UpdateJob.
// What this pins is the DECODE half, which is where the defect was: a plain
// models.BackupJob cannot tell "the caller omitted enabled" from "the caller
// sent false", so a PATCH that only changed the schedule wrote enabled=false
// and turned the backup off. Every field is a pointer for that reason, and a
// test that decodes real bodies is the thing that fails if one goes back to a
// value type.
func TestUpdateBackupJobRequestDecode(t *testing.T) {
	t.Run("an omitted field is nil, not a zero value", func(t *testing.T) {
		var req updateBackupJobRequest
		if err := json.Unmarshal([]byte(`{"schedule":"every 6h"}`), &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Schedule == nil || *req.Schedule != "every 6h" {
			t.Errorf("Schedule = %v, want every 6h", req.Schedule)
		}
		for name, got := range map[string]bool{
			"Name":            req.Name != nil,
			"SubServer":       req.SubServer != nil,
			"IncludePatterns": req.IncludePatterns != nil,
			"ExcludePatterns": req.ExcludePatterns != nil,
			"RetentionCount":  req.RetentionCount != nil,
			"StorageID":       req.StorageID != nil,
			"Enabled":         req.Enabled != nil,
		} {
			if got {
				t.Errorf("%s is set after a body that never mentioned it - it would be written over the stored value", name)
			}
		}
	})

	t.Run("enabled false is distinguishable from absent", func(t *testing.T) {
		var sent updateBackupJobRequest
		if err := json.Unmarshal([]byte(`{"enabled":false}`), &sent); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if sent.Enabled == nil {
			t.Fatal("Enabled is nil after an explicit false - the caller cannot disable a job")
		}
		if *sent.Enabled {
			t.Error("Enabled = true after an explicit false")
		}
		var absent updateBackupJobRequest
		if err := json.Unmarshal([]byte(`{"name":"x"}`), &absent); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if absent.Enabled != nil {
			t.Error("Enabled is set after a body that never mentioned it - this is exactly how a schedule change disabled the job")
		}
	})

	t.Run("an empty patterns list is a real instruction", func(t *testing.T) {
		var req updateBackupJobRequest
		if err := json.Unmarshal([]byte(`{"includePatterns":[]}`), &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.IncludePatterns == nil {
			t.Fatal("IncludePatterns is nil after an explicit [] - clearing the list would be impossible")
		}
		if len(*req.IncludePatterns) != 0 {
			t.Errorf("IncludePatterns = %v, want empty", *req.IncludePatterns)
		}
	})

	t.Run("the clear conventions decode as themselves", func(t *testing.T) {
		var req updateBackupJobRequest
		if err := json.Unmarshal([]byte(`{"subServer":"","storageId":0}`), &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// "" and 0 are how a caller clears these, because JSON null decodes to
		// the same nil an absent field does.
		if req.SubServer == nil || *req.SubServer != "" {
			t.Errorf("SubServer = %v, want a pointer to the empty string", req.SubServer)
		}
		if req.StorageID == nil || *req.StorageID != 0 {
			t.Errorf("StorageID = %v, want a pointer to 0", req.StorageID)
		}
	})
}

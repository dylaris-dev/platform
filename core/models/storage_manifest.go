package models

import "time"

// StorageManifest is the header row of one captured inventory of a blob data
// set: what was there, how big, and with which content checksums, at a point
// in time. It lives in Postgres rather than Redis because a manual migration
// can span days (export, move data out of band, reconfigure, verify), a
// manifest can hold 100k+ entries, and the Redis job record carries a 7-day
// TTL and is treated as ephemeral.
type StorageManifest struct {
	ID      int    `json:"id"`
	DataSet string `json:"dataSet"` // "ticket-attachments" | ... | "server-backups:3"
	// BackendLabel is a HUMAN description of the backend this was captured
	// from. It MUST NOT contain credentials: for s3 that means endpoint +
	// bucket + prefix, never keys.
	BackendLabel string    `json:"backendLabel"`
	Algo         string    `json:"algo"` // always "sha256" today
	CapturedAt   time.Time `json:"capturedAt"`
	ObjectCount  int64     `json:"objectCount"`
	TotalBytes   int64     `json:"totalBytes"`
	CreatedBy    string    `json:"createdBy"`
}

// StorageManifestEntry is one object inside a StorageManifest. Checksum is
// always the lower-case hex SHA-256 of the object's bytes, computed by
// streaming it - never derived from an S3 ETag, which for a multipart upload
// is md5-of-md5s and is not comparable across backends.
type StorageManifestEntry struct {
	ManifestID int    `json:"manifestId"`
	Key        string `json:"key"`
	Size       int64  `json:"size"`
	Checksum   string `json:"checksum"`
}

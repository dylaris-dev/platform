package services

// FileManagerSettings holds the four browser transfer ceilings, on the platform
// limit convention: nil is no limit, 0 is none (transfers refused), n is the cap
// in bytes.
//
// They were plain int64 and the enforcement site guarded on `n > 0`, so a stored
// 0 fell through to the built-in default - which made "nobody may upload" say
// two gigabytes, and left no way to express "no limit" at all. Both ends of the
// range were unreachable from the screen that edits them.
type FileManagerSettings struct {
	AdminUploadLimit   *int64 `json:"adminUploadLimit"`   // bytes
	AdminDownloadLimit *int64 `json:"adminDownloadLimit"` // bytes
	UserUploadLimit    *int64 `json:"userUploadLimit"`    // bytes
	UserDownloadLimit  *int64 `json:"userDownloadLimit"`  // bytes
}

// DefaultFileManagerSettings are the ceilings until an operator saves their own.
var DefaultFileManagerSettings = FileManagerSettings{
	AdminUploadLimit:   LimitPtr(2 * 1024 * 1024 * 1024), // 2 GB
	AdminDownloadLimit: LimitPtr(5 * 1024 * 1024 * 1024), // 5 GB
	UserUploadLimit:    LimitPtr(500 * 1024 * 1024),      // 500 MB
	UserDownloadLimit:  LimitPtr(1 * 1024 * 1024 * 1024), // 1 GB
}

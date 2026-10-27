package models

import "time"

type ModrinthFavorite struct {
	ProjectID string `json:"projectId"`
	Title     string `json:"title"`
}

type ModrinthWatchProject struct {
	ContentType string `json:"contentType,omitempty"`
	ProjectID   string `json:"projectId"`
	Title       string `json:"title"`
}

type ModrinthWatchVersion struct {
	Minecraft string   `json:"minecraft"`
	Available int      `json:"available"`
	Missing   []string `json:"missing"`
}

// Projects describe the last checked source. Pack alerts refresh them from the
// current recommendation; standalone project alerts keep their selected project.
type ModrinthWatch struct {
	FollowPack          bool                   `json:"followPack,omitempty"`
	CheckedBuildID      int                    `json:"checkedBuildId,omitempty"`
	CheckedBuildVersion string                 `json:"checkedBuildVersion,omitempty"`
	ID                  int64                  `json:"id"`
	UserID              string                 `json:"-"`
	BuildID             *int                   `json:"buildId,omitempty"`
	PackID              int                    `json:"packId,omitempty"`
	Title               string                 `json:"title"`
	Loader              string                 `json:"loader"`
	Current             string                 `json:"current"`
	Projects            []ModrinthWatchProject `json:"projects"`
	Unlinked            int                    `json:"unlinked"`
	Targets             []string               `json:"targets"`
	Future              bool                   `json:"future"`
	Delivered           []string               `json:"delivered"`
	Versions            []ModrinthWatchVersion `json:"versions"`
	LastError           string                 `json:"lastError,omitempty"`
	LastChecked         *time.Time             `json:"lastChecked,omitempty"`
	NextCheck           time.Time              `json:"nextCheck"`
}

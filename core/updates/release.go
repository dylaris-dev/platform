// Package updates holds the release notes this build shipped with.
//
// Two audiences, two files. `platform.md` is for people who RUN the platform
// (self-hosters, operators); `hosted.md` is for DYLARIS customers, who run a
// node or nothing at all. They are separate because "restart Core after
// changing the mail settings" and "your node needs updating by Friday" have no
// reader in common.
//
// The embedded copy is the OFFLINE FALLBACK, and nothing more. It is not this
// build's version: the repo has one version and more than one audience, so a
// release that only concerns customers leaves platform.md's top block behind
// while the images are still built from the newer release. Every component -
// Core included - reports the version STAMPED into it at build time.
package updates

import (
	_ "embed"
	"sync"

	"dylaris-pkg/release"
)

//go:embed platform.md
var platformNotes []byte

//go:embed hosted.md
var hostedNotes []byte

// Parsing happens once. The files are validated in CI, so a parse failure here
// is a build that should never have been produced - it is surfaced as an empty
// feed (the fail-open the whole update path uses) rather than as a panic that
// takes Core down over a changelog.
var (
	once     sync.Once
	platform []release.Release
	hosted   []release.Release
	parseErr error
)

func parse() {
	once.Do(func() {
		var err error
		if platform, err = release.Parse(platformNotes); err != nil {
			parseErr = err
			platform = nil
		}
		if hosted, err = release.Parse(hostedNotes); err != nil {
			parseErr = err
			hosted = nil
		}
	})
}

// Platform returns the embedded platform release notes, newest first.
func Platform() []release.Release {
	parse()
	return platform
}

// Hosted returns the embedded customer-facing release notes, newest first.
func Hosted() []release.Release {
	parse()
	return hosted
}

// Requirements returns both embedded files as one list, newest first. It is the
// ONE source of mandatory-update deadlines: the connect gate enforces it and the
// panel's Required line shows it, so a requirement written into either file is
// enforced and shown alike, and "passed" only appears when this Core refuses.
func Requirements() []release.Release {
	parse()
	return release.Merge(platform, hosted)
}

// ParseError reports a malformed embedded file, so the handler can log it once
// instead of silently serving nothing.
func ParseError() error {
	parse()
	return parseErr
}

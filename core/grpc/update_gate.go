package nodegrpc

import (
	"fmt"
	"os"
	"time"

	"dylaris-core/updates"

	pb "dylaris-proto/node"

	"dylaris-pkg/release"
)

// UpdateGate decides what to do about a node that has not applied a mandatory
// update: warn it, or refuse it.
//
// It lives here rather than in services because services imports this package;
// it is also purely an auth-path concern. The panel's Required line answers the
// same question from the same embedded set (updates.Requirements).
//
// It reads the release notes EMBEDDED in this Core build, never the remote copy.
// That is deliberate and load-bearing: this sits in the node authentication
// path, and an auth decision must not depend on GitHub being reachable. A
// mandatory deadline is also something we set and then deploy, so the Core that
// enforces it is the Core that shipped with it.
type UpdateGate struct {
	releases []release.Release
	enforce  bool
	now      func() time.Time
}

// NewUpdateGate builds the gate from the requirements of both embedded notes
// files, the same set the panel's Required line shows.
//
// RELEASE_ENFORCE_MIN_VERSION=false disables the REFUSE stage while leaving the
// warnings in place. This is a gate in the auth path: it gets an off switch that
// does not need a rebuild, because the failure mode of a wrong deadline or a
// mis-stamped image is locking paying customers out of their own hardware.
func NewUpdateGate() *UpdateGate {
	return &UpdateGate{
		releases: updates.Requirements(),
		enforce:  os.Getenv("RELEASE_ENFORCE_MIN_VERSION") != "false",
		now:      time.Now,
	}
}

// UpdateVerdict is what the gate decided. The zero value admits silently, which
// is the answer for every node that is current, unstamped, or unaffected.
type UpdateVerdict struct {
	Refuse     bool
	Warn       bool
	Message    string
	MinVersion string
	Deadline   time.Time
}

// Check answers for one component reporting one version.
//
// EVERY door out of here that is not "admit" requires all of:
//
//   - a requirement exists AND names this component
//   - the reported version parses (an unstamped node reports nothing)
//   - that version is below the release which declared the requirement
//
// and refusing additionally requires the deadline to have passed and
// enforcement to be on. An UNKNOWN version is always admitted: every image
// built before release stamping reports nothing, so ordering it would lock out
// the whole installed base the day this ships. The version is self-reported and
// can only ever make Core refuse or warn - never admit - so nothing is trusted
// that should not be.
func (g *UpdateGate) Check(service, reported string) UpdateVerdict {
	if g == nil {
		return UpdateVerdict{}
	}
	v, err := release.ParseVersion(reported)
	if err != nil {
		return UpdateVerdict{}
	}
	req, min, ok := release.Requirement(g.releases, service, v)
	if !ok {
		return UpdateVerdict{}
	}

	out := UpdateVerdict{MinVersion: min.String(), Deadline: req.Deadline}
	overdue := g.now().After(req.Deadline)

	switch {
	case overdue && g.enforce:
		out.Refuse = true
		out.Message = fmt.Sprintf(
			"this %s runs %s and the required update to %s was due %s; update the image and reconnect",
			service, v, min, req.Deadline.UTC().Format("2006-01-02 15:04 UTC"))
	case overdue:
		// Past the deadline with enforcement off. Still only a warning, but it
		// must not read like there is time left.
		out.Warn = true
		out.Message = fmt.Sprintf(
			"this %s runs %s and the required update to %s was due %s; enforcement is disabled on this Core",
			service, v, min, req.Deadline.UTC().Format("2006-01-02 15:04 UTC"))
	default:
		out.Warn = true
		out.Message = fmt.Sprintf(
			"this %s runs %s and must be updated to %s by %s or it will stop connecting",
			service, v, min, req.Deadline.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if req.Note != "" {
		out.Message += " (" + req.Note + ")"
	}
	return out
}

// applyUpdateWarning copies a warning onto a SUCCESSFUL auth result. Both
// success paths (enroll and reconnect) go through it, because a warning
// attached to only one of them would reach a node exactly once - on the connect
// where it happens to enroll - and never again.
func applyUpdateWarning(ar *pb.AuthResult, v UpdateVerdict) {
	if ar == nil || !v.Warn {
		return
	}
	ar.UpdateRequired = v.Message
	ar.UpdateRequiredVersion = v.MinVersion
	if !v.Deadline.IsZero() {
		ar.UpdateRequiredDeadline = v.Deadline.UTC().Format(time.RFC3339)
	}
}

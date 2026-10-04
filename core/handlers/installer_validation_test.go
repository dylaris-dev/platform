package handlers

import "testing"

// TestValidateInstallerRequest mirrors node/installer.go's InstallServer
// switch. Each "required" case below is a value that reached an upstream API
// as an empty string and failed there - observed live for paper, where the
// node reported "PaperMC API returned HTTP 400 for version " and the fresh
// server crash-looped to "offline" after Core had already answered 200.
func TestValidateInstallerRequest(t *testing.T) {
	cases := []struct {
		name                            string
		typ, version, loader, url, path string
		ok                              bool
	}{
		{name: "unknown type", typ: "ftp"},
		{name: "empty type", typ: ""},

		// Core ADVERTISES these three via /api/versions/software (their version
		// listing shares the PaperMC provider) but they are not an install
		// source and the node's switch has no case for them, so
		// validate.IsInstallerType excludes them and this must too.
		{name: "velocity is not an install source", typ: "velocity", version: "3.4.0"},
		{name: "waterfall is not an install source", typ: "waterfall", version: "1.20"},
		{name: "bungeecord is not an install source", typ: "bungeecord", version: "1.21"},
		// Node-side only; nothing in Core ever produces it, and the canonical
		// allowlist leaves it out.
		{name: "import is not accepted", typ: "import", url: "https://example.com/s.zip"},

		{name: "paper needs a version", typ: "paper"},
		{name: "paper with a version", typ: "paper", version: "1.21.4", ok: true},
		{name: "paper with a blank version", typ: "paper", version: "   "},
		{name: "vanilla needs a version", typ: "vanilla"},
		{name: "vanilla with a version", typ: "vanilla", version: "1.21.4", ok: true},

		// Both resolve the loader themselves when it is blank, so only the
		// version is required.
		{name: "fabric needs a version", typ: "fabric"},
		{name: "fabric without a loader is fine", typ: "fabric", version: "1.21.4", ok: true},
		{name: "forge needs a version", typ: "forge"},
		{name: "forge without a build is fine", typ: "forge", version: "1.21.4", ok: true},

		// NeoForge is passed Loader as its version; Version is unused.
		{name: "neoforge needs a loader", typ: "neoforge", version: "1.21.4"},
		{name: "neoforge with a loader", typ: "neoforge", loader: "21.4.10-beta", ok: true},

		{name: "library needs a path", typ: "library"},
		{name: "library with a path", typ: "library", path: "servers/paper.jar", ok: true},
		// A bare URL made "library" a way to have the node download anything.
		{name: "library with only a url", typ: "library", url: "https://example.com/p.jar"},

		// These carry no field this function can check; their own paths do.
		{name: "upload", typ: "upload", ok: true},
		{name: "upload-zip", typ: "upload-zip", ok: true},
		{name: "modpack", typ: "modpack", ok: true},
		{name: "pack is rewritten by SetupServer", typ: "pack", ok: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := validateInstallerRequest(c.typ, c.version, c.loader, c.url, c.path)
			if c.ok && msg != "" {
				t.Fatalf("validateInstallerRequest(%q,...) = %q, want accepted", c.typ, msg)
			}
			if !c.ok && msg == "" {
				t.Fatalf("validateInstallerRequest(%q,...) accepted it, want a rejection message", c.typ)
			}
		})
	}
}

// installer.software is forwarded to the node, which installs it over the
// upload; anything but the five server installers must stop here.
func TestValidateUploadSoftware(t *testing.T) {
	cases := []struct {
		name                         string
		typ, software, version, load string
		ok                           bool
	}{
		{name: "keep own jar", typ: "upload-zip", ok: true},
		{name: "paper over a zip", typ: "upload-zip", software: "paper", version: "1.21.11", ok: true},
		{name: "fabric over sftp files", typ: "upload", software: "fabric", version: "1.21.1", ok: true},
		{name: "neoforge needs its loader", typ: "upload-zip", software: "neoforge", load: "21.1.77", ok: true},
		{name: "neoforge without loader", typ: "upload-zip", software: "neoforge"},
		{name: "paper without version", typ: "upload-zip", software: "paper"},
		{name: "import is not a software", typ: "upload-zip", software: "import"},
		{name: "library is not a software", typ: "upload", software: "library"},
		{name: "proxy is not a software", typ: "upload", software: "velocity", version: "3.4.0"},
		// accepted by validateInstallerRequest as install sources, never as software
		{name: "modpack is not a software", typ: "upload", software: "modpack"},
		{name: "an upload is not a software", typ: "upload-zip", software: "upload-zip"},
		{name: "only for uploads", typ: "paper", software: "paper", version: "1.21.11"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := validateUploadSoftware(c.typ, c.software, c.version, c.load)
			if c.ok != (msg == "") {
				t.Fatalf("validateUploadSoftware(%q,%q) = %q, want ok=%v", c.typ, c.software, msg, c.ok)
			}
		})
	}
}

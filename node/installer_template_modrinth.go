package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"dylaris-pkg/validate"
)

// TemplateModrinthFile is one jar a template references on Modrinth instead
// of carrying it (Core: services.TemplateModrinthFile). Path is relative to
// the sub-server root, like the entries of the template's zip.
type TemplateModrinthFile struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	Sha512 string `json:"sha512"`
	Size   int64  `json:"size"`
}

// templateModrinthMax matches Core's cap on references per template.
const templateModrinthMax = 500

var sha512Hex = regexp.MustCompile(`^[0-9a-fA-F]{128}$`)

// downloadTemplateFile is the fetch step, named so a test can serve it; the
// cdn.modrinth.com check runs before it either way (see downloadModFile).
var downloadTemplateFile = downloadAndVerifyMax

// installTemplateModrinth downloads a template's referenced jars into destDir.
// Runs after the software install, so the wipe of root jars cannot touch
// them. Any file that fails fails the install: a server started without a
// plugin its configs expect is worse than one that did not install.
func installTemplateModrinth(destDir string, files []TemplateModrinthFile) error {
	if len(files) > templateModrinthMax {
		return fmt.Errorf("the template lists %d Modrinth files, more than the %d allowed", len(files), templateModrinthMax)
	}
	for _, f := range files {
		if err := installTemplateModrinthFile(destDir, f); err != nil {
			return fmt.Errorf("downloading %s from Modrinth failed: %w", truncate(f.Path, 120), err)
		}
	}
	return nil
}

func installTemplateModrinthFile(destDir string, f TemplateModrinthFile) error {
	if err := validateModrinthURL(f.URL); err != nil {
		return err
	}
	// downloadAndVerify skips the check on an empty hash; a reference always
	// has one, so a missing one is refused rather than trusted.
	if !sha512Hex.MatchString(f.Sha512) {
		return errors.New("no valid sha512 given")
	}
	rel, err := extractRel(f.Path)
	if err != nil {
		return err
	}
	dir, name := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if !validSubDir(strings.SplitN(dir, "/", 2)[0]) || !validate.IsPlainFileName(name) ||
		!strings.HasSuffix(strings.ToLower(name), ".jar") {
		return errors.New("only a .jar under mods/ or plugins/ may be downloaded")
	}
	// Same boundary as install_mod: checked before pinDir creates anything,
	// then everything happens inside the directory opened without following
	// a link.
	if _, err := resolveWithinDir(destDir, filepath.FromSlash(dir)); err != nil {
		return err
	}
	pinned, release, err := pinDir(destDir, dir, true)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer release()
	tmp := filepath.Join(pinned, name+".part")
	// Size is what Modrinth listed; a body longer than that cannot match the
	// hash, so it is cut off there instead of at the general cap.
	if err := downloadTemplateFile(f.URL, tmp, f.Sha512, f.Size); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(pinned, name)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

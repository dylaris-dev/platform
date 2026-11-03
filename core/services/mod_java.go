package services

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Modrinth does not say which Java a build needs, and "compatible with 1.21.11"
// does not imply it: WorldEdit 7.4.6 is tagged for 1.21.11 and compiled for
// Java 25, so on a Java 21 server Paper refuses it at boot while the panel said
// installed. The jar itself is the only source of truth.

// JavaFromImage reads the Java release out of a platform runtime image
// ("ghcr.io/dylaris-dev/platform-mc-java21:latest" -> 21). 0 means unknown: an
// operator-allowed custom image says nothing we can trust, so it is unchecked.
func JavaFromImage(image string) int {
	const marker = "platform-mc-java"
	i := strings.LastIndex(image, marker)
	if i < 0 {
		return 0
	}
	rest := image[i+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}

// jarClassScanCap bounds how many class headers are read per jar.
//
// ponytail: a jar is compiled with one target almost always, so a sample
// decides it; a shaded library with a newer target past the first 64 classes
// slips through. Scan everything if that ever shows up.
const jarClassScanCap = 64

// JarJavaRelease is the Java release a jar's classes need (class file major
// version - 44), the highest among the classes sampled. 0 means the archive
// holds no class files to judge (a datapack, a resource pack). Multi-release
// entries under META-INF/versions/ are skipped because the JVM only loads them
// on a runtime that new, and so is module-info.class, which Java 8 ignores and
// libraries routinely compile for 9 next to Java 8 code.
func JarJavaRelease(r io.ReaderAt, size int64) (int, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return 0, fmt.Errorf("read jar: %w", err)
	}
	maxMajor, seen := 0, 0
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasSuffix(name, ".class") || strings.HasPrefix(name, "META-INF/versions/") ||
			name == "module-info.class" || strings.HasSuffix(name, "/module-info.class") {
			continue
		}
		if seen >= jarClassScanCap {
			break
		}
		seen++
		major, err := classMajor(f)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", name, err)
		}
		maxMajor = max(maxMajor, major)
	}
	if maxMajor == 0 {
		return 0, nil
	}
	return maxMajor - 44, nil
}

// classMajor reads a class file header: 0xCAFEBABE, minor, major (big-endian).
func classMajor(f *zip.File) (int, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(rc, hdr[:]); err != nil {
		return 0, err
	}
	if binary.BigEndian.Uint32(hdr[:4]) != 0xCAFEBABE {
		return 0, fmt.Errorf("not a class file")
	}
	return int(binary.BigEndian.Uint16(hdr[6:8])), nil
}

// modrinthFileJava downloads a Modrinth file and reads the Java release it
// needs; 0 for anything that is not a jar. A variable so tests can stand in
// for the CDN.
var modrinthFileJava = func(ctx context.Context, f ModrinthFile) (int, error) {
	if !strings.HasSuffix(strings.ToLower(f.Filename), ".jar") {
		return 0, nil
	}
	tmp, err := os.CreateTemp("", "dylaris-javacheck-*.jar")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := StreamModrinthJar(ctx, f.URL, tmp, f.Hashes["sha1"], f.Hashes["sha512"])
	if err != nil {
		return 0, err
	}
	return JarJavaRelease(tmp, n)
}

// javaFallbackCap bounds the walk to older builds when the requested one needs
// a newer Java.
//
// ponytail: each step is a jar download, so five older builds is the budget;
// a project whose last five builds all need newer Java is refused with a
// message that says so, and the reader picks an older build by hand.
const javaFallbackCap = 5

// JavaFit returns the build to install on a server running Java `java`: root
// when its jar loads there, else the newest older build for the same loader and
// Minecraft version whose jar does. None fits -> *ModFitError naming both Java
// versions. installedID is the build the server has now: the walk stops there,
// so an update never turns into a downgrade or a silent reinstall.
//
// A jar that could not be downloaded or read is not judged: the install goes
// ahead the way it always did, since refusing every install while the CDN
// hiccups is the worse trade, and the node fetches the same file anyway.
func JavaFit(ctx context.Context, root *ModrinthVersion, title, loader, mc string, java int, installedID string) (*ModrinthVersion, error) {
	if java <= 0 {
		return root, nil
	}
	need, err := modrinthFileJava(ctx, root.PrimaryFile())
	if err != nil {
		log.Printf("java check: %s %s not judged: %v", root.ProjectID, root.VersionNum, err)
		return root, nil
	}
	if need <= java {
		return root, nil
	}
	if title == "" {
		title = root.PrimaryFile().Filename
	}
	head := fmt.Sprintf("%s %s needs Java %d; this server runs Java %d", title, root.VersionNum, need, java)
	refuse := &ModFitError{Reason: fmt.Sprintf("%s. Switch the server to Java %d in Setup, or install an older version.", head, need)}

	var games []string
	if !isProxyLoader(loader) && !isPluginLoader(loader) {
		games = []string{mc}
	}
	list, err := fetchProjectVersions(ctx, root.ProjectID, modLoadersFor[loader], games)
	if err != nil {
		log.Printf("java check: listing older builds of %s: %v", root.ProjectID, err)
		return nil, refuse
	}
	var older []*ModrinthVersion
	for i := range list {
		v := &list[i]
		if v.ID == root.ID || v.DatePub >= root.DatePub {
			continue
		}
		if ok, _ := ModVersionFits(loader, mc, v.Loaders, v.GameVersions); ok {
			older = append(older, v)
		}
	}
	// Newest first, and never a worse channel than the one asked for: a release
	// does not fall back to an alpha.
	sort.Slice(older, func(i, j int) bool { return older[i].DatePub > older[j].DatePub })
	tried := 0
	for _, v := range older {
		if channelRank(v.VersionType) > channelRank(root.VersionType) {
			continue
		}
		if v.ID == installedID {
			return nil, &ModFitError{Reason: fmt.Sprintf("%s, so it stays on %s. Switch the server to Java %d in Setup to update.", head, v.VersionNum, need)}
		}
		if tried >= javaFallbackCap {
			break
		}
		tried++
		n, err := modrinthFileJava(ctx, v.PrimaryFile())
		if err != nil {
			log.Printf("java check: %s %s not judged: %v", v.ProjectID, v.VersionNum, err)
			continue
		}
		if n <= java {
			return v, nil
		}
	}
	return nil, refuse
}

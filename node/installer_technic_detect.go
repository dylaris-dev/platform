package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// technicLoader is the mod loader a Technic CLIENT pack was built on, read from
// the same files Prism Launcher reads: the version.json inside bin/modpack.jar
// (Forge ships its installer or universal jar there) or bin/version.json
// (Fabric and some Forge packs).
type technicLoader struct {
	Kind    string // forge | neoforge | fabric
	MC      string // Minecraft version (inheritsFrom)
	Version string // loader version in the form the matching installer takes
}

type technicVersionJSON struct {
	InheritsFrom string `json:"inheritsFrom"`
	Libraries    []struct {
		Name string `json:"name"`
	} `json:"libraries"`
}

var errNoTechnicLoader = errors.New("could not tell which mod loader this pack uses (no Forge, NeoForge or Fabric version in bin/modpack.jar or bin/version.json)")

// technicVersionJSONMax bounds the one file we read out of a pack before it is
// trusted; a real version.json is a few KB.
const technicVersionJSONMax = 1 << 20

func detectTechnicLoader(stageDir string) (technicLoader, error) {
	var sources [][]byte
	if b, err := readVersionJSONFromJar(filepath.Join(stageDir, "bin", "modpack.jar")); err == nil {
		sources = append(sources, b)
	}
	if b, err := readFileMax(filepath.Join(stageDir, "bin", "version.json"), technicVersionJSONMax); err == nil {
		sources = append(sources, b)
	}
	for _, raw := range sources {
		var v technicVersionJSON
		if json.Unmarshal(raw, &v) != nil || v.InheritsFrom == "" {
			continue
		}
		if l, ok := loaderFromLibraries(v); ok {
			return l, nil
		}
	}
	return technicLoader{}, errNoTechnicLoader
}

func loaderFromLibraries(v technicVersionJSON) (technicLoader, bool) {
	for _, lib := range v.Libraries {
		group, rest, ok := strings.Cut(lib.Name, ":")
		if !ok {
			continue
		}
		artifact, ver, ok := strings.Cut(rest, ":")
		if !ok || ver == "" {
			continue
		}
		// A classifier after the version (group:artifact:ver:classifier) is
		// not part of the version.
		ver, _, _ = strings.Cut(ver, ":")
		switch {
		case group == "net.minecraftforge" && artifact == "forge":
			// installForge joins mc + "-" + build. Trimming only the leading mc
			// keeps 1.7.10's trailing "-1.7.10" in the build, so the joined
			// name is the real Maven coordinate.
			return technicLoader{Kind: "forge", MC: v.InheritsFrom, Version: strings.TrimPrefix(ver, v.InheritsFrom+"-")}, true
		case group == "net.neoforged" && artifact == "neoforge":
			return technicLoader{Kind: "neoforge", MC: v.InheritsFrom, Version: ver}, true
		case group == "net.fabricmc" && artifact == "fabric-loader":
			return technicLoader{Kind: "fabric", MC: v.InheritsFrom, Version: ver}, true
		}
	}
	return technicLoader{}, false
}

func readVersionJSONFromJar(jarPath string) ([]byte, error) {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "version.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return readAllMax(rc, technicVersionJSONMax)
	}
	return nil, fmt.Errorf("no version.json in %s", filepath.Base(jarPath))
}

func readFileMax(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAllMax(f, max)
}

func readAllMax(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("version.json is too large")
	}
	return b, nil
}

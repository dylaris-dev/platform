package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

// zipBytes builds an in-memory zip from name -> content.
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDetectTechnicLoader(t *testing.T) {
	// The forge 1.16.5 shape is the real one: the Solder "forge" mod of a live
	// pack, whose bin/modpack.jar is the Forge installer (2026-09-19).
	forge1165 := `{"id":"1.16.5-forge-36.2.34","inheritsFrom":"1.16.5","libraries":[{"name":"net.minecraftforge:forge:1.16.5-36.2.34"},{"name":"org.ow2.asm:asm:9.1"}]}`
	forge1710 := `{"inheritsFrom":"1.7.10","libraries":[{"name":"net.minecraftforge:forge:1.7.10-10.13.4.1614-1.7.10"}]}`
	neo := `{"inheritsFrom":"1.21.1","libraries":[{"name":"net.neoforged:neoforge:21.1.74:universal"}]}`
	fabric := `{"inheritsFrom":"1.20.1","libraries":[{"name":"net.fabricmc:intermediary:1.20.1"},{"name":"net.fabricmc:fabric-loader:0.15.11"}]}`

	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  technicLoader
		err   error
	}{
		{"forge in modpack.jar", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "modpack.jar"), string(zipBytes(t, map[string]string{"version.json": forge1165, "install_profile.json": "{}"})))
		}, technicLoader{"forge", "1.16.5", "36.2.34"}, nil},
		{"forge 1.7.10 keeps its suffix", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "modpack.jar"), string(zipBytes(t, map[string]string{"version.json": forge1710})))
		}, technicLoader{"forge", "1.7.10", "10.13.4.1614-1.7.10"}, nil},
		{"neoforge drops the classifier", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "modpack.jar"), string(zipBytes(t, map[string]string{"version.json": neo})))
		}, technicLoader{"neoforge", "1.21.1", "21.1.74"}, nil},
		{"fabric in bin/version.json", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "version.json"), fabric)
		}, technicLoader{"fabric", "1.20.1", "0.15.11"}, nil},
		{"modpack.jar without version.json falls through to bin/version.json", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "modpack.jar"), string(zipBytes(t, map[string]string{"a.class": "x"})))
			writeFile(t, filepath.Join(d, "bin", "version.json"), fabric)
		}, technicLoader{"fabric", "1.20.1", "0.15.11"}, nil},
		{"no bin", func(t *testing.T, d string) {}, technicLoader{}, errNoTechnicLoader},
		{"unknown loader", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "version.json"), `{"inheritsFrom":"1.20.1","libraries":[{"name":"org.quiltmc:quilt-loader:0.26.0"}]}`)
		}, technicLoader{}, errNoTechnicLoader},
		{"no inheritsFrom", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, "bin", "version.json"), `{"libraries":[{"name":"net.fabricmc:fabric-loader:0.15.11"}]}`)
		}, technicLoader{}, errNoTechnicLoader},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(t, dir)
			got, err := detectTechnicLoader(dir)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

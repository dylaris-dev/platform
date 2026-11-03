package services

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// jarWith builds a zip whose entries named *.class carry a class header with
// that major version; other entries get plain bytes.
func jarWith(t *testing.T, entries map[string]uint16) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, major := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".class") {
			var hdr [8]byte
			binary.BigEndian.PutUint32(hdr[:4], 0xCAFEBABE)
			binary.BigEndian.PutUint16(hdr[6:], major)
			w.Write(hdr[:])
		} else {
			w.Write([]byte("{}"))
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestJarJavaRelease(t *testing.T) {
	for _, tt := range []struct {
		name    string
		entries map[string]uint16
		want    int
	}{
		{"java 21", map[string]uint16{"a/A.class": 65, "plugin.yml": 0}, 21},
		{"java 25 wins over 21", map[string]uint16{"a/A.class": 65, "a/B.class": 69}, 25},
		{"java 8", map[string]uint16{"a/A.class": 52}, 8},
		{"no classes", map[string]uint16{"data/x/function/f.mcfunction": 0, "pack.mcmeta": 0}, 0},
		{"multi-release entry ignored", map[string]uint16{"a/A.class": 52, "META-INF/versions/25/a/A.class": 69}, 8},
		{"module-info ignored", map[string]uint16{"a/A.class": 52, "module-info.class": 53}, 8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := jarWith(t, tt.entries)
			got, err := JarJavaRelease(r, r.Size())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("java = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestJarJavaReleaseRejectsBadInput(t *testing.T) {
	if _, err := JarJavaRelease(bytes.NewReader([]byte("not a zip")), 9); err == nil {
		t.Error("a non-zip was judged")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("a/A.class")
	w.Write([]byte("garbage!"))
	zw.Close()
	if _, err := JarJavaRelease(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
		t.Error("a class without the magic was judged")
	}
}

func TestJavaFromImage(t *testing.T) {
	for _, tt := range []struct {
		image string
		want  int
	}{
		{"ghcr.io/dylaris-dev/platform-mc-java21:latest", 21},
		{"ghcr.io/dylaris-dev/platform-mc-java25", 25},
		{"ghcr.io/bartis-dev/dylaris-platform-mc-java8:latest", 8},
		{"ghcr.io/dylaris-dev/platform-mc-java17@sha256:abc", 17},
		{"eclipse-temurin:21-jre", 0},
		{"", 0},
		{"ghcr.io/dylaris-dev/platform-mc-java:latest", 0},
	} {
		if got := JavaFromImage(tt.image); got != tt.want {
			t.Errorf("JavaFromImage(%q) = %d, want %d", tt.image, got, tt.want)
		}
	}
}

func javaVersion(id, num, typ, date string) ModrinthVersion {
	return ModrinthVersion{ID: id, ProjectID: "WE", VersionNum: num, VersionType: typ, DatePub: date,
		Loaders: []string{"paper"}, GameVersions: []string{"1.21.11"},
		Files: []ModrinthFile{{Filename: id + ".jar", URL: "https://cdn.modrinth.com/" + id + ".jar", Primary: true}}}
}

func TestJavaFit(t *testing.T) {
	v746 := javaVersion("v746", "7.4.6", "release", "2026-10-01")
	v745 := javaVersion("v745", "7.4.5", "release", "2026-09-01")
	v744 := javaVersion("v744", "7.4.4", "release", "2026-08-01")
	beta := javaVersion("vbeta", "7.4.5-beta", "beta", "2026-09-15")
	forOtherMC := javaVersion("v-old-mc", "7.3.0", "release", "2026-07-01")
	forOtherMC.GameVersions = []string{"1.20.4"}
	for _, tt := range []struct {
		name      string
		java      int
		needs     map[string]int // file java per version id; missing = download fails
		list      []ModrinthVersion
		installed string
		wantID    string
		wantErr   string
		wantTries int
	}{
		{"fits", 21, map[string]int{"v746": 21}, nil, "", "v746", "", 1},
		{"no classes", 21, map[string]int{"v746": 0}, nil, "", "v746", "", 1},
		{"unknown java is unchecked", 0, nil, nil, "", "v746", "", 0},
		{"unreadable jar is not judged", 21, map[string]int{}, nil, "", "v746", "", 1},
		{"falls back to the newest older build that loads", 21,
			map[string]int{"v746": 25, "v745": 21, "v744": 21},
			[]ModrinthVersion{v744, v746, v745, beta, forOtherMC}, "", "v745", "", 2},
		{"walks past a second too-new build", 21,
			map[string]int{"v746": 25, "v745": 25, "v744": 17},
			[]ModrinthVersion{v746, v745, v744}, "", "v744", "", 3},
		{"none fits names both Java versions", 21,
			map[string]int{"v746": 25, "v745": 25, "v744": 25},
			[]ModrinthVersion{v746, v745, v744}, "", "", "WorldEdit 7.4.6 needs Java 25; this server runs Java 21. Switch the server to Java 25 in Setup", 3},
		{"update stops at the installed build", 21,
			map[string]int{"v746": 25, "v745": 21},
			[]ModrinthVersion{v746, v745, v744}, "v745", "", "so it stays on 7.4.5", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tries := 0
			oldJava, oldList := modrinthFileJava, fetchProjectVersions
			modrinthFileJava = func(_ context.Context, f ModrinthFile) (int, error) {
				tries++
				n, ok := tt.needs[strings.TrimSuffix(f.Filename, ".jar")]
				if !ok {
					return 0, errors.New("cdn down")
				}
				return n, nil
			}
			fetchProjectVersions = func(context.Context, string, []string, []string) ([]ModrinthVersion, error) {
				return tt.list, nil
			}
			t.Cleanup(func() { modrinthFileJava, fetchProjectVersions = oldJava, oldList })

			root := v746
			got, err := JavaFit(context.Background(), &root, "WorldEdit", "paper", "1.21.11", tt.java, tt.installed)
			if tries != tt.wantTries {
				t.Errorf("jar downloads = %d, want %d", tries, tt.wantTries)
			}
			if tt.wantErr != "" {
				var fit *ModFitError
				if !errors.As(err, &fit) || !strings.Contains(fit.Reason, tt.wantErr) {
					t.Fatalf("err = %v, want a ModFitError containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got == nil || got.ID != tt.wantID {
				t.Fatalf("got %+v, %v; want %s", got, err, tt.wantID)
			}
		})
	}
}

// Each fallback step is a jar download, so the walk is bounded.
func TestJavaFitBoundsTheWalk(t *testing.T) {
	root := javaVersion("v99", "99", "release", "2026-12-31")
	list := []ModrinthVersion{root}
	for i := 0; i < 20; i++ {
		list = append(list, javaVersion("old", "1", "release", "2026-01-"+string(rune('a'+i))))
	}
	tries := 0
	oldJava, oldList := modrinthFileJava, fetchProjectVersions
	modrinthFileJava = func(context.Context, ModrinthFile) (int, error) { tries++; return 25, nil }
	fetchProjectVersions = func(context.Context, string, []string, []string) ([]ModrinthVersion, error) { return list, nil }
	t.Cleanup(func() { modrinthFileJava, fetchProjectVersions = oldJava, oldList })

	if _, err := JavaFit(context.Background(), &root, "X", "paper", "1.21.11", 21, ""); err == nil {
		t.Fatal("nothing loads, yet it fit")
	}
	if tries != 1+javaFallbackCap {
		t.Errorf("jar downloads = %d, want %d", tries, 1+javaFallbackCap)
	}
}

package packs

import (
	"bytes"
	"context"
	"dylaris-core/models"
	"dylaris-core/storage/modpack"
	"strings"
	"testing"
)

func TestForkArtifactStreamingVerificationAndSourcePreservation(t *testing.T) {
	for _, name := range []string{"valid", "wrong size", "wrong checksum", "unsafe archive", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			provider := &modpack.LocalProvider{Paths: []string{t.TempDir()}}
			var err error
			filename := "kubejs/server_scripts/script.js"
			if name == "unsafe archive" {
				filename = "../escape.js"
			}
			raw := zipBytes(t, map[string]string{filename: "console.log('fixture');"})
			checksum, _, _ := hashesOf(raw)
			if err = provider.Put(context.Background(), "source.zip", raw); err != nil {
				t.Fatal(err)
			}
			entry := models.BuildContentEntry{}
			entry.Filesize = int64(len(raw))
			entry.MD5 = checksum
			if name == "wrong size" {
				entry.Filesize++
			}
			if name == "wrong checksum" {
				entry.MD5 = strings.Repeat("0", 32)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			err = copyForkArtifact(ctx, provider, "source.zip", "packs/target/forks/fixture/copied.zip", entry)
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				copied, err := provider.Get(context.Background(), "packs/target/forks/fixture/copied.zip")
				if err != nil || !bytes.Equal(copied, raw) {
					t.Fatal("fork changed uploaded bytes")
				}
			} else if err == nil {
				t.Fatal("invalid source was copied")
			}
			original, err := provider.Get(context.Background(), "source.zip")
			if err != nil || !bytes.Equal(original, raw) {
				t.Fatal("fork damaged source artifact")
			}
			if name == "valid" {
				if err = provider.Delete(context.Background(), "source.zip"); err != nil {
					t.Fatal(err)
				}
				retained, err := provider.Get(context.Background(), "packs/target/forks/fixture/copied.zip")
				if err != nil || !bytes.Equal(retained, raw) {
					t.Fatal("source storage deletion damaged independent copy")
				}
			}
		})
	}
}

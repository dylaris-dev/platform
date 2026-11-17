package main

import (
	"archive/tar"
	"bytes"
	stdgzip "compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// BenchmarkWriteServerArchive archives a real server directory, named by
// DYLARIS_BENCH_SERVER_DIR, into a byte counter: the CPU cost of tar+gzip on
// the mix a backup actually sees (region files that are already compressed,
// jars, configs, logs). SetBytes is the input size, so MB/s is the rate a
// backup reads the server at; "archive-bytes" is the output size.
func BenchmarkWriteServerArchive(b *testing.B) {
	dir := os.Getenv("DYLARIS_BENCH_SERVER_DIR")
	if dir == "" {
		b.Skip("DYLARIS_BENCH_SERVER_DIR not set")
	}
	b.SetBytes(dirSize(dir))
	var out int64
	for b.Loop() {
		var c countingWriter
		if _, err := writeServerArchive(&c, dir, dir, nil, nil, nil); err != nil {
			b.Fatal(err)
		}
		out = c.Total()
	}
	b.ReportMetric(float64(out), "archive-bytes")
}

// The backup writer's archive is plain gzip that the stdlib reads, as the
// restore path does, and completeArchive still accepts an archive an older
// node wrote with the stdlib and refuses one cut short or corrupted.
func TestBackupArchiveGzipCompat(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "server.properties"), bytes.Repeat([]byte("motd=hi\n"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if _, err := writeServerArchive(&archive, src, src, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	gr, err := stdgzip.NewReader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stdlib read: %v", err)
		}
		found = found || h.Name == "server.properties"
	}
	if !found {
		t.Fatal("server.properties not in the archive")
	}

	var old bytes.Buffer
	ow := stdgzip.NewWriter(&old)
	ow.Write(bytes.Repeat([]byte("old node archive "), 1000))
	ow.Close()
	corrupt := bytes.Clone(old.Bytes())
	corrupt[len(corrupt)-6] ^= 0xff // inside the CRC-32 trailer

	base := t.TempDir()
	info := storageInfo{Provider: "local", Config: json.RawMessage(`{"basePath":` + strconv.Quote(base) + `}`)}
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"new writer", archive.Bytes(), true},
		{"stdlib writer", old.Bytes(), true},
		{"cut short", old.Bytes()[:old.Len()/2], false},
		{"bad checksum", corrupt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(base, "a.tar.gz"), tc.body, 0o644); err != nil {
				t.Fatal(err)
			}
			size, ok := completeArchive(nil, "", info, "a.tar.gz")
			if ok != tc.ok || (ok && size != int64(len(tc.body))) {
				t.Fatalf("completeArchive = %d, %v; want ok=%v", size, ok, tc.ok)
			}
		})
	}
}

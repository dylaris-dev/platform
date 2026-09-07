package backup

import (
	"context"
	"encoding/json"
	"testing"
)

func newS3ForTest(t *testing.T, prefix string) *S3Storage {
	t.Helper()
	raw, err := json.Marshal(S3Config{
		Endpoint:        "https://example.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "dylaris",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		Prefix:          prefix,
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	st, err := NewS3(context.Background(), raw)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return st
}

// The account backup-storage form has always collected a prefix, stored it, and
// displayed it back as "bucket/prefix" - while S3Config had no field to
// unmarshal it into, so it was dropped and every archive went to the bucket
// root. The UI confirmed a placement that was not happening.
func TestS3PrefixIsAppliedToKeys(t *testing.T) {
	s := newS3ForTest(t, "server-backups")

	if got, want := s.key("srv-1/2026-09-07.tar.gz"), "server-backups/srv-1/2026-09-07.tar.gz"; got != want {
		t.Errorf("key() = %q, want %q", got, want)
	}
	// A caller must read back exactly the key it wrote. Retention lists and
	// then compares against keys it recorded itself; leaving the prefix on
	// would match nothing and silently stop pruning rather than fail.
	if got, want := s.unkey("server-backups/srv-1/2026-09-07.tar.gz"), "srv-1/2026-09-07.tar.gz"; got != want {
		t.Errorf("unkey() = %q, want %q", got, want)
	}
	if got := s.unkey(s.key("srv-1/x.tar.gz")); got != "srv-1/x.tar.gz" {
		t.Errorf("unkey(key(x)) = %q, want the original", got)
	}
}

// Slashes the operator may or may not type must not produce "a//b" or "/a/b" -
// those are legal but DIFFERENT S3 keys, so a stray slash would split one
// backup set across two prefixes.
func TestS3PrefixNormalisesSlashes(t *testing.T) {
	for _, prefix := range []string{"server-backups", "/server-backups", "server-backups/", "/server-backups/"} {
		s := newS3ForTest(t, prefix)
		if got, want := s.key("a.tar.gz"), "server-backups/a.tar.gz"; got != want {
			t.Errorf("prefix %q: key() = %q, want %q", prefix, got, want)
		}
	}
}

// Empty prefix must stay a pure passthrough. Existing rows have been writing to
// the root, and this is the case that keeps finding them.
func TestS3EmptyPrefixIsPassthrough(t *testing.T) {
	s := newS3ForTest(t, "")
	if got := s.key("srv-1/x.tar.gz"); got != "srv-1/x.tar.gz" {
		t.Errorf("key() = %q, want the key unchanged", got)
	}
	if got := s.unkey("srv-1/x.tar.gz"); got != "srv-1/x.tar.gz" {
		t.Errorf("unkey() = %q, want the key unchanged", got)
	}
}

// The control that matters for the OTHER subsystem: Core file storage builds
// this same client through storage.newS3ProviderFromOpts, which fills S3Config
// field by field and deliberately leaves Prefix out - it applies its own prefix
// in S3Provider.key(). If a future edit passes the prefix into the config too,
// every core-storage object would be written under it twice, so pin that an
// unmarshalled config without a "prefix" key produces no prefix at all.
func TestS3ConfigWithoutPrefixKeyHasNoPrefix(t *testing.T) {
	raw := []byte(`{"endpoint":"https://x","region":"auto","bucket":"dylaris","accessKeyId":"k","secretAccessKey":"s"}`)
	st, err := NewS3(context.Background(), raw)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if st.prefix != "" {
		t.Errorf("prefix = %q, want empty for a config that carries no prefix key", st.prefix)
	}
}

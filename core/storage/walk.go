package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// WalkedFile is one file discovered by WalkProvider. Key is forward-slash
// separated and RELATIVE to the walk root (never leading "/"), so a caller
// that already scoped its provider to a sub-prefix gets provider-relative
// keys straight back.
type WalkedFile struct {
	Key  string
	Size int64
}

// MaxWalkDepth caps how deep WalkProvider will recurse. This exists to fail
// loudly rather than spin forever on a pathological backend (an object store
// whose synthesized directory listing echoes its own prefix would otherwise
// recurse without end). 64 is far past any real backup tree.
const MaxWalkDepth = 64

// ErrWalkTooDeep is returned when a tree exceeds MaxWalkDepth.
var ErrWalkTooDeep = errors.New("storage: directory tree deeper than MaxWalkDepth")

// WalkProvider enumerates EVERY file under root by recursing through
// StorageProvider.ListFiles, which returns ONE directory level only on both
// LocalProvider (os.ReadDir) and S3Provider (a synthesized single level over
// the flat key space). Anything that needs the full key space must therefore
// recurse itself; this is that one implementation.
//
// A missing directory is not an error: LocalProvider.ListFiles already returns
// an empty slice for a non-existent path, and an object store has no empty
// directories at all. A real backend failure IS propagated, so a transient
// 503 can never be mistaken for "this prefix is empty" by the migration
// engine.
//
// The result is sorted ascending by Key so the copy loop and the manifest are
// deterministic across runs and backends.
func WalkProvider(ctx context.Context, p StorageProvider, root string) ([]WalkedFile, error) {
	out := []WalkedFile{}
	if err := walkInto(ctx, p, root, "", 0, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// walkInto lists one level at dir (the provider-relative path) and recurses.
// rel is the key prefix accumulated relative to the ORIGINAL walk root.
func walkInto(ctx context.Context, p StorageProvider, dir, rel string, depth int, out *[]WalkedFile) error {
	if depth > MaxWalkDepth {
		return fmt.Errorf("%w (at %q)", ErrWalkTooDeep, dir)
	}
	entries, err := p.ListFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("storage: list %q: %w", dir, err)
	}
	for _, e := range entries {
		childDir := e.Name
		if dir != "" {
			childDir = dir + "/" + e.Name
		}
		childRel := e.Name
		if rel != "" {
			childRel = rel + "/" + e.Name
		}
		if e.IsDir {
			if err := walkInto(ctx, p, childDir, childRel, depth+1, out); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, WalkedFile{Key: childRel, Size: e.Size})
	}
	return nil
}

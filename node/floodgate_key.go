package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// floodgateKeyLen is the AES-128 key Floodgate and Geyser share.
const floodgateKeyLen = 16

// writeFloodgateKey puts the shared Floodgate key where Floodgate reads it
// (key-file-name: key.pem in its data folder): plugins/floodgate on
// Paper/Spigot, config/floodgate on Fabric and NeoForge
// (FabricFloodgateMod and NeoForgeFloodgateMod resolve the config dir + "floodgate").
// serverDir is the sub-server directory. Nothing is written when neither layout
// is present, so a server without Floodgate is left alone.
//
// Floodgate generates its own key on first start; ours must win so Geyser and
// every Floodgate agree, hence the overwrite.
func writeFloodgateKey(serverDir string, key []byte) error {
	var rel string
	switch {
	case isDir(filepath.Join(serverDir, "plugins")):
		rel = "plugins/floodgate/key.pem"
	case isDir(filepath.Join(serverDir, "mods")):
		rel = "config/floodgate/key.pem"
	default:
		return nil
	}
	root, leaf, err := writeScope(serverDir, rel, true)
	if err != nil {
		return fmt.Errorf("floodgate key: %w", err)
	}
	defer root.Close()
	if err := root.WriteFile(leaf, key, nodeOwnedFileMode); err != nil {
		return fmt.Errorf("floodgate key: %w", err)
	}
	// WriteFile keeps the mode of a file that already existed.
	if err := root.Chmod(leaf, nodeOwnedFileMode); err != nil {
		return fmt.Errorf("floodgate key: %w", err)
	}
	// The key is secret, but Floodgate runs as the container's uid and must read it.
	chownForMCIn(root, leaf)
	return nil
}

func isDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// applyFloodgateKey writes the key Core published, if any. It never blocks or
// fails a start: an absent key (old Core, Bedrock off), a Redis error or a value
// that is not exactly 16 bytes once decoded only logs.
func applyFloodgateKey(serverDir string, fetch func() (string, bool, error)) {
	val, found, err := fetch()
	if err != nil {
		log.Printf("floodgate key: not read, starting without it: %v", err)
		return
	}
	if !found {
		return
	}
	key, err := base64.StdEncoding.DecodeString(val)
	if err != nil || len(key) != floodgateKeyLen {
		log.Printf("floodgate key: refusing the published value, it is not %d base64-encoded bytes", floodgateKeyLen)
		return
	}
	if err := writeFloodgateKey(serverDir, key); err != nil {
		log.Printf("floodgate key: not written, starting anyway: %v", err)
	}
}

// provisionFloodgateKey runs before a server is (re)started. subName is the
// active sub-server; empty falls back to .active_server on disk.
func provisionFloodgateKey(ctx context.Context, rdb *redis.Client, serverPath, uuid, subName string) {
	if !validActiveSubServer(subName) || subName == "" {
		b, _ := os.ReadFile(filepath.Join(serverPath, ".active_server"))
		subName = strings.TrimSpace(string(b))
		if subName == "" || !validActiveSubServer(subName) {
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	applyFloodgateKey(filepath.Join(serverPath, subName), func() (string, bool, error) {
		v, err := rdb.Get(ctx, "dylaris:server:"+uuid+":floodgate").Result()
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return v, err == nil, err
	})
}

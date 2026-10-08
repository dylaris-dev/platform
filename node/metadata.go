package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SubServerMetadata struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	MinecraftVersion string `json:"minecraft_version,omitempty"`
	Build            string `json:"build,omitempty"`
	ExtraJvmFlags    string `json:"extra_jvm_flags,omitempty"`
}

type ServerMetadata struct {
	Version         int                 `json:"version"`
	ServerUUID      string              `json:"server_uuid"`
	Name            string              `json:"name"`
	MemoryMB        int                 `json:"memory_mb"`
	CPULimit        float64             `json:"cpu_limit"`
	GameImage       string              `json:"game_image,omitempty"`
	ActiveSubServer string              `json:"active_sub_server,omitempty"`
	SubServers      []SubServerMetadata `json:"sub_servers"`
	UpdatedAt       string              `json:"updated_at"`
}

func metadataPath(serverDir string) string {
	return filepath.Join(serverDir, ".dylaris.json")
}

// writeServerMetadata serializes the metadata to <serverDir>/.dylaris.json.
// The write is atomic (temp file + rename). Version defaults to 1.
func writeServerMetadata(serverDir string, m ServerMetadata) error {
	if m.Version == 0 {
		m.Version = 1
	}
	m.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := metadataPath(serverDir) + ".tmp"
	if err := os.WriteFile(tmp, data, nodeOwnedFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, metadataPath(serverDir))
}

func readServerMetadata(serverDir string) (ServerMetadata, error) {
	var m ServerMetadata
	data, err := os.ReadFile(metadataPath(serverDir))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse .dylaris.json: %w", err)
	}
	return m, nil
}

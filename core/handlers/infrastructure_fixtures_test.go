package handlers

import (
	"dylaris-core/models"
	"dylaris-core/store"
)

// A copy of nodes/nodes_link_online_test.go's nodeListStore, for the
// infrastructure overview test.

type nodeListStore struct {
	store.Store
	settings map[string]string
	nodes    []models.Node
}

func (f *nodeListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *nodeListStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }

func (f *nodeListStore) CountServersByNode(int) (int, error) { return 0, nil }

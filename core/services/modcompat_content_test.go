package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBuildMatrixDoesNotRequireForgeTagForResourcePack(t *testing.T) {
	previous := compatHTTP
	defer func() { compatHTTP = previous }()
	compatHTTP = &http.Client{Transport: publishingContractTransport(func(r *http.Request) (*http.Response, error) {
		facets := r.URL.Query().Get("facets")
		hits := []map[string]string{}
		if !strings.Contains(facets, "versions:") {
			hits = append(hits, map[string]string{"project_id": "mod"}, map[string]string{"project_id": "resource"})
		} else if strings.Contains(facets, "categories:forge") {
			hits = append(hits, map[string]string{"project_id": "mod"})
		} else {
			hits = append(hits, map[string]string{"project_id": "resource"})
		}
		body, _ := json.Marshal(map[string]interface{}{"hits": hits})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	matrix, err := BuildMatrix(context.Background(), CompatRequest{Items: []CompatItem{{Key: "mod", ProjectID: "mod", ContentType: "mod", Side: "both"}, {Key: "resource", ProjectID: "resource", ContentType: "resourcepack", Side: "client"}}, Loader: "forge", Targets: []string{"1.20.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(matrix.Lines) != 1 || len(matrix.Lines[0].Versions[0].Missing) != 0 {
		t.Fatalf("resource pack was incorrectly filtered by Forge: %+v", matrix)
	}
}

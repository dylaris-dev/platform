package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type publishingContractTransport func(*http.Request) (*http.Response, error)

func (f publishingContractTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProjectCreationSendsRequiredEmptyArraysAndDraft(t *testing.T) {
	client := NewModrinthClient("test-token")
	client.httpClient = &http.Client{Transport: publishingContractTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/v2/project" {
			t.Fatalf("unexpected endpoint %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.FormValue("data")), &payload); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"initial_versions", "categories"} {
			if string(payload[field]) != "[]" {
				t.Errorf("%s=%s; required empty arrays must not be omitted or null", field, payload[field])
			}
		}
		if string(payload["is_draft"]) != "true" {
			t.Fatal("Modrinth requires new projects to be drafts")
		}
		if string(payload["license_id"]) != `"LicenseRef-All-Rights-Reserved"` {
			t.Fatalf("invalid all-rights-reserved identifier: %s", payload["license_id"])
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"project-id","slug":"test"}`))}, nil
	})}
	project, err := client.CreateProject(context.Background(), CreateProjectRequest{Slug: "test", Title: "Test", ProjectType: "modpack"})
	if err != nil || project.ID != "project-id" {
		t.Fatalf("create: %v %v", project, err)
	}
}

func TestProjectLicenseNormalizationPreservesExplicitLicenses(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"arr", ModrinthAllRightsReserved}, {"ARR", ModrinthAllRightsReserved},
		{ModrinthAllRightsReserved, ModrinthAllRightsReserved}, {"MIT", "MIT"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			client := NewModrinthClient("test-token")
			client.httpClient = &http.Client{Transport: publishingContractTransport(func(r *http.Request) (*http.Response, error) {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Fatal(err)
				}
				defer r.MultipartForm.RemoveAll()
				var body CreateProjectRequest
				if err := json.Unmarshal([]byte(r.FormValue("data")), &body); err != nil {
					t.Fatal(err)
				}
				if body.License != tc.want {
					t.Fatalf("license=%q want=%q", body.License, tc.want)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"project-id"}`))}, nil
			})}
			if _, err := client.CreateProject(context.Background(), CreateProjectRequest{License: tc.input}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Minimal Modrinth API client for publishing modpacks on
// behalf of users. Uses the per-user PAT obtained via the PAT mgmt handler.
//
// We only implement the endpoints we actually call:
//
//	POST /v2/project              — create modpack project (first publish)
//	POST /v2/version              — upload a new version (with .mrpack file)
//	GET  /v2/project/{slug}/check — slug availability
//	POST /v2/project/{id}/members — invite collaborator
//	DEL  /v2/project/{id}/members/{user} — remove collaborator
//	GET  /v2/project/{id}/members — list collaborators
package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const ModrinthAPIBase = "https://api.modrinth.com/v2"

type ModrinthClient struct {
	httpClient *http.Client
	userAgent  string
	pat        string
}

func NewModrinthClient(pat string) *ModrinthClient {
	return &ModrinthClient{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		userAgent:  DylarisUserAgent(),
		pat:        pat,
	}
}

// --- Project ---

type CreateProjectRequest struct {
	Slug            string   `json:"slug"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	Body            string   `json:"body"`
	ProjectType     string   `json:"project_type"` // "modpack"
	ClientSide      string   `json:"client_side"`  // "required"|"optional"|"unsupported"
	ServerSide      string   `json:"server_side"`  // same
	License         string   `json:"license_id"`   // SPDX slug; "arr" for all-rights-reserved
	IsDraft         bool     `json:"is_draft"`     // true → not published, requires explicit publish
	Categories      []string `json:"categories"`   // optional
	AdditionalCats  []string `json:"additional_categories,omitempty"`
	InitialVersions []string `json:"initial_versions,omitempty"`
}

type ProjectResponse struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// CreateProject creates a modpack project on Modrinth. The plaintext PAT
// authorises; project ownership lands on the PAT's user.
func (c *ModrinthClient) CreateProject(ctx context.Context, req CreateProjectRequest) (*ProjectResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Modrinth's POST /v2/project is multipart: a "data" field with the
	// JSON payload + optional file fields (icon, file). We only send data.
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("data", string(body)); err != nil {
		return nil, err
	}
	mw.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ModrinthAPIBase+"/project", buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", c.pat)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("modrinth create project: status %d: %s", resp.StatusCode, truncateBody(rawBody))
	}
	var p ProjectResponse
	if err := json.Unmarshal(rawBody, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// --- Version ---

type CreateVersionRequest struct {
	Name          string   `json:"name"`
	VersionNumber string   `json:"version_number"`
	Changelog     string   `json:"changelog"`
	Dependencies  []string `json:"dependencies"` // empty for modpacks (deps live in mrpack)
	GameVersions  []string `json:"game_versions"`
	VersionType   string   `json:"version_type"` // "release"|"beta"|"alpha"
	Loaders       []string `json:"loaders"`
	Featured      bool     `json:"featured"`
	Status        string   `json:"status"` // "listed"|"draft"|"unlisted"
	ProjectID     string   `json:"project_id"`
	FileParts     []string `json:"file_parts"` // ["primary"]
	PrimaryFile   string   `json:"primary_file"`
}

type VersionResponse struct {
	ID string `json:"id"`
}

// UploadVersion creates a new version on an existing Modrinth project,
// streaming the supplied .mrpack bytes as the "primary" file. The mrpack
// builder in handlers/modpacks_export.go already produced the bytes; we
// just hand them off here.
func (c *ModrinthClient) UploadVersion(ctx context.Context, meta CreateVersionRequest, mrpack []byte, filename string) (*VersionResponse, error) {
	meta.FileParts = []string{"primary"}
	meta.PrimaryFile = "primary"
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("data", string(data)); err != nil {
		return nil, err
	}
	part, err := mw.CreateFormFile("primary", filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(mrpack); err != nil {
		return nil, err
	}
	mw.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ModrinthAPIBase+"/version", buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", c.pat)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("modrinth upload version: status %d: %s", resp.StatusCode, truncateBody(body))
	}
	var v VersionResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CheckSlug returns ("", nil) when the slug is free, or the existing
// project ID when it's already taken.
func (c *ModrinthClient) CheckSlug(ctx context.Context, slug string) (string, error) {
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ModrinthAPIBase+"/project/"+slug+"/check", nil)
	httpReq.Header.Set("User-Agent", c.userAgent)
	if c.pat != "" {
		httpReq.Header.Set("Authorization", c.pat)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check slug: status %d", resp.StatusCode)
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// --- Collaborators ---

type CollaboratorInvite struct {
	UserID      string `json:"user_id"`
	Role        string `json:"role"`        // free-form, "Developer" works
	Permissions int    `json:"permissions"` // bitfield, 0 = read-only (Modrinth defaults role-based perms anyway)
}

// MemberResponse is one entry from GET /v2/project/{id}/members.
type MemberResponse struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Role string `json:"role"`
}

func (c *ModrinthClient) ListProjectMembers(ctx context.Context, projectID string) ([]MemberResponse, error) {
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ModrinthAPIBase+"/project/"+projectID+"/members", nil)
	httpReq.Header.Set("Authorization", c.pat)
	httpReq.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list members: status %d", resp.StatusCode)
	}
	var members []MemberResponse
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, err
	}
	return members, nil
}

func (c *ModrinthClient) InviteMember(ctx context.Context, projectID, username string) error {
	// We have to translate username → Modrinth user_id first.
	uidReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ModrinthAPIBase+"/user/"+username, nil)
	uidReq.Header.Set("User-Agent", c.userAgent)
	uidReq.Header.Set("Authorization", c.pat)
	uidResp, err := c.httpClient.Do(uidReq)
	if err != nil {
		return err
	}
	defer uidResp.Body.Close()
	if uidResp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("modrinth user %q not found", username)
	}
	if uidResp.StatusCode != http.StatusOK {
		return fmt.Errorf("modrinth user lookup: status %d", uidResp.StatusCode)
	}
	var u struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(uidResp.Body).Decode(&u); err != nil {
		return err
	}

	body, _ := json.Marshal(CollaboratorInvite{UserID: u.ID, Role: "Tester", Permissions: 0})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		ModrinthAPIBase+"/project/"+projectID+"/members", strings.NewReader(string(body)))
	req.Header.Set("Authorization", c.pat)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("invite member: status %d: %s", resp.StatusCode, truncateBody(raw))
	}
	return nil
}

func (c *ModrinthClient) RemoveMember(ctx context.Context, projectID, modrinthUserID string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete,
		ModrinthAPIBase+"/project/"+projectID+"/members/"+modrinthUserID, nil)
	req.Header.Set("Authorization", c.pat)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remove member: status %d: %s", resp.StatusCode, truncateBody(raw))
	}
	return nil
}

func truncateBody(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "…"
	}
	return string(b)
}

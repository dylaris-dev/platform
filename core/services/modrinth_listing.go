package services

// The project page of a published pack: what Modrinth shows, its review
// checklist, and the submission. Everything here acts on the pack OWNER's
// project with the owner's PAT, like publishing does.
//
// Two API generations on purpose. Project fields are edited through v2, which
// still takes them and is what the rest of this client speaks. The review
// checklist (GET /project/{id}/validate), the submission (status
// "processing") and a version's environment exist only in v3 - the website
// uses exactly these calls (modrinth/code, ModerationProjectNags.vue,
// link-network-validation/submission.ts).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const modrinthAPIv3 = "https://api.modrinth.com/v3"

// ModrinthProjectPage is the subset of GET /v2/project the panel edits or shows.
type ModrinthProjectPage struct {
	ID                   string   `json:"id"`
	Slug                 string   `json:"slug"`
	Title                string   `json:"title"`
	Description          string   `json:"description"` // the summary line
	Body                 string   `json:"body"`        // the long markdown description
	Status               string   `json:"status"`
	RequestedStatus      string   `json:"requested_status"`
	Categories           []string `json:"categories"`
	AdditionalCategories []string `json:"additional_categories"`
	IconURL              *string  `json:"icon_url"`
	SourceURL            *string  `json:"source_url"`
	IssuesURL            *string  `json:"issues_url"`
	WikiURL              *string  `json:"wiki_url"`
	DiscordURL           *string  `json:"discord_url"`
	Versions             []string `json:"versions"`
	License              struct {
		ID  string  `json:"id"`
		URL *string `json:"url"`
	} `json:"license"`
	ModeratorMessage *struct {
		Message string  `json:"message"`
		Body    *string `json:"body"`
	} `json:"moderator_message"`
}

// ModrinthNag is one item of Modrinth's publishing checklist. Severity is
// "required" (blocks submission), "warning" or "suggestion".
type ModrinthNag struct {
	Kind     string                 `json:"kind"`
	Severity string                 `json:"severity"`
	Details  map[string]interface{} `json:"details,omitempty"`
}

func (c *ModrinthClient) do(ctx context.Context, method, rawURL string, body io.Reader, contentType string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.pat)
	req.Header.Set("User-Agent", c.userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("modrinth %s %s: status %d: %s", method, req.URL.Path, resp.StatusCode, truncateBody(raw))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *ModrinthClient) doJSON(ctx context.Context, method, rawURL string, in, out interface{}) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return c.do(ctx, method, rawURL, body, "application/json", out)
}

// GetProjectPage reads the project as its owner sees it (drafts included).
func (c *ModrinthClient) GetProjectPage(ctx context.Context, projectID string) (*ModrinthProjectPage, error) {
	var p ModrinthProjectPage
	if err := c.doJSON(ctx, http.MethodGet, ModrinthAPIBase+"/project/"+url.PathEscape(projectID), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// EditProject applies v2 project fields (title, description, body,
// categories, additional_categories, license_id, license_url, source_url,
// issues_url, wiki_url, discord_url). Modrinth answers 204.
func (c *ModrinthClient) EditProject(ctx context.Context, projectID string, fields map[string]interface{}) error {
	return c.doJSON(ctx, http.MethodPatch, ModrinthAPIBase+"/project/"+url.PathEscape(projectID), fields, nil)
}

// ValidateProject returns Modrinth's own publishing checklist for the project.
func (c *ModrinthClient) ValidateProject(ctx context.Context, projectID string) ([]ModrinthNag, error) {
	var out struct {
		Nags []ModrinthNag `json:"nags"`
	}
	if err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/project/"+url.PathEscape(projectID)+"/validate", nil, &out); err != nil {
		return nil, err
	}
	return out.Nags, nil
}

// SubmitForReview sends a draft (or rejected) project to Modrinth's moderators.
// Modrinth refuses it while a required checklist item is open.
func (c *ModrinthClient) SubmitForReview(ctx context.Context, projectID string) error {
	return c.doJSON(ctx, http.MethodPatch, modrinthAPIv3+"/project/"+url.PathEscape(projectID), map[string]string{"status": "processing"}, nil)
}

// SetVersionEnvironment sets where a version runs ("client_and_server",
// "client_only", ...). Modrinth keeps this per version, and v2 version uploads
// cannot set it, so every upload needs this follow-up or the checklist demands it.
func (c *ModrinthClient) SetVersionEnvironment(ctx context.Context, versionID, environment string) error {
	return c.doJSON(ctx, http.MethodPatch, modrinthAPIv3+"/version/"+url.PathEscape(versionID), map[string]string{"environment": environment}, nil)
}

// VersionEnvironment reads a version's environment ("unknown" when unset).
func (c *ModrinthClient) VersionEnvironment(ctx context.Context, versionID string) (string, error) {
	var v struct {
		Environment string `json:"environment"`
	}
	if err := c.doJSON(ctx, http.MethodGet, modrinthAPIv3+"/version/"+url.PathEscape(versionID), nil, &v); err != nil {
		return "", err
	}
	return v.Environment, nil
}

// SetProjectIcon replaces the project icon; ext is the image type
// (png, jpg, webp, gif), contentType its MIME type, the body the raw image.
func (c *ModrinthClient) SetProjectIcon(ctx context.Context, projectID, ext, contentType string, image []byte) error {
	return c.do(ctx, http.MethodPatch, ModrinthAPIBase+"/project/"+url.PathEscape(projectID)+"/icon?ext="+url.QueryEscape(ext), bytes.NewReader(image), contentType, nil)
}

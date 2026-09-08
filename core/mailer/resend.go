package mailer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Resend, the HTTP mail transport.
//
// It exists because "configure SMTP" is a real obstacle for someone standing up
// a panel: it means a mail server, a reverse DNS entry and an SPF record before
// a single verification mail goes out. An API key and a verified domain is the
// version of that job most people can finish.
//
// Setting keys:
//
//	resend.api_key   - the credential. Encrypted at rest (store.settingsSecretKeys).
//	resend.endpoint  - override, for a test double. Empty means the real API.
//
// The FROM address is shared with SMTP, see fromIdentity: switching provider is
// not a reason to retype your own address.
const (
	SettingKeyResendAPIKey  = "resend.api_key"
	settingKeyResendBaseURL = "resend.endpoint"
	resendDefaultEndpoint   = "https://api.resend.com/emails"
	resendTimeout           = 15 * time.Second
)

type resendTransport struct {
	apiKey    string
	endpoint  string
	fromEmail string
	fromName  string
	client    *http.Client
}

func loadResend(s SettingsReader, purpose string) (Transport, error) {
	key, _ := s.GetSetting(SettingKeyResendAPIKey)
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("resend api key not configured")
	}
	fromEmail, fromName := fromIdentity(s, purpose)
	if fromEmail == "" {
		// Resend rejects a message with no from, and the rejection is a 422 with
		// a body nobody reads. Say it here, where the operator can act on it.
		return nil, errors.New("from address not configured")
	}
	endpoint, _ := s.GetSetting(settingKeyResendBaseURL)
	if strings.TrimSpace(endpoint) == "" {
		endpoint = resendDefaultEndpoint
	}
	return &resendTransport{
		apiKey:    key,
		endpoint:  strings.TrimSpace(endpoint),
		fromEmail: fromEmail,
		fromName:  fromName,
		client:    &http.Client{Timeout: resendTimeout},
	}, nil
}

func (t *resendTransport) Describe() string { return "Resend" }

func (t *resendTransport) Send(msg Message) error {
	if msg.To == "" {
		return errors.New("empty recipient")
	}
	from := t.fromEmail
	if t.fromName != "" {
		from = fmt.Sprintf("%s <%s>", t.fromName, t.fromEmail)
	}
	// Text is always sent; HTML rides alongside it when the template has one.
	// Resend builds the multipart/alternative itself, so this mirrors what the
	// SMTP transport assembles by hand - and sending html WITHOUT text would
	// leave a text-only reader with an empty message.
	payload := map[string]any{
		"from":    from,
		"to":      []string{msg.To},
		"subject": msg.Subject,
		"text":    msg.Body,
	}
	if msg.HTML != "" {
		payload["html"] = msg.HTML
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Resend's own message names the actual problem - an unverified domain,
		// a malformed from - and is far more use than the status code. Bounded,
		// because it ends up in a toast.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("resend returned %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

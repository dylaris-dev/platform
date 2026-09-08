package services

import (
	"fmt"

	"dylaris-core/mailer"
	"dylaris-core/models"
)

// Sending one of the platform's defined mails.
//
// One place rather than a copy per send site, because the steps are easy to get
// subtly different: resolve the definition, load the operator's override if
// there is one, pick the transport for the definition's PURPOSE, render, send
// both parts. The old code did steps three and five inline at each site and
// built the body with fmt.Sprintf, which is why the wording could not be edited
// and why two of the sites had drifted apart in tone.

// MailStore is what dispatching needs. Deliberately narrow: this must be
// callable from handlers and from background services without either of them
// handing over the whole store.
type MailStore interface {
	GetSetting(key string) (string, error)
	GetMailTemplate(key string) (*models.MailTemplate, error)
}

// RenderMail resolves the definition, applies the operator's override and
// renders it. Exported separately from SendMail so the preview endpoint shows
// exactly what would be sent rather than an approximation of it.
func RenderMail(st MailStore, key, panelURL string, vars map[string]string) (mailer.Definition, mailer.Rendered, error) {
	def, ok := mailer.DefinitionByKey(key)
	if !ok {
		return def, mailer.Rendered{}, fmt.Errorf("unknown mail template %q", key)
	}

	var override *mailer.Template
	if row, err := st.GetMailTemplate(key); err == nil && row != nil {
		override = &mailer.Template{Key: row.Key, Subject: row.Subject, Body: row.Body}
	}
	// A read error is deliberately NOT fatal here: a database blip must not
	// stop a password reset going out. The default wording is always available,
	// and sending the default beats sending nothing.

	_, senderName := mailer.SenderIdentity(st, def.Purpose)
	brand := mailer.Brand{SiteName: senderName, PanelURL: panelURL}

	return def, mailer.Render(def, override, brand, vars), nil
}

// SendMail renders and sends one defined mail.
func SendMail(st MailStore, key, to, panelURL string, vars map[string]string) error {
	def, out, err := RenderMail(st, key, panelURL, vars)
	if err != nil {
		return err
	}
	transport, err := mailer.Load(st, def.Purpose)
	if err != nil {
		return fmt.Errorf("mail not configured: %w", err)
	}
	return transport.Send(mailer.Message{
		To:      to,
		Subject: out.Subject,
		Body:    out.Text,
		HTML:    out.HTML,
	})
}

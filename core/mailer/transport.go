package mailer

import (
	"errors"
	"strings"
)

// A transport is the thing that actually delivers a message.
//
// There used to be exactly one, hardcoded: net/smtp. That is fine for a
// self-hoster with a mail server and awkward for everyone else, because the
// usual answer today is an HTTP mail API. The seam is deliberately narrow - one
// method - so adding a provider is writing a Send and nothing else.
//
// The FROM identity is not part of it. It is a property of the mail
// configuration rather than of the wire protocol, so both transports read the
// same stored values and switching provider does not ask the operator to retype
// their own address.
type Transport interface {
	// Send blocks until the message is delivered or fails. Synchronous on
	// purpose, so a caller can decide between firing it into a goroutine and
	// surfacing the error (the admin test-send does the latter).
	Send(msg Message) error
	// Describe names the transport for an error or a log line, e.g.
	// `SMTP smtp.example.com:587`. Without it a failure reads the same
	// regardless of which provider produced it.
	Describe() string
}

// Provider names, as stored in the `mail.provider` setting.
const (
	ProviderSMTP   = "smtp"
	ProviderResend = "resend"
)

// SettingKeyProvider selects the transport. Empty means SMTP, so an install
// that predates this reads exactly as it did.
const SettingKeyProvider = "mail.provider"

// ErrNoTransport is returned when nothing is configured well enough to send.
// Callers already treat a load failure as "mail is off"; this just names it.
var ErrNoTransport = errors.New("no mail transport configured")

// Load resolves the transport for a purpose ("auth", "default", ...).
//
// Purpose exists because a deployment may want ticket replies to come from a
// different address than password resets. It is honoured by the SMTP profile
// keys, which is where it has always lived.
func Load(s SettingsReader, purpose string) (Transport, error) {
	provider, _ := s.GetSetting(SettingKeyProvider)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderResend:
		return loadResend(s, purpose)
	case "", ProviderSMTP:
		cfg, err := LoadConfig(s, purpose)
		if err != nil {
			return nil, err
		}
		return smtpTransport{cfg: cfg}, nil
	default:
		return nil, errors.New("unknown mail provider " + provider)
	}
}

type smtpTransport struct{ cfg *SMTPConfig }

func (t smtpTransport) Send(msg Message) error { return Send(t.cfg, msg) }

func (t smtpTransport) Describe() string {
	if t.cfg == nil {
		return "SMTP (unconfigured)"
	}
	return "SMTP " + t.cfg.Host
}

// SenderIdentity is fromIdentity for callers outside this package: the address
// and NAME a given purpose sends from. The name doubles as the platform's name
// in a rendered template, which is deliberate - it is already configured, it is
// already visible in the panel, and a separate "site name" setting beside it
// would be two places to change one thing.
func SenderIdentity(s SettingsReader, purpose string) (email, name string) {
	return fromIdentity(s, purpose)
}

// fromIdentity resolves the sender shared by every transport.
//
// It reads the SMTP profile keys, which is a historical name rather than a
// statement about the protocol: those keys already hold the operator's address
// on every existing install, and inventing `mail.from_email` beside them would
// mean two places to set one thing and a migration to keep them in step.
func fromIdentity(s SettingsReader, purpose string) (email, name string) {
	get := func(field string) string {
		if purpose != "" && purpose != "default" {
			if v, _ := s.GetSetting("smtp." + purpose + "." + field); v != "" {
				return v
			}
		}
		v, _ := s.GetSetting("smtp.default." + field)
		return v
	}
	return strings.TrimSpace(get("from_email")), strings.TrimSpace(get("from_name"))
}

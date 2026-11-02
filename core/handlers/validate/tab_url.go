package validate

import (
	"errors"
	"net/url"
)

// ValidateTabURL ensures the URL parses + uses http/https. We don't pin
// hosts (unlike Modrinth) since custom tabs are the user's own infra and
// they're responsible for what they wire up.
func ValidateTabURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url must be http or https")
	}
	return nil
}

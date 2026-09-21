package handlers

import (
	"golang.org/x/crypto/bcrypt"
)

// The admin account-management actions re-authenticate, so every test that
// drives one to its WRITE has to prove who is asking. One password and one
// hash for the whole package, at MinCost because these run on every build.
//
// A fixture that wants to exercise the refusal simply leaves the block out.
const testReauthPassword = "correct-horse-battery-staple"

var testReauthHash = mustHash(testReauthPassword)

func mustHash(pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}

// withReauth adds the acting administrator's credential to a request body.
func withReauth(body map[string]interface{}) map[string]interface{} {
	if body == nil {
		body = map[string]interface{}{}
	}
	body["reauth"] = map[string]string{"password": testReauthPassword}
	return body
}

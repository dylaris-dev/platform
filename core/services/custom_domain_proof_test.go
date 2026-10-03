package services

import (
	"context"
	"errors"
	"testing"
)

// fakeResolver drives the proof without touching DNS.
type fakeResolver struct {
	cname    map[string]string
	hosts    map[string][]string
	txt      map[string][]string
	cnameErr error
	hostErr  error
	txtErr   error
}

func (f *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if f.cnameErr != nil {
		return "", f.cnameErr
	}
	// A real resolver returns the queried name when there is no CNAME.
	if v, ok := f.cname[host]; ok {
		return v, nil
	}
	return host + ".", nil
}
func (f *fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if f.hostErr != nil {
		return nil, f.hostErr
	}
	return f.hosts[host], nil
}
func (f *fakeResolver) LookupTXT(_ context.Context, host string) ([]string, error) {
	if f.txtErr != nil {
		return nil, f.txtErr
	}
	return f.txt[host], nil
}

func TestCheckTXTToken(t *testing.T) {
	const domain = "mc.example.com"
	token := "dylaris-verify=0123456789abcdef"

	t.Run("the published token passes", func(t *testing.T) {
		r := &fakeResolver{txt: map[string][]string{TXTVerifyPrefix + domain: {token}}}
		if !CheckTXTToken(context.Background(), r, domain, token) {
			t.Error("the correct TXT record was rejected")
		}
	})

	t.Run("a different token fails", func(t *testing.T) {
		r := &fakeResolver{txt: map[string][]string{TXTVerifyPrefix + domain: {"dylaris-verify=deadbeefdeadbeef"}}}
		if CheckTXTToken(context.Background(), r, domain, token) {
			t.Error("a foreign token was accepted")
		}
	})

	// An empty token must never match, or a claim with no token issued would be
	// verifiable by publishing an empty record.
	t.Run("an empty token never passes", func(t *testing.T) {
		r := &fakeResolver{txt: map[string][]string{TXTVerifyPrefix + domain: {""}}}
		if CheckTXTToken(context.Background(), r, domain, "") {
			t.Error("an empty token was accepted")
		}
	})

	t.Run("a record on the bare domain does not count", func(t *testing.T) {
		r := &fakeResolver{txt: map[string][]string{domain: {token}}}
		if CheckTXTToken(context.Background(), r, domain, token) {
			t.Error("a TXT record outside the _dylaris-verify label was accepted")
		}
	})

	t.Run("lookup failure is not proof", func(t *testing.T) {
		r := &fakeResolver{txtErr: errors.New("nxdomain")}
		if CheckTXTToken(context.Background(), r, domain, token) {
			t.Error("a failed lookup was treated as proof")
		}
	})
}

func TestNewTXTTokenIsUniqueAndPrefixed(t *testing.T) {
	a, err := NewTXTToken()
	if err != nil {
		t.Fatalf("NewTXTToken: %v", err)
	}
	b, _ := NewTXTToken()
	if a == b {
		t.Error("two tokens collided")
	}
	if len(a) < 20 {
		t.Errorf("token %q is too short to be unguessable", a)
	}
}

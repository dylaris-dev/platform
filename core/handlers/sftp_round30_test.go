package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypto/rand"
	"crypto/rsa"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"golang.org/x/crypto/ssh"
)

type suspendedUploadStore struct{ quotaHTTPFakeStore }

func (suspendedUploadStore) GetUserBilling(string) (*store.UserBilling, error) {
	return &store.UserBilling{Status: "suspended"}, nil
}

// Beam refused a suspended tenant a ticket; the panel's upload took their file.
// An operator's key carries no override (round 26), so it is refused too.
func TestAnUploadToASuspendedTenantsServerIsRefused(t *testing.T) {
	rdb, _ := newQuotaHTTPRedis(t)
	st := suspendedUploadStore{quotaHTTPFakeStore{newCoreStorageHTTPFakeStore()}}
	h := &FileHandler{state: &AppState{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}}
	req := newUploadRequest(t, "s1", 10)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1}}))
	rw := httptest.NewRecorder()
	h.UploadFileHandler(rw, req)
	if rw.Code != http.StatusForbidden || !strings.Contains(rw.Body.String(), "suspended") {
		t.Fatalf("status %d (%s), want the suspension refusal", rw.Code, rw.Body.String())
	}
}

func TestOnlyStrongSSHKeysAreTaken(t *testing.T) {
	const ed = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl laptop"
	line, fp, err := normalizeSSHPublicKey(ed)
	if err != nil || strings.Contains(line, "laptop") || !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("ed25519: %q %q %v", line, fp, err)
	}
	for name, bad := range map[string]string{
		"private key":  "-----BEGIN OPENSSH PRIVATE KEY-----",
		"two lines":    ed + "\n" + ed,
		"with options": `command="x" ` + ed,
		"empty":        "",
	} {
		if _, _, err := normalizeSSHPublicKey(bad); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if _, _, err := normalizeSSHPublicKey(string(ssh.MarshalAuthorizedKey(rsaPub(t, 2048)))); err == nil {
		t.Error("a 2048-bit RSA key was taken")
	}
}

func rsaPub(t *testing.T, bits int) ssh.PublicKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

type sshKeyAddStore struct {
	store.Store
	added int
}

func (f *sshKeyAddStore) GetUserByID(string) (*models.User, error) {
	return &models.User{ID: "u1", Password: "$2a$10$not-the-password-hash-at-all-xxxxxxxxxxxxxxxxxxxxxxxx"}, nil
}
func (f *sshKeyAddStore) AddSSHKey(*models.SSHKey, int) (bool, error) { f.added++; return true, nil }

// A key opens the account's files without the second factor, so adding one
// asks for the password (and code) again, like minting an API key.
func TestAddingAnSSHKeyNeedsThePasswordAgain(t *testing.T) {
	fs := &sshKeyAddStore{}
	h := NewSSHKeysHandler(&AppState{Store: fs})
	body := `{"name":"laptop","publicKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl","password":"wrong"}`
	req := httptest.NewRequest(http.MethodPost, "/api/me/ssh-keys", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), "userID", "u1"))
	rw := httptest.NewRecorder()
	h.Create(rw, req)
	if rw.Code != http.StatusUnauthorized || fs.added != 0 {
		t.Fatalf("status %d, keys added %d; want 401 and none", rw.Code, fs.added)
	}
}

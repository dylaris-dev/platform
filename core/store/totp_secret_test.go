package store

import (
	"database/sql/driver"
	"regexp"
	"strings"
	"testing"
	"time"

	"dylaris-core/pkg/crypto"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/pquerna/otp/totp"
)

func sealTOTP(t *testing.T, clusterSecret, secret string) string {
	t.Helper()
	ct, err := crypto.Encrypt(crypto.DeriveKey(clusterSecret, totpSecretPurpose), []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return totpEncMarker + ct
}

// A sealed secret opens under its own key; a plaintext one from before
// encryption reads through; anything that cannot be opened reads as "", which
// validates no code.
func TestDecodeTOTPSecret(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	sealed := sealTOTP(t, "cluster-a", secret)

	keyed := &PostgresStore{}
	keyed.SetTOTPEncryptionKey("cluster-a")
	other := &PostgresStore{}
	other.SetTOTPEncryptionKey("cluster-b")
	none := &PostgresStore{}

	for _, c := range []struct {
		name  string
		s     *PostgresStore
		value string
		want  string
	}{
		{"sealed, right key", keyed, sealed, secret},
		{"legacy plaintext", keyed, secret, secret},
		{"empty", keyed, "", ""},
		{"sealed, other key", other, sealed, ""},
		{"sealed, no key", none, sealed, ""},
		{"marker, garbage", keyed, totpEncMarker + "zz", ""},
	} {
		if got := c.s.decodeTOTPSecret("u1", c.value); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if strings.Contains(sealed, secret) {
		t.Fatal("the sealed value carries the secret in the clear")
	}
}

// What reaches the column is sealed: the secret never lands in the clear, and
// the wizard's verified secret turns 2FA on.
func TestTOTPSecretWritesAreSealed(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	sealedArg := sqlmock.Argument(totpSealedArg{t: t, plain: secret})

	t.Run("SetUserTOTP", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		s := NewPostgresStore(db)
		s.SetTOTPEncryptionKey("cluster-a")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET totp_secret = $1`)).
			WithArgs(sealedArg, "[]", true, "u1").WillReturnResult(sqlmock.NewResult(0, 1))
		if err := s.SetUserTOTP("u1", secret, "[]", true); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("CreateFirstAdmin", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		s := NewPostgresStore(db)
		s.SetTOTPEncryptionKey("cluster-a")
		stored := sealTOTP(t, "cluster-a", secret)
		mock.ExpectQuery(regexp.QuoteMeta(createFirstAdminQ)).
			WithArgs("alice", "hash", sealedArg, true).
			WillReturnRows(sqlmock.NewRows([]string{"id", "username", "is_admin", "role", "totp_secret", "is_2fa_enabled", "created_at"}).
				AddRow("uuid-1", "alice", true, "admin", stored, true, time.Now()))
		u, err := s.CreateFirstAdmin("alice", "hash", secret)
		if err != nil {
			t.Fatal(err)
		}
		if u.TOTPSecret != secret || !u.Is2FAEnabled {
			t.Errorf("returned user: secret %q, 2FA %v", u.TOTPSecret, u.Is2FAEnabled)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// totpSealedArg matches a column value that is sealed and does not carry the
// plaintext.
type totpSealedArg struct {
	t     *testing.T
	plain string
}

func (a totpSealedArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, totpEncMarker) && !strings.Contains(s, a.plain)
}

// What an older Core does with a sealed secret: no code validates. That is why
// sealing ships one release after the code that opens it.
func TestASealedSecretValidatesNoCode(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !totp.Validate(code, secret) {
		t.Fatal("the fixture code does not validate against its own secret")
	}
	if totp.Validate(code, sealTOTP(t, "cluster-a", secret)) {
		t.Error("a sealed secret validated a code")
	}
}

package handlers

import (
	"encoding/json"
	"errors"

	"dylaris-core/models"
)

// backupStorageSecretField is the one credential field inside an s3 backup
// storage config. Only s3 configs carry a secret; every other provider's config
// is non-sensitive.
const backupStorageSecretField = "secretAccessKey"

// redactBackupStorageSecret returns a copy of the storage with its s3 secret
// removed from Config and SecretSet reflecting whether one was stored. Used on
// every read path so the secret never leaves Core. A non-s3 provider, or an
// unparseable config, is returned unchanged with SecretSet false.
func redactBackupStorageSecret(bs models.BackupStorage) models.BackupStorage {
	if bs.Provider != "s3" || len(bs.Config) == 0 {
		return bs
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(bs.Config, &m); err != nil {
		// An unreadable config cannot be safely redacted field-by-field, so
		// drop the whole blob rather than risk leaking part of it.
		bs.Config = json.RawMessage("{}")
		return bs
	}
	if raw, ok := m[backupStorageSecretField]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			bs.SecretSet = true
		}
		delete(m, backupStorageSecretField)
	}
	if out, err := json.Marshal(m); err == nil {
		bs.Config = out
	} else {
		bs.Config = json.RawMessage("{}")
	}
	return bs
}

// validateBackupStorageEndpoint runs the same fail-closed S3-endpoint check
// core storage, modpacks and storage connections already apply: refuse an
// endpoint carrying credentials in its userinfo (https://AKIA...:secret@host)
// and require a parseable URL.
//
// backup_storages was the last of the four s3 config surfaces that skipped it,
// which is the same gap storage connections had before validateStorageConnectionEndpoint
// closed it. The row's Config is marshalled verbatim into the command an
// OPERATOR node receives, so an endpoint written that way carries a credential
// into a place nobody expects one, and it survives every redaction path here
// (redactBackupStorageSecret only knows the secretAccessKey field).
//
// A non-s3 provider has no endpoint to check, and an unparseable config is left
// to the provider factory to reject with a better message.
func validateBackupStorageEndpoint(bs models.BackupStorage) error {
	if bs.Provider != "s3" || len(bs.Config) == 0 {
		return nil
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(bs.Config, &m); err != nil {
		return nil
	}
	return validateS3Endpoint("backup storage", jsonString(m["endpoint"]))
}

// backupStorageEndpoint reads the s3 endpoint out of the config blob.
//
// Only for the reachability step of a test, so an unparseable config or a
// non-s3 provider is not an error here: it means there is nothing to dial and
// the probe itself gets to produce the message.
func backupStorageEndpoint(bs models.BackupStorage) string {
	if bs.Provider != "s3" || len(bs.Config) == 0 {
		return ""
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(bs.Config, &m); err != nil {
		return ""
	}
	return jsonString(m["endpoint"])
}

// backupStorageIdentityFields are the s3 config fields that decide WHERE a
// stored secret gets used. They mirror the trio mergeCoreStorageCandidate
// compares (S3Endpoint / S3Bucket / S3AccessKey); the names differ only because
// this config blob is backup.S3Config's JSON shape.
var backupStorageIdentityFields = []string{"endpoint", "bucket", "accessKeyId"}

// ErrBackupStorageSecretRequired is returned when an edit changes where the
// stored s3 secret would be used without supplying a new one. The HTTP layer
// answers 400.
var ErrBackupStorageSecretRequired = errors.New(
	"the s3 endpoint, bucket or access key changed, so the stored secret cannot be reused - re-enter the secret access key with this change")

// mergeBackupStorageSecret backfills the s3 secret from the stored config when
// the incoming config omits it or sends it blank. This is what lets the panel
// present the secret as write-only ("leave blank to keep") without an edit
// wiping it: the form never receives the secret, so a save carries none, and
// the existing one is preserved unless a new value is supplied.
//
// The backfill happens ONLY when the identity-defining fields (endpoint, bucket,
// accessKeyId) are all unchanged - the same condition mergeCoreStorageCandidate
// has applied to the Core file storage secret all along, for the same two
// reasons, which apply here word for word:
//
//   - Security. settings.write is a delegatable panel capability, and
//     ListStorages redacts the secret on every read, so a holder who cannot READ
//     the secret could point it at an endpoint and bucket of their choosing
//     merely by submitting those fields with the secret left blank. SigV4 signs
//     with the secret rather than sending it, so this is not a plaintext leak -
//     it is credential rebinding: an attacker-chosen host receives validly signed
//     requests carrying the operator's backups.
//   - Usability. Changing only the access key while leaving the secret blank
//     (because the form never shows it) would otherwise persist a NEW access key
//     paired with the OLD secret. Every backup to that target then fails a
//     signature check, at the node, hours later, with nothing pointing at the
//     edit that caused it.
//
// It REFUSES rather than saving a secret-less row, which is where this differs
// from the Core-storage sibling: there, validateCoreStorageConfig runs after the
// merge and rejects the empty secret for us. Nothing validates a backup_storages
// row before UpdateBackupStorage writes it, so silently dropping the secret here
// would wipe a working credential and answer 200.
//
// A non-blank incoming secret is used as-is (a genuine rotation). A non-s3
// provider, no existing row, or an existing row that holds no secret to rebind,
// are all returned unchanged.
func mergeBackupStorageSecret(incoming models.BackupStorage, existing *models.BackupStorage) (models.BackupStorage, error) {
	if incoming.Provider != "s3" || existing == nil {
		return incoming, nil
	}
	in := map[string]json.RawMessage{}
	if len(incoming.Config) > 0 {
		if err := json.Unmarshal(incoming.Config, &in); err != nil {
			return incoming, nil
		}
	}
	if s := jsonString(in[backupStorageSecretField]); s != "" {
		return incoming, nil // a new secret was submitted; keep it
	}
	ex := map[string]json.RawMessage{}
	if err := json.Unmarshal(existing.Config, &ex); err != nil {
		return incoming, nil
	}
	raw, ok := ex[backupStorageSecretField]
	if !ok || jsonString(raw) == "" {
		// Nothing stored to rebind or to wipe, so there is nothing to guard and
		// refusing would only block an unrelated edit.
		return incoming, nil
	}
	for _, f := range backupStorageIdentityFields {
		if jsonString(in[f]) != jsonString(ex[f]) {
			return incoming, ErrBackupStorageSecretRequired
		}
	}
	in[backupStorageSecretField] = raw
	if out, err := json.Marshal(in); err == nil {
		incoming.Config = out
	}
	return incoming, nil
}

// jsonString decodes a JSON value into a string, returning "" on anything that
// is not a non-empty string.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

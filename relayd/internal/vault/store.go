package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/store"
)

// Options configures the vault.
type Options struct {
	// DBPath is the vault database. It is a different file from the main
	// database and shares nothing with it.
	DBPath string
	// KeyPath is where the file backend's encryption key goes when there is no
	// keychain. Defaults to a "vault.key" beside DBPath.
	KeyPath string
	// Service is the keychain service name. Defaults to "relay".
	Service string
	// Keyring defaults to the OS keychain. Tests inject a MemoryKeyring, and a
	// MemoryKeyring with FailAll reproduces a machine that has none.
	Keyring Keyring
	// Backend forces a backend. Empty picks the keychain when one works.
	Backend Backend
	// Validator makes MEMORY.md §6's one real call before a proposal becomes a
	// credential. Nil is a supported state and a visible one: an accepted
	// credential is then stored with no validation timestamp at all, so the
	// console shows "never validated" rather than implying a probe that never
	// happened.
	Validator Validator
	// Clock defaults to time.Now.
	Clock func() time.Time
}

type vault struct {
	db     *store.DB
	opts   Options
	kr     Keyring
	status Status
	aead   cipher.AEAD
	// key is the raw AEAD key, kept for [vault.fingerprint]. It never leaves
	// this process and nothing derived from it identifies a secret off this
	// machine, which is why the proposal queue can key on a credential's
	// content without writing that content down.
	key       []byte
	validator Validator
	clock     func() time.Time
}

var _ Vault = (*vault)(nil)

// Open opens the vault, choosing a backend and degrading visibly when the OS
// keychain is unavailable.
//
// The order is: probe the keychain with a canary; if it works, secrets go
// straight into it. If it does not, fall back to AES-256-GCM ciphertext in the
// vault database with the key in a 0600 file — and record why, so the console
// says "no keychain on this machine" instead of implying one.
func Open(ctx context.Context, opts Options) (Vault, error) {
	if opts.DBPath == "" {
		return nil, errors.New("vault: no database path")
	}
	if opts.Service == "" {
		opts.Service = "relay"
	}
	if opts.KeyPath == "" {
		opts.KeyPath = filepath.Join(filepath.Dir(opts.DBPath), "vault.key")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	kr := opts.Keyring
	if kr == nil {
		kr = OSKeyring{}
	}

	db, err := store.OpenVault(opts.DBPath)
	if err != nil {
		return nil, err
	}

	v := &vault{db: db, opts: opts, kr: kr, validator: opts.Validator, clock: opts.Clock}

	keychainErr := probeKeyring(kr, opts.Service)
	backend := opts.Backend
	if backend == "" {
		if keychainErr == nil {
			backend = BackendKeychain
		} else {
			backend = BackendFile
		}
	}
	v.status = Status{Backend: backend}
	if keychainErr != nil {
		v.status.Reason = keychainErr.Error()
	}

	// The encryption key is loaded whichever backend won, because the proposal
	// queue seals candidates into this database in both cases. A proposal is
	// not yet a credential — nobody has confirmed it is even the user's — so it
	// does not get a keychain entry of its own; it gets ciphertext in a file
	// that is deleted the moment the proposal is decided. The credential
	// backend is unchanged: with a keychain, secrets still go into the keychain.
	key, source, err := v.loadOrCreateKey(keychainErr == nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	v.aead = aead
	v.key = key
	v.status.KeySource = source
	v.status.Degraded = keychainErr != nil

	return v, nil
}

const keyName = "vault-key"

// loadOrCreateKey returns the 32-byte file-backend key, from the keychain when
// one is usable and from a 0600 file otherwise.
func (v *vault) loadOrCreateKey(keychainOK bool) ([]byte, KeySource, error) {
	if keychainOK {
		if hexKey, err := v.kr.Get(v.opts.Service, keyName); err == nil && hexKey != "" {
			key, err := hex.DecodeString(hexKey)
			if err == nil && len(key) == 32 {
				return key, KeySourceKeychain, nil
			}
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, KeySourceNone, err
		}
		if err := v.kr.Set(v.opts.Service, keyName, hex.EncodeToString(key)); err == nil {
			return key, KeySourceKeychain, nil
		}
		// The canary passed but the real write did not; fall through to the file.
	}

	if b, err := os.ReadFile(v.opts.KeyPath); err == nil {
		key, err := hex.DecodeString(string(b))
		if err != nil || len(key) != 32 {
			return nil, KeySourceNone, fmt.Errorf("%w: %s is not a 32-byte key", ErrLocked, v.opts.KeyPath)
		}
		return key, KeySourceFile, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, KeySourceNone, fmt.Errorf("%w: %v", ErrLocked, err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, KeySourceNone, err
	}
	if err := os.MkdirAll(filepath.Dir(v.opts.KeyPath), 0o700); err != nil {
		return nil, KeySourceNone, err
	}
	if err := os.WriteFile(v.opts.KeyPath, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, KeySourceNone, err
	}
	return key, KeySourceFile, nil
}

func (v *vault) Status() Status { return v.status }
func (v *vault) Close() error   { return v.db.Close() }

func (v *vault) Put(ctx context.Context, in Input) (Entry, error) {
	if in.Service == "" {
		return Entry{}, errors.New("vault: a credential needs a service")
	}
	if in.Secret == "" {
		return Entry{}, errors.New("vault: refusing to store an empty secret")
	}
	if in.Source.Kind == "" {
		return Entry{}, errors.New("vault: a credential needs provenance; MEMORY.md §6 keeps which session and what date")
	}
	id := in.ID
	if id == "" {
		id = uuid.NewString()
	}
	now := v.clock()

	var ciphertext, nonce []byte
	switch v.status.Backend {
	case BackendKeychain:
		if err := v.kr.Set(v.opts.Service, id, in.Secret); err != nil {
			return Entry{}, fmt.Errorf("%w: %v", ErrLocked, err)
		}
	case BackendFile:
		var err error
		ciphertext, nonce, err = v.seal(in.Secret)
		if err != nil {
			return Entry{}, err
		}
	default:
		return Entry{}, fmt.Errorf("vault: unknown backend %q", v.status.Backend)
	}

	_, err := v.db.SQL().ExecContext(ctx, `
		INSERT INTO credential (id, service, label, last_four, backend, ciphertext, nonce,
			ref_kind, ref_value, source_kind, source_runtime, source_session, source_path,
			source_at, shared_session, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'managed', '', ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			service = excluded.service, label = excluded.label,
			last_four = excluded.last_four, backend = excluded.backend,
			ciphertext = excluded.ciphertext, nonce = excluded.nonce,
			source_kind = excluded.source_kind, source_runtime = excluded.source_runtime,
			source_session = excluded.source_session, source_path = excluded.source_path,
			source_at = excluded.source_at, shared_session = excluded.shared_session,
			revoked_at = NULL`,
		id, in.Service, in.Label, LastFour(in.Secret), string(v.status.Backend),
		ciphertext, nonce,
		string(in.Source.Kind), in.Source.Runtime, in.Source.Session, in.Source.Path,
		nullTime(in.Source.At), boolInt(in.Source.SharedSession), now.UnixMilli())
	if err != nil {
		return Entry{}, err
	}

	// Remember that this exact secret is held, so the proposal queue does not
	// offer it again the next forty times it turns up in a transcript. The
	// fingerprint is an HMAC under this machine's own vault key: it identifies
	// a secret here and is meaningless anywhere else.
	if fp := v.fingerprint(in.Service, in.Secret); fp != "" {
		if err := v.db.SetMeta(ctx, fingerprintKey(fp), id); err != nil {
			return Entry{}, err
		}
	}

	return v.Get(ctx, id)
}

// Current is MEMORY.md §6's "newest validated wins".
//
// Two Stripe keys means one is probably rotated, and the vault should say which
// is which rather than guessing. So a credential that answered a real call wins
// over one that never has, whatever their dates; among validated ones the most
// recently validated wins; among unvalidated ones the newest wins. Revoked
// credentials never win, and the entry that comes back carries its own
// provenance so a caller can see why it was chosen.
func (v *vault) Current(ctx context.Context, service string) (Entry, error) {
	row := v.db.SQL().QueryRowContext(ctx, `
		SELECT `+entryCols+` FROM credential
		WHERE service = ? AND revoked_at IS NULL
		ORDER BY (last_validation_reason = 'ok') DESC,
		         last_validated_at DESC, created_at DESC
		LIMIT 1`, service)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: no live credential for %s", ErrNotFound, service)
	}
	return e, err
}

// fingerprint identifies a secret to this machine and to nowhere else.
//
// HMAC rather than a bare hash on purpose: a plain SHA-256 of a credential is a
// value an attacker with the database could test candidate keys against, and
// this table is meant to survive being read. Keyed on the vault's own AES key,
// which never leaves the process, the digest is unlinkable without it.
func (v *vault) fingerprint(service, secret string) string {
	if len(v.key) == 0 || secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(service))))
	mac.Write([]byte{0})
	mac.Write([]byte(secret))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func fingerprintKey(fp string) string { return "credential-fingerprint/" + fp }

func (v *vault) Reveal(ctx context.Context, id string) (string, error) {
	var backend string
	var ciphertext, nonce []byte
	var revoked *int64
	err := v.db.SQL().QueryRowContext(ctx,
		`SELECT backend, ciphertext, nonce, revoked_at FROM credential WHERE id = ?`, id).
		Scan(&backend, &ciphertext, &nonce, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return "", err
	}
	if revoked != nil {
		return "", fmt.Errorf("%w: %s", ErrRevoked, id)
	}

	switch Backend(backend) {
	case BackendKeychain:
		secret, err := v.kr.Get(v.opts.Service, id)
		if errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("%w: %s is in the database but not in the keychain", ErrNotFound, id)
		}
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrLocked, err)
		}
		return secret, nil
	case BackendFile:
		if v.aead == nil {
			return "", fmt.Errorf("%w: this vault was opened without the file key", ErrLocked)
		}
		return v.open(ciphertext, nonce)
	}
	return "", fmt.Errorf("vault: unknown backend %q on %s", backend, id)
}

func (v *vault) Get(ctx context.Context, id string) (Entry, error) {
	row := v.db.SQL().QueryRowContext(ctx, `SELECT `+entryCols+` FROM credential WHERE id = ?`, id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return e, err
}

func (v *vault) List(ctx context.Context) ([]Entry, error) {
	rows, err := v.db.SQL().QueryContext(ctx,
		`SELECT `+entryCols+` FROM credential ORDER BY service, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (v *vault) Touch(ctx context.Context, id, usedBy string) error {
	res, err := v.db.SQL().ExecContext(ctx,
		`UPDATE credential SET last_used_at = ?, last_used_by = ? WHERE id = ?`,
		v.clock().UnixMilli(), usedBy, id)
	return affected(res, err, id)
}

func (v *vault) RecordValidation(ctx context.Context, id, reason string, at time.Time) error {
	if at.IsZero() {
		at = v.clock()
	}
	res, err := v.db.SQL().ExecContext(ctx,
		`UPDATE credential SET last_validated_at = ?, last_validation_reason = ? WHERE id = ?`,
		at.UnixMilli(), reason, id)
	return affected(res, err, id)
}

// Revoke withdraws a credential and destroys the secret material. The row
// stays: the console has to be able to say a credential was revoked, and when.
//
// The fingerprint mapping stays too, so a revoked key that keeps turning up in
// old transcripts is not re-proposed every time backfill reaches another one.
// A dead key offered back as a fresh discovery is the noise that teaches people
// to dismiss the queue unread.
func (v *vault) Revoke(ctx context.Context, id string) error {
	e, err := v.Get(ctx, id)
	if err != nil {
		return err
	}
	if e.Backend == BackendKeychain {
		if err := v.kr.Delete(v.opts.Service, id); err != nil {
			return fmt.Errorf("%w: %v", ErrLocked, err)
		}
	}
	res, err := v.db.SQL().ExecContext(ctx,
		`UPDATE credential SET revoked_at = ?, ciphertext = NULL, nonce = NULL WHERE id = ?`,
		v.clock().UnixMilli(), id)
	return affected(res, err, id)
}

// ------------------------------------------------------------------ crypto --

func (v *vault) seal(secret string) (ciphertext, nonce []byte, err error) {
	if v.aead == nil {
		return nil, nil, fmt.Errorf("%w: no encryption key", ErrLocked)
	}
	nonce = make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return v.aead.Seal(nil, nonce, []byte(secret), nil), nonce, nil
}

func (v *vault) open(ciphertext, nonce []byte) (string, error) {
	if len(ciphertext) == 0 || len(nonce) == 0 {
		return "", fmt.Errorf("%w: no secret material stored", ErrNotFound)
	}
	plain, err := v.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return string(plain), nil
}

// -------------------------------------------------------------------- rows --

const entryCols = `id, service, label, last_four, backend, source_kind, source_runtime,
	source_session, source_path, source_at, shared_session, created_at,
	last_used_at, last_used_by, last_validated_at, last_validation_reason, revoked_at`

func scanEntry(sc interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	var backend, sourceKind string
	var sourceAt, lastUsed, lastValidated, revoked *int64
	var shared int64
	var created int64
	err := sc.Scan(&e.ID, &e.Service, &e.Label, &e.LastFour, &backend,
		&sourceKind, &e.Source.Runtime, &e.Source.Session, &e.Source.Path,
		&sourceAt, &shared, &created, &lastUsed, &e.LastUsedBy,
		&lastValidated, &e.LastValidationReason, &revoked)
	if err != nil {
		return Entry{}, err
	}
	e.Backend = Backend(backend)
	e.Source.Kind = SourceKind(sourceKind)
	e.Source.At = fromPtr(sourceAt)
	e.Source.SharedSession = shared != 0
	e.CreatedAt = time.UnixMilli(created).UTC()
	e.LastUsedAt = fromPtr(lastUsed)
	e.LastValidatedAt = fromPtr(lastValidated)
	e.RevokedAt = fromPtr(revoked)
	return e, nil
}

func affected(res sql.Result, err error, id string) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func fromPtr(v *int64) time.Time {
	if v == nil {
		return time.Time{}
	}
	return time.UnixMilli(*v).UTC()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

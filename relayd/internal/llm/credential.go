package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RefKind is how a credential is reached. ORCHESTRATOR.md §2 takes this from
// OpenClaw: credentials are stored as references rather than pasted inline,
// with a preflight before saving. A Relay box holds a speech key, two model
// keys and five agent logins at once, so it matters more here than there.
type RefKind string

const (
	// RefEnv reads an environment variable: "env:OPENROUTER_API_KEY".
	RefEnv RefKind = "env"
	// RefFile reads a file, trimmed: "file:~/.config/relay/openrouter.key".
	RefFile RefKind = "file"
	// RefExec runs a command and takes its stdout, trimmed:
	// "exec:op read op://Private/OpenRouter/credential". This is how a
	// password manager gets in without the secret ever landing on disk.
	RefExec RefKind = "exec"
	// RefVault reads Relay's own vault: "vault:<id>". Resolved through the
	// SecretLookup hook so this package does not import the vault.
	RefVault RefKind = "vault"
	// RefCodex borrows the Codex CLI's ChatGPT login: "codex:~/.codex". The
	// value is CODEX_HOME, spelled out rather than defaulted so a relocated
	// home survives being written to config.toml.
	//
	// This is the fifth kind, and it is the one that is not a string on disk:
	// it resolves to an access token minted at the moment of use, from the
	// refresh token the CLI left behind. See codex.go. ORCHESTRATOR.md §2
	// counts four kinds because when it was written a credential was always a
	// key; a subscription is not, and pointing env: at one is the question with
	// no correct answer.
	RefCodex RefKind = "codex"
	// RefInline is a literal secret. Supported because a custom provider in a
	// test needs one, and discouraged everywhere else.
	RefInline RefKind = "inline"
)

// CredentialRef points at a secret without being one.
type CredentialRef struct {
	Kind  RefKind
	Value string
}

// SecretLookup resolves a vault id to a secret.
type SecretLookup func(ctx context.Context, id string) (string, error)

// Resolution failures, mapped onto ORCHESTRATOR.md §2's reason codes by Probe.
var (
	// ErrMissingCredential means nothing was configured at all.
	ErrMissingCredential = errors.New("llm: no credential configured")
	// ErrUnresolvedRef means the reference exists but does not lead anywhere:
	// the env var is unset, the file is gone, the exec failed, the vault entry
	// was revoked.
	ErrUnresolvedRef = errors.New("llm: credential reference does not resolve")
)

// ParseRef reads "kind:value". A bare string with no recognised prefix is
// treated as an env var name, because that is what people actually type.
func ParseRef(s string) (CredentialRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return CredentialRef{}, ErrMissingCredential
	}
	kind, value, ok := strings.Cut(s, ":")
	if !ok {
		return CredentialRef{Kind: RefEnv, Value: s}, nil
	}
	value = strings.TrimSpace(value)
	switch RefKind(kind) {
	case RefEnv, RefFile, RefExec, RefVault, RefInline, RefCodex:
		if value == "" {
			return CredentialRef{}, fmt.Errorf("%w: %q has no value", ErrUnresolvedRef, s)
		}
		return CredentialRef{Kind: RefKind(kind), Value: value}, nil
	default:
		// Not a prefix we know — most likely an env var with a colon in it,
		// which is not a thing, so treat the whole string as one.
		return CredentialRef{Kind: RefEnv, Value: s}, nil
	}
}

func (r CredentialRef) String() string {
	if r.Kind == "" {
		return ""
	}
	if r.Kind == RefInline {
		// Never print an inline secret, not even into a log line.
		return "inline:****"
	}
	return string(r.Kind) + ":" + r.Value
}

// IsZero reports whether nothing was configured.
func (r CredentialRef) IsZero() bool { return r.Kind == "" || r.Value == "" }

// ExecTimeout caps how long an exec reference may take. A password manager
// that wants a fingerprint is fine; one that hangs is not.
const ExecTimeout = 20 * time.Second

// Resolve turns the reference into a secret. lookup may be nil unless the
// reference is a vault one.
func (r CredentialRef) Resolve(ctx context.Context, lookup SecretLookup) (string, error) {
	return r.ResolveWith(ctx, lookup, CodexOptions{})
}

// ResolveWith is Resolve with the Codex seams supplied.
//
// A "codex:" reference reads a filesystem and a Keychain, so resolving one
// through the process's own is exactly the mistake the rest of this package
// avoids: a test would read the developer's real ChatGPT login and pass on one
// machine only. Everything else here is already injectable; this makes the
// fifth kind match.
func (r CredentialRef) ResolveWith(ctx context.Context, lookup SecretLookup, codex CodexOptions) (string, error) {
	if r.IsZero() {
		return "", ErrMissingCredential
	}

	switch r.Kind {
	case RefInline:
		return r.Value, nil

	case RefEnv:
		v := os.Getenv(r.Value)
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("%w: $%s is unset or empty", ErrUnresolvedRef, r.Value)
		}
		return strings.TrimSpace(v), nil

	case RefFile:
		path, err := expandHome(r.Value)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrUnresolvedRef, r.Value, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrUnresolvedRef, r.Value, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("%w: %s is empty", ErrUnresolvedRef, r.Value)
		}
		return v, nil

	case RefExec:
		ctx, cancel := context.WithTimeout(ctx, ExecTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", r.Value)
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("%w: exec %q: %v", ErrUnresolvedRef, r.Value, err)
		}
		v := strings.TrimSpace(string(out))
		if v == "" {
			return "", fmt.Errorf("%w: exec %q produced nothing", ErrUnresolvedRef, r.Value)
		}
		return v, nil

	case RefCodex:
		codex.Home = r.Value
		codex.Lookup = lookup
		auth, err := ResolveCodex(ctx, codex)
		if err != nil {
			return "", err
		}
		if auth.Mode == "apikey" {
			// The home resolves, but to a key rather than the subscription this
			// reference was chosen for. Hand back the key: it is what the user
			// actually has, and the probe that follows will say whether the
			// configured endpoint accepts it.
			return auth.APIKey, nil
		}
		return auth.Access, nil

	case RefVault:
		if lookup == nil {
			return "", fmt.Errorf("%w: vault:%s, but no vault is wired up", ErrUnresolvedRef, r.Value)
		}
		v, err := lookup(ctx, r.Value)
		if err != nil {
			return "", fmt.Errorf("%w: vault:%s: %v", ErrUnresolvedRef, r.Value, err)
		}
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("%w: vault:%s is empty", ErrUnresolvedRef, r.Value)
		}
		return v, nil
	}

	return "", fmt.Errorf("%w: unknown reference kind %q", ErrUnresolvedRef, r.Kind)
}

func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

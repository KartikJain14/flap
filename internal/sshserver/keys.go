package sshserver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// checkPrivateFile refuses key material that other users can read, like
// OpenSSH does.
func checkPrivateFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible by other users (mode %v); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	return nil
}

// GenerateHostKey writes a new Ed25519 host key in OpenSSH format. It never
// overwrites an existing file.
func GenerateHostKey(path string) (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "flapd host key")
	if err != nil {
		return nil, err
	}
	if err := writeNewPrivateFile(path, pem.EncodeToMemory(block)); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// writeNewPrivateFile creates path with mode 0600, failing if it exists.
func writeNewPrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadHostKeys reads every configured host key, generating missing ones when
// allowed. Existing keys are never replaced, so clients keep seeing the same
// host identity across restarts and upgrades.
func LoadHostKeys(files []string, generate bool) (signers []ssh.Signer, generated []string, err error) {
	for _, path := range files {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			if !generate {
				return nil, nil, fmt.Errorf("host key %s does not exist; create it with: flapd keygen -out %s", path, path)
			}
			s, err := GenerateHostKey(path)
			if err != nil {
				return nil, nil, fmt.Errorf("generate host key %s: %w", path, err)
			}
			signers = append(signers, s)
			generated = append(generated, path)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read host key: %w", err)
		}
		if err := checkPrivateFile(path); err != nil {
			return nil, nil, fmt.Errorf("host key: %w", err)
		}
		s, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, nil, fmt.Errorf("parse host key %s: %w (encrypted keys are not supported)", path, err)
		}
		signers = append(signers, s)
	}
	if len(signers) == 0 {
		return nil, nil, errors.New("no host keys configured")
	}
	return signers, generated, nil
}

// LoadSecret reads the 32-byte identity secret, creating it on first use.
func LoadSecret(path string) (secret []byte, created bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, false, err
		}
		if err := writeNewPrivateFile(path, []byte(hex.EncodeToString(secret)+"\n")); err != nil {
			return nil, false, fmt.Errorf("create identity secret: %w", err)
		}
		return secret, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read identity secret: %w", err)
	}
	if err := checkPrivateFile(path); err != nil {
		return nil, false, fmt.Errorf("identity secret: %w", err)
	}
	secret, err = hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(secret) != 32 {
		return nil, false, fmt.Errorf("identity secret %s must contain 64 hex characters", path)
	}
	return secret, false, nil
}

// Allowlist is a set of authorised public keys.
type Allowlist struct{ keys map[string]struct{} }

// LoadAllowlist parses an OpenSSH authorized_keys file. Options such as
// command= are rejected because flapd never runs commands.
func LoadAllowlist(path string) (*Allowlist, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authorized keys: %w", err)
	}
	al := &Allowlist{keys: map[string]struct{}{}}
	for line := 1; len(bytes.TrimSpace(data)) > 0; line++ {
		var (
			key     ssh.PublicKey
			options []string
			rest    []byte
		)
		key, _, options, rest, err = ssh.ParseAuthorizedKey(data)
		if err != nil {
			// ParseAuthorizedKey skips blank lines and comments itself; an
			// error here means a malformed entry.
			if strings.Contains(err.Error(), "no key found") {
				break
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(options) > 0 {
			return nil, fmt.Errorf("%s: key options (%s) are not supported", path, strings.Join(options, ","))
		}
		if err := acceptableKey(key); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		al.keys[string(key.Marshal())] = struct{}{}
		data = rest
	}
	return al, nil
}

// Contains reports whether key is in the allowlist.
func (a *Allowlist) Contains(key ssh.PublicKey) bool {
	if a == nil {
		return false
	}
	_, ok := a.keys[string(key.Marshal())]
	return ok
}

// Len returns the number of keys.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.keys)
}

// acceptableKey rejects weak or unsupported client keys.
func acceptableKey(key ssh.PublicKey) error {
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
		return nil
	case ssh.KeyAlgoRSA:
		ck, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return errors.New("unsupported RSA key")
		}
		pk, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || pk.N.BitLen() < 2048 {
			return errors.New("RSA keys must be at least 2048 bits")
		}
		return nil
	}
	return fmt.Errorf("key type %s is not accepted", key.Type())
}

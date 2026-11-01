package imapauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/mrothroc/mailpipe/pkg/keychain"
)

var ErrCredential = errors.New("IMAP credential unavailable or incomplete; sign in again")

type Items interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
	Delete(context.Context, string) error
}
type Store interface {
	Session(context.Context, func(Items) error) error
}
type keychainStore struct{ k *keychain.Keychain }

func NewKeychain(k *keychain.Keychain) Store { return keychainStore{k} }
func (s keychainStore) Session(ctx context.Context, fn func(Items) error) error {
	return s.k.Exclusive(ctx, func(k *keychain.Keychain) error { return fn(k) })
}

func generation(v string) bool {
	if len(v) != 32 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func (p Principal) Encode(gen, password string) (string, error) {
	q, e := NewPrincipal(p.Provider, p.Endpoint, p.Login)
	if e != nil || q != p || !generation(gen) || !printable(password, 256) {
		return "", ErrCredential
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode([]string{"imap-credential-v1", p.tuple(), gen, password}) != nil {
		return "", ErrCredential
	}
	v := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	if len(v) > keychain.MaxUnchunkedBytes {
		return "", ErrCredential
	}
	return string(v), nil
}
func envelope(v string) ([]string, error) {
	if len(v) > keychain.MaxUnchunkedBytes {
		return nil, ErrCredential
	}
	var a []string
	d := json.NewDecoder(bytes.NewBufferString(v))
	if d.Decode(&a) != nil || len(a) != 4 {
		return nil, ErrCredential
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return nil, ErrCredential
	}
	return a, nil
}
func (p Principal) Decode(v, gen string) (string, error) {
	a, e := envelope(v)
	if e != nil || a[0] != "imap-credential-v1" || a[1] != p.tuple() || a[2] != gen || !generation(gen) || !printable(a[3], 256) {
		return "", ErrCredential
	}
	return a[3], nil
}

// Replaceable permits explicit verified-login repair of truncated bytes at this
// injective tuple key, but never overwrites a readable different tuple/version.
func (p Principal) Replaceable(v string) bool {
	// Preserve recognizable identity/version evidence even when a later write
	// left the remaining envelope incomplete.
	d := json.NewDecoder(bytes.NewBufferString(v))
	if token, err := d.Token(); err == nil && token == json.Delim('[') {
		var version string
		if d.Decode(&version) == nil {
			if version != "imap-credential-v1" {
				return false
			}
			var tuple string
			if d.Decode(&tuple) == nil && tuple != p.tuple() {
				return false
			}
		}
	}
	a, e := envelope(v)
	if e != nil {
		return true
	}
	return a[0] == "imap-credential-v1" && a[1] == p.tuple()
}

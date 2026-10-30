package mailbox

import (
	"errors"
	"strings"
	"time"
)

// Opaque identity types must not contain credentials. Only adapters interpret
// provider keys and locators; domain consumers compare them without parsing.
type (
	AccountID    string
	ProviderID   string
	MessageID    string
	CollectionID string
	Locator      string
)

// Account identifies a configured mailbox, independently of display addresses.
type Account struct {
	ID         AccountID
	ProviderID ProviderID
}

// Validate requires a stable configured account and provider identity.
func (a Account) Validate() error {
	if err := boundAccount("account", a.ID, a.ID); err != nil {
		return err
	}
	if strings.TrimSpace(string(a.ProviderID)) == "" {
		return errors.New("account: provider identity is required")
	}
	return nil
}

func boundAccount(op string, expected, actual AccountID) error {
	if strings.TrimSpace(string(expected)) == "" || strings.TrimSpace(string(actual)) == "" {
		return errors.New(op + ": account identity is required")
	}
	if actual != expected {
		return &AccountMismatchError{Operation: op, Expected: expected, Actual: actual}
	}
	return nil
}

// LocalRef is a stable stored number within one account. Adapters neither
// allocate nor renumber it; it is never a provider identifier.
type LocalRef struct {
	AccountID AccountID
	N         int64
}

// Validate rejects unbound or nonpositive local handles.
func (r LocalRef) Validate(account AccountID) error {
	if err := boundAccount("local reference", account, r.AccountID); err != nil {
		return err
	}
	if r.N <= 0 {
		return errors.New("local reference: positive message number is required")
	}
	return nil
}

// MessageRef separates provider identity from its current routing locator.
// MessageID is stable only within the granularity the adapter can prove. A
// mailbox-scoped occurrence must include its scope/epoch in the opaque identity.
// RFC Message-ID and content are never substitutes for this identity. Locators
// may rotate; stale locators must fail rather than target a different message.
type MessageRef struct {
	AccountID AccountID
	MessageID MessageID
	Locator   Locator
}

// Validate checks account binding and presence, without parsing opaque values.
func (r MessageRef) Validate(account AccountID) error {
	if err := boundAccount("message reference", account, r.AccountID); err != nil {
		return err
	}
	if strings.TrimSpace(string(r.MessageID)) == "" || strings.TrimSpace(string(r.Locator)) == "" {
		return errors.New("message reference: provider identity and locator are required")
	}
	return nil
}

// Binding associates a local handle with proven provider identity. Persistence
// and proof of continuity after an identity change belong to callers/adapters.
type Binding struct {
	Local  LocalRef
	Remote MessageRef
}

// Validate requires both halves to belong to the selected account.
func (b Binding) Validate(account AccountID) error {
	if err := b.Local.Validate(account); err != nil {
		return err
	}
	return b.Remote.Validate(account)
}

// CollectionRef identifies a collection without assuming a folder hierarchy.
type CollectionRef struct {
	AccountID    AccountID
	CollectionID CollectionID
}

// Validate checks account binding and opaque collection identity.
func (r CollectionRef) Validate(account AccountID) error {
	if err := boundAccount("collection reference", account, r.AccountID); err != nil {
		return err
	}
	if strings.TrimSpace(string(r.CollectionID)) == "" {
		return errors.New("collection reference: identity is required")
	}
	return nil
}

// Collection has a display name, not a unique path or semantic destination.
type Collection struct {
	Ref  CollectionRef
	Name string
}

// Membership associates the enclosing message with a collection. Multiple
// memberships do not create duplicate message identities or local handles.
type Membership struct{ Collection CollectionRef }

// Address is untrusted metadata. Name is never an identity precondition.
type Address struct{ Address, Name string }

// Metadata is complete message metadata, with absent headers left absent.
// Adapter sender fallback must be consistent between sync and inspection.
type Metadata struct {
	InternetMessageID      string
	Received               time.Time
	Sender                 Address
	Recipients             []Address
	Subject, Preview       string
	HasAttachments, IsRead bool
}

// Message is a sync upsert with complete metadata. Its membership in the sync
// collection is implicit; it says nothing about membership in other collections.
type Message struct {
	Ref      MessageRef
	Metadata Metadata
}

// State is the current authoritative message identity and membership set.
// Revision is an optional opaque provider precondition token, never a credential.
type State struct {
	Ref         MessageRef
	Metadata    Metadata
	Memberships []Membership
	Revision    string
}

// Validate accepts zero or multiple memberships but rejects duplicates and
// cross-account references. Missing mail headers do not invalidate opaque IDs.
func (s State) Validate(account AccountID) error {
	if err := s.Ref.Validate(account); err != nil {
		return err
	}
	seen := make(map[CollectionID]bool, len(s.Memberships))
	for _, m := range s.Memberships {
		if err := m.Collection.Validate(account); err != nil {
			return err
		}
		if seen[m.Collection.CollectionID] {
			return errors.New("message state: duplicate collection membership")
		}
		seen[m.Collection.CollectionID] = true
	}
	return nil
}

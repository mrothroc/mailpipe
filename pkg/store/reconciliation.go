package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

const (
	// LegacyPrimaryMailbox describes completed primary-mailbox observation, not
	// deletion or ownership proof and not archive/recoverable-store coverage.
	LegacyPrimaryMailbox = "graph-primary-mailbox-v1"
	LegacyUnavailable    = "not-observed-and-item-not-found"
)

// LegacyReconciliation is supplied by a trusted read-only adapter. Unavailable
// references have no invented remote State; the owner attributes their history.
type LegacyReconciliation struct {
	Verification LegacyVerification
	// Only verified originals with a reviewed metadata exception have a first
	// observation.
	EnumeratedStates map[int64]mailbox.State
	// Trusted adapter stamps for independently checked source-to-mapping pairs.
	// Only noncase sender disagreements may supply this capability.
	SenderSourceIntegrity map[int64]bool
	Unavailable           map[int64]string
	Coverage              string
	Started, Finished     time.Time
	Pages, Observed       int
}

type LegacyReconciliationVerifier interface {
	ReconcileLegacy(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error)
}

type LegacyReconciliationItem struct {
	RecipientNameChanged   bool                 `json:"recipient_name_changed"`
	SenderAddressDisagrees bool                 `json:"sender_address_disagrees"`
	SenderNameChanged      bool                 `json:"sender_name_changed"`
	SenderCasingChanged    bool                 `json:"sender_casing_changed"`
	RecipientCasingChanged bool                 `json:"recipient_casing_changed"`
	EnumeratedState        *mailbox.State       `json:"enumerated_state,omitempty"`
	OriginalFolder         string               `json:"original_folder,omitempty"`
	OriginalMemberships    []mailbox.Membership `json:"original_memberships,omitempty"`
	MembershipChanged      bool                 `json:"membership_changed"`
	N                      int64                `json:"n"`
	OriginalID             string               `json:"original_id"`
	Removed                bool                 `json:"original_removed"`
	Disposition            string               `json:"disposition"`
	Reason                 string               `json:"reason,omitempty"`
	State                  *mailbox.State       `json:"state,omitempty"`
}

// LegacyReconciliationReceipt records local attribution authority and scoped
// remote evidence. It never authorizes mailbox execution.
type LegacyReconciliationReceipt struct {
	Version                        int                              `json:"version"`
	Account                        mailbox.Account                  `json:"account"`
	SourceDigest                   string                           `json:"source_digest"`
	Digest                         string                           `json:"reconciliation_digest"`
	Coverage                       string                           `json:"coverage"`
	Started                        time.Time                        `json:"started"`
	Finished                       time.Time                        `json:"finished"`
	Pages                          int                              `json:"pages"`
	Observed                       int                              `json:"observed"`
	HistoricalAttributionConfirmed bool                             `json:"historical_attribution_confirmed"`
	Backup                         string                           `json:"backup,omitempty"`
	BackupDigest                   string                           `json:"backup_digest,omitempty"`
	Folders                        map[string]mailbox.CollectionRef `json:"folders"`
	Items                          []LegacyReconciliationItem       `json:"items"`
}

// ReviewLegacyReconciliation validates complete, disjoint coverage and derives a
// deterministic proposal digest. Preview does not require owner confirmation.
func ReviewLegacyReconciliation(a mailbox.Account, p LegacyPreparation, v LegacyReconciliation) (LegacyReconciliationReceipt, error) {
	v = ownReconciliation(v)
	r := LegacyReconciliationReceipt{}
	if e := a.Validate(); e != nil {
		return r, e
	}
	if v.Verification.Account != a {
		return r, &mailbox.AccountMismatchError{Operation: "legacy reconciliation", Expected: a.ID, Actual: v.Verification.Account.ID}
	}
	if a.ProviderID != GraphProvider || p.digest == "" || p.nonce == "" || v.Verification.Nonce != p.nonce {
		return r, errors.New("store: exact account and preparation challenge required")
	}
	if v.Coverage != LegacyPrimaryMailbox || v.Pages <= 0 || v.Observed < len(v.Verification.States) || v.Started.IsZero() || v.Finished.Before(v.Started) {
		return r, errors.New("store: complete primary-mailbox evidence required")
	}
	if len(v.Verification.States)+len(v.Unavailable) != len(p.Live) {
		return r, errors.New("store: complete disjoint reconciliation inventory required")
	}
	if len(v.Unavailable) > 0 && len(v.Verification.States) == 0 {
		return r, stale("insufficient same-account live evidence; preserve the mirror and restore access to an original immutable reference or ask the owner for an approved evidence policy")
	}
	r = LegacyReconciliationReceipt{Version: 7, Account: a, SourceDigest: p.digest, Coverage: v.Coverage, Started: v.Started, Finished: v.Finished, Pages: v.Pages, Observed: v.Observed, HistoricalAttributionConfirmed: v.Verification.HistoricalAttributionConfirmed, Folders: v.Verification.Folders}
	acceptedPairs := make(map[int64]bool, len(v.EnumeratedStates))
	acceptedIntegrity := make(map[int64]bool, len(v.SenderSourceIntegrity))
	for _, m := range p.Live {
		if m.GraphID == "" {
			return LegacyReconciliationReceipt{}, stale("historical immutable identity is missing")
		}
		scope, ok := v.Verification.Folders[m.Folder]
		if !ok || scope.Validate(a.ID) != nil {
			return LegacyReconciliationReceipt{}, stale("legacy collection mapping is missing")
		}
		state, verified := v.Verification.States[m.N]
		reason, unavailable := v.Unavailable[m.N]
		if verified == unavailable {
			return LegacyReconciliationReceipt{}, errors.New("store: overlapping or missing reconciliation disposition")
		}
		item := LegacyReconciliationItem{N: m.N, OriginalID: m.GraphID, OriginalFolder: m.Folder, OriginalMemberships: []mailbox.Membership{{Collection: scope}}}
		if unavailable {
			if reason != LegacyUnavailable {
				return LegacyReconciliationReceipt{}, errors.New("store: unavailable reference lacks complete scoped evidence")
			}
			item.Disposition = "unavailable"
			item.Reason = reason
		} else {
			var e error
			state, item.MembershipChanged, e = reconciliationState(a.ID, m, scope, state)
			if e != nil {
				return LegacyReconciliationReceipt{}, e
			}
			item.Disposition = "verified"
			item.State = &state
			if enumerated, ok := v.EnumeratedStates[m.N]; ok {
				if e = reconciliationMetadataPair(a.ID, enumerated, state, v.SenderSourceIntegrity[m.N]); e != nil {
					return LegacyReconciliationReceipt{}, e
				}
				enumerated = reconciliationSnapshot(enumerated)
				item.EnumeratedState = &enumerated
				item.SenderNameChanged = enumerated.Metadata.Sender.Name != state.Metadata.Sender.Name
				item.SenderAddressDisagrees = !strings.EqualFold(enumerated.Metadata.Sender.Address, state.Metadata.Sender.Address)
				item.SenderCasingChanged = !item.SenderAddressDisagrees && enumerated.Metadata.Sender.Address != state.Metadata.Sender.Address
				if item.SenderAddressDisagrees {
					acceptedIntegrity[m.N] = true
				}
				for i, before := range enumerated.Metadata.Recipients {
					after := state.Metadata.Recipients[i]
					item.RecipientNameChanged = item.RecipientNameChanged || before.Name != after.Name
					item.RecipientCasingChanged = item.RecipientCasingChanged || before.Address != after.Address
				}
				acceptedPairs[m.N] = true
			}
		}
		r.Items = append(r.Items, item)
	}
	for n := range v.EnumeratedStates {
		if !acceptedPairs[n] {
			return LegacyReconciliationReceipt{}, errors.New("store: metadata observation requires a verified original")
		}
	}
	for n, valid := range v.SenderSourceIntegrity {
		if !valid || !acceptedIntegrity[n] {
			return LegacyReconciliationReceipt{}, errors.New("store: source integrity requires a verified sender disagreement")
		}
	}
	for _, m := range p.Removed {
		r.Items = append(r.Items, LegacyReconciliationItem{N: m.N, OriginalID: m.GraphID, Removed: true, Disposition: "historical"})
	}
	// Items are sorted independently of exported preparation slice order.
	sort.Slice(r.Items, func(i, j int) bool { return r.Items[i].N < r.Items[j].N })
	raw, e := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []LegacyReconciliationItem
	}{"mailpipe-legacy-reconciliation-v7", a, p.digest, v.Coverage, r.Folders, r.Items})
	if e != nil {
		return LegacyReconciliationReceipt{}, e
	}
	h := sha256.Sum256(raw)
	r.Digest = hex.EncodeToString(h[:])
	return r, nil
}

// Independently validate the adapter's exception at the persistence boundary.
// Display names are reviewed evidence; recipient addresses remain indexed.
func reconciliationMetadataPair(account mailbox.AccountID, enumerated, inspected mailbox.State, integrity bool) error {
	invalid := func() error { return stale("reviewed metadata observations disagree") }
	if enumerated.Validate(account) != nil || inspected.Validate(account) != nil || len(enumerated.Memberships) != 1 || len(inspected.Memberships) != 1 {
		return invalid()
	}
	disagrees := !strings.EqualFold(enumerated.Metadata.Sender.Address, inspected.Metadata.Sender.Address)
	precondition := enumerated
	if disagrees {
		if !integrity || enumerated.Metadata.Sender.Address == "" || inspected.Metadata.Sender.Address == "" {
			return invalid()
		}
		precondition.Metadata.Sender.Address = inspected.Metadata.Sender.Address
	}
	if err := mailbox.CheckPrecondition(account, precondition, inspected); err != nil {
		return err
	}
	a, b := enumerated.Metadata, inspected.Metadata
	if a.Sender.Name != b.Sender.Name && (!utf8.ValidString(a.Sender.Name) || !utf8.ValidString(b.Sender.Name)) {
		return invalid()
	}
	if !utf8.ValidString(a.Sender.Address) || !utf8.ValidString(b.Sender.Address) {
		return invalid()
	}
	if reflect.DeepEqual(a, b) || (a.Recipients == nil) != (b.Recipients == nil) || len(a.Recipients) != len(b.Recipients) {
		return invalid()
	}
	// Retain position-only comparison even for indistinguishable duplicate
	// address slots. No sorting, cross-slot identity inference or input mutation.
	for i, left := range a.Recipients {
		right := b.Recipients[i]
		if left.Name != right.Name && (!utf8.ValidString(left.Name) || !utf8.ValidString(right.Name)) {
			return invalid()
		}
		if !utf8.ValidString(left.Address) || !utf8.ValidString(right.Address) || !strings.EqualFold(left.Address, right.Address) {
			return invalid()
		}
		left.Address, right.Address = "", ""
		left.Name, right.Name = "", ""
		if !reflect.DeepEqual(left, right) {
			return invalid()
		}
	}
	a.Sender.Address, b.Sender.Address = "", ""
	a.Sender.Name, b.Sender.Name = "", ""
	a.Recipients, b.Recipients = nil, nil
	if !reflect.DeepEqual(a, b) {
		return invalid()
	}
	return nil
}

func reconciliationSnapshot(s mailbox.State) mailbox.State {
	if s.Metadata.Recipients != nil {
		s.Metadata.Recipients = append([]mailbox.Address{}, s.Metadata.Recipients...)
	}
	if s.Memberships != nil {
		s.Memberships = append([]mailbox.Membership{}, s.Memberships...)
	}
	return s
}

func ownReconciliation(v LegacyReconciliation) LegacyReconciliation {
	states := func(in map[int64]mailbox.State) map[int64]mailbox.State {
		out := make(map[int64]mailbox.State, len(in))
		for n, s := range in {
			out[n] = reconciliationSnapshot(s)
		}
		return out
	}
	v.Verification.States = states(v.Verification.States)
	v.EnumeratedStates = states(v.EnumeratedStates)
	integrity := make(map[int64]bool, len(v.SenderSourceIntegrity))
	for n, valid := range v.SenderSourceIntegrity {
		integrity[n] = valid
	}
	v.SenderSourceIntegrity = integrity
	folders := make(map[string]mailbox.CollectionRef, len(v.Verification.Folders))
	for name, ref := range v.Verification.Folders {
		folders[name] = ref
	}
	v.Verification.Folders = folders
	unavailable := make(map[int64]string, len(v.Unavailable))
	for n, reason := range v.Unavailable {
		unavailable[n] = reason
	}
	v.Unavailable = unavailable
	return v
}

// reconciliationState separates the historical collection from independently
// observed current membership. Only reviewed reconciliation uses this comparison;
// strict binding and reviewed actions retain their full membership precondition.
func reconciliationState(account mailbox.AccountID, original Message, old mailbox.CollectionRef, current mailbox.State) (mailbox.State, bool, error) {
	if err := old.Validate(account); err != nil {
		return mailbox.State{}, false, err
	}
	if err := current.Validate(account); err != nil {
		return mailbox.State{}, false, err
	}
	if len(current.Memberships) != 1 {
		return mailbox.State{}, false, stale("complete current Graph collection required")
	}
	expected := mailbox.State{Ref: mailbox.MessageRef{AccountID: account, MessageID: mailbox.MessageID(original.GraphID), Locator: mailbox.Locator(original.GraphID)}, Metadata: mailbox.Metadata{InternetMessageID: original.InternetMessageID, Sender: mailbox.Address{Address: original.Sender}}, Memberships: current.Memberships}
	if err := mailbox.CheckPrecondition(account, expected, current); err != nil {
		return mailbox.State{}, false, err
	}
	return canonicalState(reconciliationSnapshot(current)), current.Memberships[0].Collection != old, nil
}

type legacyReconciliationRequest struct {
	verifier     LegacyReconciliationVerifier
	confirmation string
	receipt      LegacyReconciliationReceipt
	evidence     LegacyReconciliation
}

// ReconcileLegacy binds only a freshly regenerated, exactly reviewed proposal.
func ReconcileLegacy(ctx context.Context, path string, a mailbox.Account, p LegacyPreparation, confirmation string, v LegacyReconciliationVerifier) (*AccountStore, error) {
	if v == nil || confirmation == "" {
		return nil, errors.New("store: reviewed reconciliation required")
	}
	return bindLegacy(ctx, path, a, p, nil, &legacyReconciliationRequest{verifier: v, confirmation: confirmation})
}

func writeReconciliationReceipt(ctx context.Context, q sqlAccess, r LegacyReconciliationReceipt) error {
	items := r.Items
	r.Items = nil
	raw, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if _, e = q.ExecContext(ctx, "insert into legacy_reconciliations(singleton,account_id,provider_id,source_digest,reconciliation_digest,receipt) values(1,?,?,?,?,?)", r.Account.ID, r.Account.ProviderID, r.SourceDigest, r.Digest, string(raw)); e != nil {
		return e
	}
	for _, i := range items {
		raw, e = json.Marshal(i)
		if e != nil {
			return e
		}
		if _, e = q.ExecContext(ctx, "insert into legacy_reconciliation_items(n,account_id,provider_id,item) values(?,?,?,?)", i.N, r.Account.ID, r.Account.ProviderID, string(raw)); e != nil {
			return e
		}
	}
	return nil
}

// LegacyReconciliationReceipt returns the committed receipt without remote I/O.
func (a *AccountStore) LegacyReconciliationReceipt(ctx context.Context) (out LegacyReconciliationReceipt, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		var raw string
		if e := q.QueryRowContext(ctx, "select receipt from legacy_reconciliations where account_id=? and provider_id=?", a.account.ID, a.account.ProviderID).Scan(&raw); e != nil {
			return e
		}
		if e := json.Unmarshal([]byte(raw), &out); e != nil {
			return e
		}
		rows, e := q.QueryContext(ctx, "select item from legacy_reconciliation_items where account_id=? and provider_id=? order by n", a.account.ID, a.account.ProviderID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			var i LegacyReconciliationItem
			if e = rows.Scan(&raw); e != nil {
				return e
			}
			if e = json.Unmarshal([]byte(raw), &i); e != nil {
				return e
			}
			out.Items = append(out.Items, i)
		}
		return rows.Err()
	})
	return
}

// ReadLegacyReconciliationReceipt reads an existing binding under an ordinary
// shared lifecycle lease. It never migrates, creates a store or first-binds it.
func ReadLegacyReconciliationReceipt(ctx context.Context, path string, a mailbox.Account) (out LegacyReconciliationReceipt, found bool, err error) {
	if e := a.Validate(); e != nil {
		return out, false, e
	}
	life, e := acquireLifecycle(ctx, path, false, diskFiles{})
	if e != nil {
		return out, false, e
	}
	db, e := sqliteOpen(ctx, life.entry.path)
	if e != nil {
		return out, false, errors.Join(e, life.release())
	}
	defer func() { err = errors.Join(err, closeMaintenance(life, db)) }()
	current, e := schemaCurrent(ctx, db)
	if e != nil {
		return out, false, e
	}
	if !current {
		return out, false, nil
	}
	selected, _, bound, e := readBinding(ctx, db)
	if e != nil || !bound {
		return out, false, e
	}
	if selected != a {
		return out, false, &mailbox.AccountMismatchError{Operation: "reconciliation receipt", Expected: a.ID, Actual: selected.ID}
	}
	// Reuse the transaction-validated account reader; this handle owns no extra
	// lifecycle or raw database descriptor and is closed by the outer scope.
	temporary := &AccountStore{store: &Store{db: db, life: life}, account: a}
	out, e = temporary.LegacyReconciliationReceipt(ctx)
	return out, e == nil, e
}

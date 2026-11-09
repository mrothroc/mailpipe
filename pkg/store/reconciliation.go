package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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

// Reference reconciliation has a distinct scope from legacy mailbox-list evidence.
const (
	LegacyReferenceMethod            = "graph-exact-original-inspections-v1"
	LegacyReferenceCoverage          = "graph-original-references-v1"
	LegacyReferenceUnavailable       = "exact-reference-item-not-found"
	LegacyReferenceFound             = "found"
	LegacyReferenceNotFound          = "item-not-found"
	LegacyReferenceLimit             = 1000000
	LegacyReferenceStateLimit  int64 = 256 << 20
	LegacyReferenceReadTimeout       = 30 * time.Minute
)

type LegacyReferenceContext struct {
	RequestedReferences int   `json:"requested_references"`
	CompletedReferences int   `json:"completed_references"`
	InspectionBatches   int   `json:"inspection_batches"`
	StateBytes          int64 `json:"state_bytes"`
}
type LegacyReferenceOutcome struct {
	Ref             mailbox.MessageRef
	Kind            string
	SourceValidated bool
}

// LegacyReconciliation is supplied by a trusted read-only adapter. Unavailable
// references have no invented remote State; the owner attributes their history.
type LegacyReconciliation struct {
	Method            string
	ReferenceContext  LegacyReferenceContext
	ReferenceOutcomes map[int64]LegacyReferenceOutcome
	Verification      LegacyVerification
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
	ReadRef                *mailbox.MessageRef  `json:"read_ref,omitempty"`
	ReadOutcome            string               `json:"read_outcome,omitempty"`
	SourceValidated        bool                 `json:"source_validated,omitempty"`
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
	Method                         string                           `json:"method,omitempty"`
	ReferenceContext               *LegacyReferenceContext          `json:"reference_context,omitempty"`
	Version                        int                              `json:"version"`
	Account                        mailbox.Account                  `json:"account"`
	SourceDigest                   string                           `json:"source_digest"`
	Digest                         string                           `json:"reconciliation_digest"`
	AuditDigest                    string                           `json:"audit_digest,omitempty"`
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
// stable approval and separate raw-observation audit digests. Preview does not
// require owner confirmation.
func ReviewLegacyReconciliation(a mailbox.Account, p LegacyPreparation, v LegacyReconciliation) (LegacyReconciliationReceipt, error) {
	v = ownReconciliation(v)
	zero := LegacyReconciliationReceipt{}
	invalid := func() (LegacyReconciliationReceipt, error) {
		return zero, stale("complete exact original-reference evidence required")
	}
	if e := a.Validate(); e != nil {
		return zero, e
	}
	if v.Verification.Account != a {
		return zero, &mailbox.AccountMismatchError{Operation: "legacy reconciliation", Expected: a.ID, Actual: v.Verification.Account.ID}
	}
	if a.ProviderID != GraphProvider || p.digest == "" || p.nonce == "" || v.Verification.Nonce != p.nonce {
		return invalid()
	}
	if v.Method != LegacyReferenceMethod || v.Coverage != LegacyReferenceCoverage || v.Pages != 0 || v.Observed != 0 || len(v.EnumeratedStates) != 0 || len(v.SenderSourceIntegrity) != 0 {
		return invalid()
	}
	c := v.ReferenceContext
	if len(p.Live) > LegacyReferenceLimit || c.RequestedReferences != len(p.Live) || c.CompletedReferences != len(p.Live) || len(v.ReferenceOutcomes) != len(p.Live) || len(v.Verification.States)+len(v.Unavailable) != len(p.Live) || c.StateBytes < 0 || c.StateBytes > LegacyReferenceStateLimit {
		return invalid()
	}
	if (len(p.Live) == 0 && c.InspectionBatches != 0) || (len(p.Live) > 0 && (c.InspectionBatches <= 0 || c.InspectionBatches > len(p.Live))) {
		return invalid()
	}
	if v.Started.IsZero() || v.Finished.Before(v.Started) || v.Finished.Sub(v.Started) > LegacyReferenceReadTimeout {
		return invalid()
	}
	if len(v.Unavailable) > 0 && len(v.Verification.States) == 0 {
		return zero, stale("insufficient same-account live evidence; preserve the mirror and restore access to an original immutable reference")
	}
	r := LegacyReconciliationReceipt{Version: 9, Method: v.Method, ReferenceContext: &c, Account: a, SourceDigest: p.digest, Coverage: v.Coverage, Started: v.Started, Finished: v.Finished, HistoricalAttributionConfirmed: v.Verification.HistoricalAttributionConfirmed, Folders: v.Verification.Folders}
	live := map[int64]bool{}
	numbers := map[int64]bool{}
	ids := map[string]bool{}
	folders := map[string]bool{}
	var stateBytes int64
	for _, m := range p.Live {
		if m.N <= 0 || numbers[m.N] || m.GraphID == "" || ids[m.GraphID] || !utf8.ValidString(m.GraphID) || !utf8.ValidString(m.Folder) {
			return invalid()
		}
		live[m.N], numbers[m.N], ids[m.GraphID], folders[m.Folder] = true, true, true, true
		scope, ok := v.Verification.Folders[m.Folder]
		if !ok || scope.Validate(a.ID) != nil || !utf8.ValidString(string(scope.CollectionID)) {
			return invalid()
		}
		o, ok := v.ReferenceOutcomes[m.N]
		if !ok || !referenceRefValid(a.ID, o.Ref) || o.Ref.MessageID != mailbox.MessageID(m.GraphID) || o.Ref.Locator != mailbox.Locator(m.GraphID) {
			return invalid()
		}
		state, found := v.Verification.States[m.N]
		reason, missing := v.Unavailable[m.N]
		ref := o.Ref
		i := LegacyReconciliationItem{N: m.N, OriginalID: m.GraphID, OriginalFolder: m.Folder, OriginalMemberships: []mailbox.Membership{{Collection: scope}}, ReadRef: &ref, ReadOutcome: o.Kind}
		switch o.Kind {
		case LegacyReferenceFound:
			if !found || missing || !o.SourceValidated || !referenceMetadataValid(state.Metadata) || !utf8.ValidString(state.Revision) {
				return invalid()
			}
			current, changed, e := reconciliationState(a.ID, m, scope, state)
			if e != nil {
				return zero, e
			}
			for _, membership := range current.Memberships {
				if !utf8.ValidString(string(membership.Collection.CollectionID)) {
					return invalid()
				}
			}
			raw, e := json.Marshal(current)
			if e != nil {
				return zero, e
			}
			if int64(len(raw)) > LegacyReferenceStateLimit-stateBytes {
				return invalid()
			}
			stateBytes += int64(len(raw))
			i.Disposition, i.State, i.MembershipChanged, i.SourceValidated = "verified", &current, changed, true
		case LegacyReferenceNotFound:
			if found || !missing || reason != LegacyReferenceUnavailable || o.SourceValidated {
				return invalid()
			}
			i.Disposition, i.Reason = "unavailable", LegacyReferenceUnavailable
		default:
			return invalid()
		}
		r.Items = append(r.Items, i)
	}
	for n := range v.ReferenceOutcomes {
		if !live[n] {
			return invalid()
		}
	}
	for n := range v.Verification.States {
		if !live[n] {
			return invalid()
		}
	}
	for n := range v.Unavailable {
		if !live[n] {
			return invalid()
		}
	}
	if stateBytes != c.StateBytes || len(folders) != len(v.Verification.Folders) {
		return invalid()
	}
	for _, m := range p.Removed {
		if m.N <= 0 || numbers[m.N] || !utf8.ValidString(m.GraphID) || (m.GraphID != "" && ids[m.GraphID]) {
			return invalid()
		}
		numbers[m.N] = true
		if m.GraphID != "" {
			ids[m.GraphID] = true
		}
		r.Items = append(r.Items, LegacyReconciliationItem{N: m.N, OriginalID: m.GraphID, Removed: true, Disposition: "historical"})
	}
	sort.Slice(r.Items, func(i, j int) bool { return r.Items[i].N < r.Items[j].N })
	approval, audit, e := reconciliationReceiptDigests(r)
	if e != nil {
		return zero, e
	}
	r.Digest, r.AuditDigest = approval, audit
	return r, nil
}
func referenceRefValid(account mailbox.AccountID, r mailbox.MessageRef) bool {
	return r.Validate(account) == nil && r.MessageID == mailbox.MessageID(r.Locator) && utf8.ValidString(string(r.AccountID)) && utf8.ValidString(string(r.MessageID)) && utf8.ValidString(string(r.Locator))
}
func referenceMetadataValid(m mailbox.Metadata) bool {
	for _, s := range []string{m.InternetMessageID, m.Sender.Address, m.Sender.Name, m.Subject, m.Preview} {
		if !utf8.ValidString(s) {
			return false
		}
	}
	for _, r := range m.Recipients {
		if !utf8.ValidString(r.Address) || !utf8.ValidString(r.Name) {
			return false
		}
	}
	return true
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
	outcomes := make(map[int64]LegacyReferenceOutcome, len(v.ReferenceOutcomes))
	for n, o := range v.ReferenceOutcomes {
		outcomes[n] = o
	}
	v.ReferenceOutcomes = outcomes
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

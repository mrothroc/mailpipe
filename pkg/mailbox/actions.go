package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Action selects recoverable semantics, never an arbitrary destination.
type Action string

const (
	Trash   Action = "trash"
	Archive Action = "archive"
)

// ReviewedAction carries a stored plan's identity preconditions. Positive IDs
// are well-formed context, NOT proof of review or owner execution approval.
// Callers must load the actual stored plan and establish authority separately.
type ReviewedAction struct {
	PlanID   int64
	Local    LocalRef
	Expected State
}

// Validate requires complete, account-consistent plan context.
func (r ReviewedAction) Validate(account AccountID) error {
	if r.PlanID <= 0 {
		return errors.New("reviewed action: positive stored plan ID is required")
	}
	if err := r.Local.Validate(account); err != nil {
		return err
	}
	return r.Expected.Validate(account)
}

// TrashCapability offers recoverable provider trash, never permanent deletion.
// Adapters must revalidate the reviewed target immediately at execution, using
// conditional/version operations where available. Inability to establish safe
// identity is stale/error; inability to preserve semantics is UnsupportedError.
// Successful identity changes require proven continuity and an explicit State.
// Ambiguous outcomes are errors requiring reconciliation before any retry.
type TrashCapability interface {
	Trash(context.Context, ReviewedAction) (State, error)
}

// ArchiveCapability offers the documented provider archive operation, without
// guessing folder names or removing every membership. The same execution-time
// identity, continuity and uncertain-outcome obligations as TrashCapability apply.
type ArchiveCapability interface {
	Archive(context.Context, ReviewedAction) (State, error)
}

// Capabilities are derived availability claims, not authority or a guarantee
// against subsequent credential, permission, network or server failures.
type Capabilities struct{ Trash, Archive, NativeRules bool }

// CapabilitiesOf uses the same method assertions as dispatch. Adapters expose
// only supported method sets for their configured account/mode. Read-only
// wrappers must not embed writable methods and try to disable them with a flag.
func CapabilitiesOf(p Provider) Capabilities {
	_, trash := p.(TrashCapability)
	_, archive := p.(ArchiveCapability)
	_, rules := p.(NativeRuleCapability)
	return Capabilities{Trash: trash, Archive: archive, NativeRules: rules}
}

// CheckPrecondition compares exact provider identity/locator, RFC Message-ID,
// sender address, membership set and any recorded revision. Sender comparison
// uses TrimSpace and Unicode EqualFold, matching existing apply behavior. It
// does not normalize Unicode, translate IDNA/punycode or compare display names.
// Distinct spellings not equated by EqualFold conservatively require re-review.
// Equality here is not atomic protection against changes on a server.
func CheckPrecondition(account AccountID, expected, current State) error {
	if err := expected.Validate(account); err != nil {
		return err
	}
	if err := current.Validate(account); err != nil {
		return err
	}
	reason := ""
	switch {
	case expected.Ref != current.Ref:
		reason = "provider identity or locator changed"
	case expected.Metadata.InternetMessageID != current.Metadata.InternetMessageID:
		reason = "message header identity changed"
	case !strings.EqualFold(strings.TrimSpace(expected.Metadata.Sender.Address), strings.TrimSpace(current.Metadata.Sender.Address)):
		reason = "sender changed"
	case expected.Revision != "" && expected.Revision != current.Revision:
		reason = "provider revision changed"
	default:
		memberships := make(map[CollectionRef]bool, len(expected.Memberships))
		for _, m := range expected.Memberships {
			memberships[m.Collection] = true
		}
		if len(expected.Memberships) != len(current.Memberships) {
			reason = "collection memberships changed"
		} else {
			for _, m := range current.Memberships {
				if !memberships[m.Collection] {
					reason = "collection memberships changed"
					break
				}
			}
		}
	}
	if reason != "" {
		return &StaleIdentityError{Operation: "check reviewed message", Reason: reason}
	}
	return nil
}

// ApplyReviewed validates context, refuses absent capabilities without I/O,
// inspects current preconditions and invokes exactly one optional operation.
// It never retries, falls back or establishes owner approval. The adapter must
// still enforce execution-time preconditions: preflight inspection is not atomic.
func ApplyReviewed(ctx context.Context, p Provider, action Action, r ReviewedAction) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if p == nil {
		return State{}, errors.New("reviewed action: provider is required")
	}
	account := p.Account()
	if err := account.Validate(); err != nil {
		return State{}, err
	}
	if err := r.Validate(account.ID); err != nil {
		return State{}, err
	}
	var execute func(context.Context, ReviewedAction) (State, error)
	switch action {
	case Trash:
		if capability, ok := p.(TrashCapability); ok {
			execute = capability.Trash
		}
	case Archive:
		if capability, ok := p.(ArchiveCapability); ok {
			execute = capability.Archive
		}
	default:
		return State{}, &UnsupportedError{Operation: "apply reviewed action"}
	}
	if execute == nil {
		return State{}, &UnsupportedError{Operation: string(action)}
	}
	current, err := p.Inspect(ctx, r.Expected.Ref)
	if err != nil {
		return State{}, fmt.Errorf("inspect reviewed message: %w", err)
	}
	if err = CheckPrecondition(account.ID, r.Expected, current); err != nil {
		return State{}, err
	}
	if err = ctx.Err(); err != nil {
		return State{}, err
	}
	result, err := execute(ctx, r)
	if err != nil {
		return State{}, fmt.Errorf("execute reviewed action: %w", err)
	}
	if err = result.Validate(account.ID); err != nil {
		return State{}, fmt.Errorf("invalid action result (reconcile before retry): %w", err)
	}
	return result, nil
}

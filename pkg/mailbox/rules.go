package mailbox

import (
	"context"
	"errors"
	"net/mail"
	"strings"
)

// RuleRef is an opaque native-rule identity, scoped to an account.
type RuleRef struct {
	AccountID AccountID
	ID        string
}

// Validate checks rule account and identity without interpreting the ID.
func (r RuleRef) Validate(account AccountID) error {
	if err := boundAccount("native rule reference", account, r.AccountID); err != nil {
		return err
	}
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("native rule reference: identity is required")
	}
	return nil
}

// NativeRule represents only exact-sender-to-recoverable-trash semantics. There
// is no required display name, sequence number, query language or update method.
// Other native rules remain visible by Ref with Representable=false and must
// not be round-tripped or mutated. Revision is an optional opaque precondition.
type NativeRule struct {
	Ref             RuleRef
	SenderAddresses []string
	Revision        string
	Representable   bool
}

func sendersValid(senders []string) error {
	if len(senders) == 0 {
		return errors.New("native rule: exact sender addresses are required")
	}
	for _, s := range senders {
		address, err := mail.ParseAddress(s)
		if err != nil || address.Address != s || s != strings.TrimSpace(s) || strings.ContainsAny(s, "\r\n") {
			return errors.New("native rule: each sender must be one exact email address without a display name")
		}
	}
	return nil
}

// ReviewedRuleCreation describes a separately reviewed exact-sender trash rule.
// ReviewID identifies stored review context; it cannot establish owner approval.
type ReviewedRuleCreation struct {
	AccountID       AccountID
	ReviewID        int64
	SenderAddresses []string
}

// Validate checks account and well-formed review context; adapters must reject
// any predicate/effect they cannot implement exactly, without approximation.
func (r ReviewedRuleCreation) Validate(account AccountID) error {
	if err := boundAccount("create native rule", account, r.AccountID); err != nil {
		return err
	}
	if r.ReviewID <= 0 {
		return errors.New("create native rule: positive stored review ID is required")
	}
	return sendersValid(r.SenderAddresses)
}

// ReviewedRuleDeletion carries the complete expected representable rule. The
// caller establishes managed ownership and approval independently; the adapter
// compares expected rule/revision immediately before deletion and fails stale
// if changed. Mail content and display names do not establish rule ownership.
type ReviewedRuleDeletion struct {
	AccountID AccountID
	ReviewID  int64
	Expected  NativeRule
}

// Validate refuses unrepresentable native rules rather than approximating them.
func (r ReviewedRuleDeletion) Validate(account AccountID) error {
	if err := boundAccount("delete native rule", account, r.AccountID); err != nil {
		return err
	}
	if err := r.Expected.Ref.Validate(account); err != nil {
		return err
	}
	if r.ReviewID <= 0 {
		return errors.New("delete native rule: positive stored review ID is required")
	}
	if !r.Expected.Representable {
		return &UnsupportedError{Operation: "delete unrepresentable native rule"}
	}
	return sendersValid(r.Expected.SenderAddresses)
}

// NativeRuleCapability is optional server-side rule management, separate from
// scan-time policy. Only exact-sender-to-recoverable-trash list/create/delete is
// modeled. Unsupported predicates, effects, ordering or replacement must return
// UnsupportedError; no generic wire payload or default mutation is available.
// Implementations validate requests before transport, require caller-established
// ownership/review and recheck expected rule state before deleting. Errors after
// uncertain writes require reconciliation, never blind retries.
type NativeRuleCapability interface {
	ListRules(context.Context) ([]NativeRule, error)
	CreateRule(context.Context, ReviewedRuleCreation) (NativeRule, error)
	DeleteRule(context.Context, ReviewedRuleDeletion) error
}

// NativeRulesFor returns optional native-rule support or a typed actionable
// error. It does not inspect, create, delete or approve any rule.
func NativeRulesFor(p Provider) (NativeRuleCapability, error) {
	if rules, ok := p.(NativeRuleCapability); ok {
		return rules, nil
	}
	return nil, &UnsupportedError{Operation: "native rules"}
}

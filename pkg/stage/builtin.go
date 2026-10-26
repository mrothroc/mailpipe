package stage

import (
	"context"
	"strings"
)

// Allowlist keeps mail from the owner (their addresses and domains) and from
// known contacts (everyone the owner has sent mail to). A hit is final: no
// later stage needs to look at that message.
type Allowlist struct {
	name      string
	self      map[string]bool
	domains   []string
	contacts  map[string]bool
	automated map[string]bool
}

func lowerSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			set[v] = true
		}
	}
	return set
}

// NewAllowlist builds an Allowlist; matching is case-insensitive. Automated
// senders (agents or scripts, even on the owner's own domains) are never kept,
// so policy rules apply to their mail.
func NewAllowlist(name string, selfAddresses, selfDomains, contacts, automated []string) *Allowlist {
	var domains []string
	for d := range lowerSet(selfDomains) {
		domains = append(domains, d)
	}
	return &Allowlist{name: name, self: lowerSet(selfAddresses), domains: domains, contacts: lowerSet(contacts), automated: lowerSet(automated)}
}

func (a *Allowlist) Name() string    { return a.name }
func (a *Allowlist) NeedsBody() bool { return false }

// IsSelf reports whether address belongs to the owner.
func (a *Allowlist) IsSelf(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	if a.automated[address] {
		return false
	}
	if a.self[address] {
		return true
	}
	_, domain, ok := strings.Cut(address, "@")
	if !ok {
		return false
	}
	for _, d := range a.domains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

func (a *Allowlist) Evaluate(_ context.Context, m Message) (Verdict, error) {
	v := Verdict{Stage: a.name, Provenance: "allowlist"}
	switch {
	case a.IsSelf(m.Sender):
		v.Labels, v.Action, v.Final, v.Reason = []string{"self"}, Keep, true, "sent by the owner"
	case a.contacts[strings.ToLower(strings.TrimSpace(m.Sender))] && !a.automated[strings.ToLower(strings.TrimSpace(m.Sender))]:
		v.Labels, v.Action, v.Final, v.Reason = []string{"known_contact"}, Keep, true, "the owner has sent mail to this address"
	}
	return v, nil
}

// Honeypot flags mail addressed to a honeypot address by anyone but the owner.
type Honeypot struct {
	name      string
	addresses map[string]bool
	owner     *Allowlist
}

// NewHoneypot builds a Honeypot; owner decides which senders are the owner.
func NewHoneypot(name string, addresses []string, owner *Allowlist) *Honeypot {
	return &Honeypot{name: name, addresses: lowerSet(addresses), owner: owner}
}

func (h *Honeypot) Name() string    { return h.name }
func (h *Honeypot) NeedsBody() bool { return false }

func (h *Honeypot) Evaluate(_ context.Context, m Message) (Verdict, error) {
	v := Verdict{Stage: h.name, Provenance: "honeypot"}
	if h.owner != nil && h.owner.IsSelf(m.Sender) {
		return v, nil
	}
	for _, r := range m.Recipients {
		if h.addresses[strings.ToLower(strings.TrimSpace(r))] {
			v.Labels = []string{"honeypot"}
			v.Scores = map[string]float64{"honeypot": 1}
			v.Reason = "addressed to a honeypot"
			return v, nil
		}
	}
	return v, nil
}

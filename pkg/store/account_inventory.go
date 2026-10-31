package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// IMAPInventoryEntry contains occurrence identity only, never cached content or
// a success-shaped partial Message. Provider identity is the store's binding.
type IMAPInventoryEntry struct {
	Local  mailbox.LocalRef
	Remote mailbox.MessageRef
	IMAP   IMAPLocator
}

const (
	IMAPInventoryMaxRows       = 100000
	IMAPInventoryMaxFieldBytes = 8192
	// IMAPInventoryMaxBytes bounds logical retained identity data, not process RSS,
	// SQLite page/cache allocations or allocator/container overhead.
	IMAPInventoryMaxBytes = 64 << 20
)

// IMAPInventoryLimitError reports a fixed resource dimension without identities.
type IMAPInventoryLimitError struct {
	Dimension string
	Limit     int64
}

func (e *IMAPInventoryLimitError) Error() string {
	return fmt.Sprintf("store: IMAP inventory %s limit %d exceeded", e.Dimension, e.Limit)
}
func inventoryField(s string) error {
	if len(s) > IMAPInventoryMaxFieldBytes {
		return &IMAPInventoryLimitError{"field_bytes", IMAPInventoryMaxFieldBytes}
	}
	return nil
}

// ReadIMAPInventory reads a bounded bodyless projection in one SQLite snapshot.
// Scalar preflight checks all byte lengths BEFORE the driver converts opaque
// strings. Then only N, message ID, locator and UID are projected. Each entry's
// logical charge is 256 + 2*len(account) + len(collection) + len(mailbox) +
// len(messageID) + len(locator). Any failure returns nil, never a partial prefix.
// SQLite may still read database pages internally. No cursor spans network work.
func (a *AccountStore) ReadIMAPInventory(ctx context.Context, scope mailbox.CollectionRef, expected IMAPEpoch) ([]IMAPInventoryEntry, error) {
	if e := a.scope(scope); e != nil {
		return nil, e
	}
	for _, v := range []string{string(a.account.ID), string(a.account.ProviderID), string(scope.CollectionID), expected.MailboxID} {
		if e := inventoryField(v); e != nil {
			return nil, e
		}
	}
	if a.account.ProviderID == GraphProvider || strings.TrimSpace(expected.MailboxID) == "" || expected.UIDValidity == 0 || expected.Generation <= 0 {
		return nil, stale("initialized IMAP epoch required")
	}
	var out []IMAPInventoryEntry
	e := a.transaction(ctx, func(q sqlAccess) error {
		var matches bool
		e := q.QueryRowContext(ctx, `select mailbox_id=? and uidvalidity=? and epoch_generation=? from collections where account_id=? and provider_id=? and collection_id=?`, expected.MailboxID, expected.UIDValidity, expected.Generation, a.account.ID, a.account.ProviderID, scope.CollectionID).Scan(&matches)
		if errors.Is(e, sql.ErrNoRows) {
			return stale("IMAP collection missing")
		}
		if e != nil {
			return e
		}
		if !matches {
			return stale("IMAP inventory epoch changed")
		}
		// Match membership first, but never filter a bad/missing active locator into
		// apparent absence. LEFT JOIN makes those inconsistent live rows fail closed.
		from := ` from messages m join memberships b on b.n=m.n and b.account_id=m.account_id and b.provider_id=m.provider_id
left join imap_locators l on l.n=m.n and l.valid=1
where m.account_id=? and m.provider_id=? and b.collection_id=? and m.identity_valid=1 order by m.n limit 100001`
		rows, e := q.QueryContext(ctx, `select m.n,typeof(m.provider_message_id),length(cast(m.provider_message_id as blob)),typeof(m.locator),length(cast(m.locator as blob)),
coalesce(l.account_id=m.account_id and l.provider_id=m.provider_id and l.collection_id=b.collection_id and l.mailbox_id=? and l.uidvalidity=? and l.epoch_generation=? and l.uid between 1 and 4294967295,0),
coalesce(typeof(l.mailbox_id)='text' and length(cast(l.mailbox_id as blob))<=8192 and typeof(l.collection_id)='text' and length(cast(l.collection_id as blob))<=8192,0)`+from, expected.MailboxID, expected.UIDValidity, expected.Generation, a.account.ID, a.account.ProviderID, scope.CollectionID)
		if e != nil {
			return e
		}
		count, total := 0, int64(0)
		base := int64(256 + 2*len(a.account.ID) + len(scope.CollectionID) + len(expected.MailboxID))
		for rows.Next() {
			var n, idBytes, locBytes int64
			var idType, locType string
			var tuple, fields bool
			if e = rows.Scan(&n, &idType, &idBytes, &locType, &locBytes, &tuple, &fields); e != nil {
				break
			}
			count++
			if count > IMAPInventoryMaxRows {
				e = &IMAPInventoryLimitError{"rows", IMAPInventoryMaxRows}
				break
			}
			if n <= 0 || !tuple || !fields || idType != "text" || locType != "text" || idBytes <= 0 || locBytes <= 0 {
				e = stale("invalid scoped IMAP occurrence")
				break
			}
			if idBytes > IMAPInventoryMaxFieldBytes || locBytes > IMAPInventoryMaxFieldBytes {
				e = &IMAPInventoryLimitError{"field_bytes", IMAPInventoryMaxFieldBytes}
				break
			}
			charge := base + idBytes + locBytes
			if charge > IMAPInventoryMaxBytes-total {
				e = &IMAPInventoryLimitError{"aggregate_bytes", IMAPInventoryMaxBytes}
				break
			}
			total += charge
			if e = ctx.Err(); e != nil {
				break
			}
		}
		e = errors.Join(e, rows.Err(), rows.Close())
		if e != nil {
			return e
		}
		rows, e = q.QueryContext(ctx, `select m.n,m.provider_message_id,m.locator,l.uid`+from, a.account.ID, a.account.ProviderID, scope.CollectionID)
		if e != nil {
			return e
		}
		entries := make([]IMAPInventoryEntry, 0, count)
		uids := make(map[uint32]bool, count)
		last := int64(0)
		for rows.Next() {
			v := IMAPInventoryEntry{Local: mailbox.LocalRef{AccountID: a.account.ID}, Remote: mailbox.MessageRef{AccountID: a.account.ID}, IMAP: IMAPLocator{CollectionID: scope.CollectionID, MailboxID: expected.MailboxID, UIDValidity: expected.UIDValidity, EpochGeneration: expected.Generation}}
			if e = rows.Scan(&v.Local.N, &v.Remote.MessageID, &v.Remote.Locator, &v.IMAP.UID); e != nil {
				break
			}
			if len(entries) >= count || v.Local.N <= last || uids[v.IMAP.UID] || v.IMAP.UID == 0 {
				e = stale("duplicate or changed IMAP inventory")
				break
			}
			if e = v.Remote.Validate(a.account.ID); e != nil {
				break
			}
			if e = inventoryField(string(v.Remote.MessageID)); e != nil {
				break
			}
			if e = inventoryField(string(v.Remote.Locator)); e != nil {
				break
			}
			last = v.Local.N
			uids[v.IMAP.UID] = true
			entries = append(entries, v)
			if e = ctx.Err(); e != nil {
				break
			}
		}
		e = errors.Join(e, rows.Err(), rows.Close())
		if e != nil {
			return e
		}
		if len(entries) != count {
			return stale("IMAP inventory changed")
		}
		out = entries
		return nil
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

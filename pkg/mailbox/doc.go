// Package mailbox defines provider-neutral accounts, message identities,
// collection memberships and optional reviewed capabilities.
//
// This package is a foundation for later adapter and consumer migrations; it
// does not wire providers into the current application. It depends only on the
// standard library and does not own storage, credentials, policy or transport.
// Stages judge, policy decides, and callers establish execution authority from
// reviewed stored plans before invoking actions. A plan number is not approval.
package mailbox

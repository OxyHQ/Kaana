// Package sessionowner describes only the ephemeral location of a realtime
// session. It contains no credential, customer identity, conversation or audio.
package sessionowner

import (
	"context"
	"time"
)

const (
	Public        = "public"
	Internal      = "internal"
	LeaseDuration = 45 * time.Second
	RenewInterval = 15 * time.Second
)

// Entry locates an existing session; it cannot authorize or recreate one.
type Entry struct {
	RequestID  string
	Lane       string
	Address    string
	Generation string
	ExpiresAt  time.Time
}

// Directory uses compare-and-set on the process generation. A stale process
// cannot renew or remove a replacement owner's entry.
type Directory interface {
	Claim(context.Context, Entry) (bool, error)
	Lookup(context.Context, string, string) (Entry, bool, error)
	Renew(context.Context, Entry) (bool, error)
	Release(context.Context, Entry) error
}

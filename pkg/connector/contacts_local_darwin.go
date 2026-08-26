//go:build darwin && !ios

package connector

import (
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/imessage"
	"github.com/lrhodin/corten-matrix/imessage/mac"
)

// localContactSource wraps the macOS Contacts framework as a contactSource.
// Used when backfill_source=chatdb (no iCloud, local-only).
type localContactSource struct {
	store *mac.ContactStore

	mu       sync.RWMutex
	contacts []*imessage.Contact
	lastSync time.Time
}

func newLocalContactSource(log zerolog.Logger) contactSource {
	cs := mac.NewContactStore()
	if err := cs.RequestContactAccess(); err != nil {
		log.Warn().Err(err).Msg("Failed to request macOS contact access")
		return nil
	}
	if !cs.HasContactAccess {
		log.Warn().Msg("macOS contact access denied — contacts command will be unavailable")
		return nil
	}
	log.Info().Msg("Using local macOS Contacts for contact resolution")
	return &localContactSource{store: cs}
}

func (l *localContactSource) SyncContacts(log zerolog.Logger) error {
	contacts, err := l.store.GetContactList()
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.contacts = contacts
	l.lastSync = time.Now()
	l.mu.Unlock()
	log.Info().Int("count", len(contacts)).Msg("Loaded local macOS contacts")
	return nil
}

// CacheStatus implements contactSource. Lookups go straight to the macOS
// Contacts framework rather than a local map, so the count is only used to
// report how much the last load saw; a non-zero lastSync means the store is
// usable.
func (l *localContactSource) CacheStatus() (int, time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.contacts), l.lastSync
}

func (l *localContactSource) GetContactInfo(identifier string) (*imessage.Contact, error) {
	return l.store.GetContactInfo(identifier)
}

func (l *localContactSource) GetAllContacts() []*imessage.Contact {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.contacts
}

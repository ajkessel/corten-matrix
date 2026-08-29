// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the contact-cache replace guard and the name hold-back it enables.
//
// Regression context: a transient CardDAV error (Google answers REPORT with
// HTTP 500) used to be swallowed — the fetch loop skipped the failed address
// book, the cache was replaced with an EMPTY one, and SyncContacts reported
// success. Every subsequent name resolution then fell back to the raw
// phone/email, so the next inbound message rewrote the sender's ghost display
// name and fanned a "changed their display name" member event into every
// shared room. The following good sync flipped them all back.

package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/lrhodin/corten-matrix/imessage"
)

// fakeContactSource is a contactSource whose cache contents and sync state are
// set directly by the test.
type fakeContactSource struct {
	contacts map[string]*imessage.Contact
	count    int
	lastSync time.Time
}

func (f *fakeContactSource) SyncContacts(zerolog.Logger) error { return nil }

func (f *fakeContactSource) GetContactInfo(identifier string) (*imessage.Contact, error) {
	return f.contacts[identifier], nil
}

func (f *fakeContactSource) GetAllContacts() []*imessage.Contact {
	all := make([]*imessage.Contact, 0, len(f.contacts))
	for _, c := range f.contacts {
		all = append(all, c)
	}
	return all
}

func (f *fakeContactSource) CacheStatus() (int, time.Time) { return f.count, f.lastSync }

func TestCheckContactCacheReplace(t *testing.T) {
	boom := errors.New("REPORT returned 500")
	cases := []struct {
		name       string
		cached     int
		fetched    int
		books      int
		fetchErr   error
		wantReject bool
	}{
		// The bug: 2474 cached contacts, one address book, its REPORT 500s, so
		// the fetch yields nothing. Never let that land.
		{"failed fetch empties a full cache", 2474, 0, 1, boom, true},
		// Same shape without an explicit fetch error (a server answering 207
		// with an empty multistatus). Zero is still never plausible.
		{"silent empty response", 2474, 0, 1, nil, true},
		{"no address books listed", 2474, 0, 0, nil, true},
		// A partial multi-address-book failure that loses most of the book.
		{"partial fetch loses most contacts", 2474, 100, 3, boom, true},
		// Plausible results must pass through untouched.
		{"partial fetch keeps most contacts", 2474, 2400, 3, boom, false},
		{"clean full fetch", 2474, 2474, 1, nil, false},
		// A user genuinely deleting most of their address book still applies:
		// the shrink rule only fires when a fetch actually failed.
		{"user deleted most contacts", 2474, 10, 1, nil, false},
		// First sync of an empty cache must always be allowed, including the
		// legitimately-empty address book (otherwise the cache never warms).
		{"first sync", 0, 2474, 1, nil, false},
		{"first sync of empty address book", 0, 0, 1, nil, false},
		{"first sync that failed", 0, 0, 1, boom, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkContactCacheReplace(tc.cached, tc.fetched, tc.books, tc.fetchErr)
			if tc.wantReject && err == nil {
				t.Fatalf("checkContactCacheReplace(%d, %d, %d, %v) = nil, want rejection",
					tc.cached, tc.fetched, tc.books, tc.fetchErr)
			}
			if !tc.wantReject && err != nil {
				t.Fatalf("checkContactCacheReplace(%d, %d, %d, %v) = %v, want nil",
					tc.cached, tc.fetched, tc.books, tc.fetchErr, err)
			}
			// The rejection must keep the underlying cause reachable: the
			// periodic loop's errors.Is(err, errICloudContactsThrottled) check
			// is what turns an Apple 403 into a hard backoff.
			if tc.wantReject && tc.fetchErr != nil && !errors.Is(err, tc.fetchErr) {
				t.Errorf("checkContactCacheReplace() = %v, want it to wrap %v", err, tc.fetchErr)
			}
		})
	}
}

// carddavTestServer serves the three discovery steps and delegates the vCard
// REPORT to reportHandler, so a test can fail just that step.
func carddavTestServer(t *testing.T, reportHandler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		multistatus := func(body string) {
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(207)
			fmt.Fprint(w, body)
		}
		switch {
		case r.Method == "REPORT":
			reportHandler(w)
		case r.Method == "PROPFIND" && r.URL.Path == "/":
			multistatus(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response>` +
				`<d:href>/</d:href><d:propstat><d:prop><d:current-user-principal>` +
				`<d:href>/principal/</d:href></d:current-user-principal></d:prop>` +
				`<d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		case r.Method == "PROPFIND" && r.URL.Path == "/principal/":
			multistatus(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">` +
				`<d:response><d:href>/principal/</d:href><d:propstat><d:prop><card:addressbook-home-set>` +
				`<d:href>/books/</d:href></card:addressbook-home-set></d:prop>` +
				`<d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		case r.Method == "PROPFIND" && r.URL.Path == "/books/":
			multistatus(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">` +
				`<d:response><d:href>/books/default/</d:href><d:propstat><d:prop><d:resourcetype>` +
				`<d:collection/><card:addressbook/></d:resourcetype><d:displayname>Address Book</d:displayname>` +
				`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestExternalCardDAVSyncKeepsCacheOnFetchFailure is the regression test for the
// wipe itself: a 500 on the vCard REPORT must fail the sync and leave the
// previously-synced contacts in place, so the periodic loop backs off (and
// skips the readiness/refresh passes) instead of running on an empty book.
func TestExternalCardDAVSyncKeepsCacheOnFetchFailure(t *testing.T) {
	srv := carddavTestServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error": {"code": 500, "message": "Internal error encountered."}}`)
	})

	esther := &imessage.Contact{FirstName: "Esther", LastName: "Example", Emails: []string{"esther@example.com"}}
	lastSync := time.Now().Add(-15 * time.Minute)
	client := &externalCardDAVClient{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
		byPhone:    map[string]*imessage.Contact{},
		byEmail:    map[string]*imessage.Contact{"esther@example.com": esther},
		contacts:   []*imessage.Contact{esther},
		lastSync:   lastSync,
	}

	err := client.SyncContacts(zerolog.Nop())
	if err == nil {
		t.Fatal("SyncContacts() = nil, want error so the periodic loop backs off and skips setContactsReady")
	}
	if !strings.Contains(err.Error(), "refusing to replace") {
		t.Errorf("SyncContacts() error = %v, want the cache-replace rejection", err)
	}

	contact, _ := client.GetContactInfo("esther@example.com")
	if contact == nil || contact.FirstName != "Esther" {
		t.Fatalf("GetContactInfo() = %#v, want the cached contact to survive a failed sync", contact)
	}
	count, gotSync := client.CacheStatus()
	if count != 1 || !gotSync.Equal(lastSync) {
		t.Errorf("CacheStatus() = (%d, %v), want (1, %v) — a rejected fetch must not touch the cache",
			count, gotSync, lastSync)
	}
}

// TestExternalCardDAVSyncReplacesCacheOnSuccess pins the other side: a healthy
// fetch still swaps the cache and stamps lastSync, so the guard can't silently
// freeze contacts.
func TestExternalCardDAVSyncReplacesCacheOnSuccess(t *testing.T) {
	srv := carddavTestServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">`+
			`<d:response><d:href>/books/default/1.vcf</d:href><d:propstat><d:prop><card:address-data>`+
			"BEGIN:VCARD\nVERSION:3.0\nFN:Fresh Contact\nN:Contact;Fresh;;;\nEMAIL:fresh@example.com\nEND:VCARD"+
			`</card:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	})

	stale := &imessage.Contact{FirstName: "Stale", Emails: []string{"stale@example.com"}}
	client := &externalCardDAVClient{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
		byPhone:    map[string]*imessage.Contact{},
		byEmail:    map[string]*imessage.Contact{"stale@example.com": stale},
		contacts:   []*imessage.Contact{stale},
		lastSync:   time.Now().Add(-15 * time.Minute),
	}

	if err := client.SyncContacts(zerolog.Nop()); err != nil {
		t.Fatalf("SyncContacts() = %v, want nil", err)
	}
	if contact, _ := client.GetContactInfo("fresh@example.com"); contact == nil {
		t.Error("GetContactInfo(fresh@example.com) = nil, want the newly synced contact")
	}
	if contact, _ := client.GetContactInfo("stale@example.com"); contact != nil {
		t.Error("GetContactInfo(stale@example.com) != nil, want the replaced cache to drop it")
	}
}

func testGhost(id, name string) *bridgev2.Ghost {
	return &bridgev2.Ghost{Ghost: &database.Ghost{ID: makeUserID(id), Name: name}}
}

func testNameClient(store contactSource) *IMClient {
	cfg := IMConfig{DisplaynameTemplate: "{{ or .Nickname .FirstName .Phone .Email .ID }}"}
	if err := cfg.PostProcess(); err != nil {
		panic(err)
	}
	c := &IMClient{Main: &IMConnector{Config: cfg}}
	if store != nil {
		c.setContactStore(store)
	}
	return c
}

// TestGetUserInfoHoldsNameWhileContactsDegraded is the regression test for the
// visible symptom: with the address book unavailable, a ghost that already has
// a contact-resolved name must NOT be rewritten to its Apple ID. Returning nil
// (rather than a UserInfo with a nil Name) also leaves the avatar untouched —
// bridgev2 latches AvatarSet on a nil Avatar inside a non-nil UserInfo.
func TestGetUserInfoHoldsNameWhileContactsDegraded(t *testing.T) {
	// Source configured but never successfully synced: exactly the state a
	// rejected sync (or a pre-first-sync restart) leaves behind.
	degraded := &fakeContactSource{contacts: map[string]*imessage.Contact{}}
	c := testNameClient(degraded)

	info, err := c.GetUserInfo(context.Background(), testGhost("mailto:esther@example.com", "Esther Example"))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info != nil {
		got := "<nil name>"
		if info.Name != nil {
			got = *info.Name
		}
		t.Fatalf("GetUserInfo() = %q, want nil so the resolved name is left alone", got)
	}
}

// TestGetUserInfoNamesUnnamedGhostWhileDegraded: the hold-back must not leave a
// brand-new ghost nameless — it would render as a raw @corten_mailto=3a… MXID
// in clients and push notifications.
func TestGetUserInfoNamesUnnamedGhostWhileDegraded(t *testing.T) {
	degraded := &fakeContactSource{contacts: map[string]*imessage.Contact{}}
	c := testNameClient(degraded)

	info, err := c.GetUserInfo(context.Background(), testGhost("mailto:esther@example.com", ""))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info == nil || info.Name == nil || *info.Name != "esther@example.com" {
		t.Fatalf("GetUserInfo() = %#v, want the identifier fallback for a nameless ghost", info)
	}
}

// TestGetUserInfoFallsBackWhenCacheHealthy: a healthy-but-missing contact is a
// real "not in the address book" answer, so the identifier fallback still
// applies — including for a ghost that already has a name (a deleted contact
// must be able to downgrade).
func TestGetUserInfoFallsBackWhenCacheHealthy(t *testing.T) {
	healthy := &fakeContactSource{contacts: map[string]*imessage.Contact{}, count: 2474, lastSync: time.Now()}
	c := testNameClient(healthy)

	info, err := c.GetUserInfo(context.Background(), testGhost("mailto:esther@example.com", "Esther Example"))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info == nil || info.Name == nil || *info.Name != "esther@example.com" {
		t.Fatalf("GetUserInfo() = %#v, want the identifier fallback when the address book is healthy", info)
	}
}

// TestGetUserInfoFallsBackWithNoContactSource: with no address book configured
// at all there is no later sync coming, so the identifier fallback is the
// answer and must not be held back.
func TestGetUserInfoFallsBackWithNoContactSource(t *testing.T) {
	c := testNameClient(nil)

	info, err := c.GetUserInfo(context.Background(), testGhost("tel:+15551234567", "Old Name"))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info == nil || info.Name == nil || *info.Name != "+15551234567" {
		t.Fatalf("GetUserInfo() = %#v, want the identifier fallback with no contact source", info)
	}
}

func TestContactsDegraded(t *testing.T) {
	cases := []struct {
		name  string
		store contactSource
		want  bool
	}{
		{"no source configured", nil, false},
		{"configured, never synced", &fakeContactSource{}, true},
		{"synced with contacts", &fakeContactSource{count: 10, lastSync: time.Now()}, false},
		// Genuinely empty address book: the replace guard means an empty cache
		// with a completed sync is the truth, not a wipe.
		{"synced but empty", &fakeContactSource{count: 0, lastSync: time.Now()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testNameClient(tc.store)
			if got := c.contactsDegraded(); got != tc.want {
				t.Errorf("contactsDegraded() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveContactDisplaynameDegraded pins the self-chat / DM-title side of
// the hold-back: "" means "no answer yet", which dmFocusName and the GetChatInfo
// self-chat branch treat as "leave the current title alone".
func TestResolveContactDisplaynameDegraded(t *testing.T) {
	degraded := testNameClient(&fakeContactSource{contacts: map[string]*imessage.Contact{}})
	if got := degraded.resolveContactDisplayname("mailto:adam@example.com"); got != "" {
		t.Errorf("resolveContactDisplayname() = %q, want \"\" while contacts are degraded", got)
	}

	healthy := testNameClient(&fakeContactSource{
		contacts: map[string]*imessage.Contact{},
		count:    2474,
		lastSync: time.Now(),
	})
	if got := healthy.resolveContactDisplayname("mailto:adam@example.com"); got != "adam@example.com" {
		t.Errorf("resolveContactDisplayname() = %q, want the identifier fallback when healthy", got)
	}

	named := testNameClient(&fakeContactSource{
		contacts: map[string]*imessage.Contact{"adam@example.com": {FirstName: "Adam", LastName: "Example"}},
		count:    2474,
		lastSync: time.Now(),
	})
	if got := named.resolveContactDisplayname("mailto:adam@example.com"); got != "Adam" {
		t.Errorf("resolveContactDisplayname() = %q, want the contact name", got)
	}
}

// TestGetUserInfoHoldsSharedProfileWhileContactsDegraded is the regression test
// for the startup avatar double-write: on connect, cached shared iMessage
// profiles are pushed to every ghost BEFORE the first CardDAV sync finishes, so
// a ghost with both a shared photo and an address-book photo used to get both
// written seconds apart — two uploads, two avatar_url PUTs, and a member event
// in every shared room, on every restart. While the address book is merely
// unavailable, the lower-priority source must not overwrite an existing profile.
func TestGetUserInfoHoldsSharedProfileWhileContactsDegraded(t *testing.T) {
	c := testNameClient(&fakeContactSource{contacts: map[string]*imessage.Contact{}})
	sharedPhoto := []byte("shared-profile-photo-bytes")
	c.sharedProfiles.Store("mailto:cf@example.com", &sharedProfileRow{
		Identifier: "mailto:cf@example.com",
		FirstName:  "Clayton",
		LastName:   "Example",
		Avatar:     sharedPhoto,
	})

	info, err := c.GetUserInfo(context.Background(), testGhost("mailto:cf@example.com", "Clayton Example"))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info != nil {
		t.Fatalf("GetUserInfo() = %#v, want nil so the shared profile does not overwrite the rendered profile", info)
	}
}

// TestGetUserInfoUsesSharedProfileWhenCacheHealthy: once contacts are loaded, a
// handle genuinely absent from the address book still falls through to its
// shared iMessage profile — the hold-back must not disable that source.
func TestGetUserInfoUsesSharedProfileWhenCacheHealthy(t *testing.T) {
	c := testNameClient(&fakeContactSource{
		contacts: map[string]*imessage.Contact{},
		count:    2474,
		lastSync: time.Now(),
	})
	c.sharedProfiles.Store("mailto:cf@example.com", &sharedProfileRow{
		Identifier: "mailto:cf@example.com",
		FirstName:  "Clayton",
		LastName:   "Example",
	})

	info, err := c.GetUserInfo(context.Background(), testGhost("mailto:cf@example.com", "cf@example.com"))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info == nil || info.Name == nil || *info.Name != "Clayton" {
		t.Fatalf("GetUserInfo() = %#v, want the shared-profile name", info)
	}
}

// TestGetUserInfoNamesUnnamedGhostFromSharedProfileWhileDegraded: a brand-new
// ghost must still get a name even during the cold-contacts window, otherwise
// it renders as a raw MXID until the first sync lands.
func TestGetUserInfoNamesUnnamedGhostFromSharedProfileWhileDegraded(t *testing.T) {
	c := testNameClient(&fakeContactSource{contacts: map[string]*imessage.Contact{}})
	c.sharedProfiles.Store("mailto:cf@example.com", &sharedProfileRow{
		Identifier: "mailto:cf@example.com",
		FirstName:  "Clayton",
		LastName:   "Example",
	})

	info, err := c.GetUserInfo(context.Background(), testGhost("mailto:cf@example.com", ""))
	if err != nil {
		t.Fatalf("GetUserInfo() error = %v", err)
	}
	if info == nil || info.Name == nil || *info.Name != "Clayton" {
		t.Fatalf("GetUserInfo() = %#v, want the shared-profile name for a nameless ghost", info)
	}
}

// TestContactsDegradedIsTimeBounded: the hold-back exists to bridge the gap
// while a contact source that is coming up hasn't answered yet. Left unbounded
// it becomes a permanent regression on an install whose CardDAV never syncs —
// every self-chat and group title stays unresolved for the life of the process.
func TestContactsDegradedIsTimeBounded(t *testing.T) {
	c := testNameClient(&fakeContactSource{contacts: map[string]*imessage.Contact{}})

	if !c.contactsDegraded() {
		t.Fatal("a freshly installed source that hasn't synced must count as degraded")
	}

	c.contactsMu.Lock()
	c.contactsInstalledAt = time.Now().Add(-contactsDegradedGrace - time.Minute)
	c.contactsMu.Unlock()
	if c.contactsDegraded() {
		t.Error("a source that never synced past the grace period must stop holding names back")
	}

	// A source that did sync is never degraded, however long ago it was
	// installed.
	healthy := testNameClient(&fakeContactSource{count: 2474, lastSync: time.Now()})
	healthy.contactsMu.Lock()
	healthy.contactsInstalledAt = time.Now().Add(-24 * time.Hour)
	healthy.contactsMu.Unlock()
	if healthy.contactsDegraded() {
		t.Error("a synced source must never be degraded")
	}
}

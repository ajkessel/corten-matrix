// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the diverged-DM-room-name repair.

package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// repairPortal builds the portal shape the sweep filters on.
func repairPortal(id2 networkid.PortalID, mxid id.RoomID, roomType database.RoomType, name string, nameIsCustom bool, receiver networkid.UserLoginID) *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{
		PortalKey:    networkid.PortalKey{ID: id2, Receiver: receiver},
		MXID:         mxid,
		RoomType:     roomType,
		Name:         name,
		NameSet:      true,
		NameIsCustom: nameIsCustom,
	}}
}

// TestDMNameRepairSkipRules pins which portals the sweep is willing to touch.
// Getting this wrong is expensive in both directions: too broad and it re-pushes
// names the moon path owns (seizing titles it shouldn't), too narrow and the
// rooms stuck showing a phone number stay stuck.
func TestDMNameRepairSkipRules(t *testing.T) {
	const login = networkid.UserLoginID("D:1")
	cases := []struct {
		name   string
		portal *bridgev2.Portal
		want   bool // eligible for a name check
	}{
		{
			name:   "plain DM with an implicit name",
			portal: repairPortal("tel:+15551234567", "!a:example.com", database.RoomTypeDM, "Sam Example", false, login),
			want:   true,
		},
		{
			name:   "custom name is ours to own (focus moon, self-chat)",
			portal: repairPortal("tel:+15551234567", "!a:example.com", database.RoomTypeDM, "Sam Example 🌙", true, login),
			want:   false,
		},
		{
			name:   "group rooms are named explicitly, not derived from a ghost",
			portal: repairPortal("tel:+1,tel:+2", "!a:example.com", database.RoomTypeDefault, "Family", false, login),
			want:   false,
		},
		{
			name:   "portal with no Matrix room",
			portal: repairPortal("tel:+15551234567", "", database.RoomTypeDM, "Sam Example", false, login),
			want:   false,
		},
		{
			name:   "another login's portal",
			portal: repairPortal("tel:+15551234567", "!a:example.com", database.RoomTypeDM, "Sam Example", false, "D:2"),
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dmNameRepairEligible(tc.portal, login); got != tc.want {
				t.Errorf("dmNameRepairEligible() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDMNameRepairNeedsPush is the comparison at the heart of the sweep: the
// live Matrix name against the name the framework would derive. The observed
// break was a room stuck at "+15557654321" while both the ghost and the stored
// portal row said "Sam Example" — bridgev2 pushed the implicit name without
// persisting the row, so nothing could detect it afterwards.
func TestDMNameRepairNeedsPush(t *testing.T) {
	cases := []struct {
		name string
		live string
		want string
		push bool
	}{
		{"stuck on the raw phone number", "+15557654321", "Sam Example", true},
		{"stuck on the raw Apple ID", "someone@example.com", "Alex Example", true},
		{"already correct", "Sam Example", "Sam Example", false},
		{"no name event at all", "", "Sam Example", true},
		// Nothing to push toward: leave the room alone rather than blanking it.
		{"nothing to derive", "+15557654321", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dmNameNeedsRepair(tc.live, tc.want); got != tc.push {
				t.Errorf("dmNameNeedsRepair(%q, %q) = %v, want %v", tc.live, tc.want, got, tc.push)
			}
		})
	}
}

// TestDMNameRepairEnabled pins the config gate. Under private_chat_portal_meta:
// false bridgev2 leaves DMs unnamed on purpose, so an absent name event is not
// damage there — without this gate the sweep would push explicit titles into
// every non-custom DM on those installs, which is why dmNameNeedsRepair("",
// want) returning true is only ever reached with the setting on.
func TestDMNameRepairEnabled(t *testing.T) {
	cases := []struct {
		name       string
		pcpm, done bool
		want       bool
	}{
		{"framework owns DM names, not yet run", true, false, true},
		{"already recorded as done", true, true, false},
		{"PCPM off: DMs are intentionally unnamed", false, false, false},
		{"PCPM off and done", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dmNameRepairEnabled(tc.pcpm, tc.done); got != tc.want {
				t.Errorf("dmNameRepairEnabled(%v, %v) = %v, want %v", tc.pcpm, tc.done, got, tc.want)
			}
		})
	}
}

// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// One-time repair for DM room names that diverged from the bridge's own state.

package connector

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

const (
	// dmNameRepairKey marks the sweep as complete so it runs once per install.
	// Bump the suffix to re-run it after changing what the sweep repairs.
	dmNameRepairKey = database.Key("corten_dm_name_repair_v1")

	// dmNameRepairBudget caps repairs per startup. Each is one state PUT; the
	// sweep is idempotent and the KV flag is only written once a full pass ends
	// under budget, so a large backlog finishes over a few startups.
	dmNameRepairBudget = 300

	// dmNameRepairPace spaces the state reads so a sweep over thousands of DMs
	// doesn't burst the homeserver during startup.
	dmNameRepairPace = 20 * time.Millisecond
)

// repairDivergedDMRoomNames re-pushes m.room.name for DM portals whose live
// Matrix name disagrees with the name the bridge believes the room has.
//
// Such a room can never self-correct. bridgev2 pushes a DM's implicit name from
// the ghost via ghost.updateDMPortals -> lockedUpdateInfoFromGhost, which
// discards UpdateInfoFromGhost's "changed" result and never calls portal.Save
// (unlike the portal.UpdateInfo path, which does). So when that push lands, the
// portal row keeps its old name and NameSet stays true. Once the portal is next
// loaded from the database, updateName sees name == ghost.Name && NameSet and
// early-returns: Matrix keeps whatever was pushed, forever.
//
// The damage this repairs was done by a raw-handle window — a startup or a
// wiped contact cache left ghosts named from their phone/email, and the implicit
// push copied that into the room name. On one live bridge 7 of 60 sampled DMs
// were stuck showing a phone number or Apple ID, all pushed within the same five
// seconds. The window itself is fixed elsewhere (the contact-cache replace guard
// and the cold-contacts hold-back in GetUserInfo); this only cleans up rooms
// that are already wrong.
//
// Rooms with a custom name (NameIsCustom, e.g. the focus-moon title) are left
// alone: their name is ours to own, not the ghost's to derive.
func (c *IMClient) repairDivergedDMRoomNames(log zerolog.Logger) {
	log = log.With().Str("component", "dm_name_repair").Logger()
	ctx := log.WithContext(context.Background())

	if done := c.Main.Bridge.DB.KV.Get(ctx, dmNameRepairKey); done != "" {
		return
	}
	stateReader, ok := c.Main.Bridge.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState)
	if !ok {
		log.Debug().Msg("Matrix connector can't read arbitrary room state — skipping DM name repair")
		return
	}

	portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to load portals for DM name repair")
		return
	}

	var checked, repaired, failed int
	budgetLeft := dmNameRepairBudget
	for _, portal := range portals {
		if budgetLeft <= 0 {
			log.Info().Int("checked", checked).Int("repaired", repaired).
				Msg("DM name repair hit its per-startup budget — resuming on next startup")
			return
		}
		if !dmNameRepairEligible(portal, c.UserLogin.ID) {
			continue
		}
		want := c.dmGhostName(ctx, portal)
		if want == "" {
			continue
		}

		select {
		case <-time.After(dmNameRepairPace):
		case <-c.stopChan:
			return
		}
		checked++

		live, err := stateReader.GetStateEvent(ctx, portal.MXID, event.StateRoomName, "")
		if err != nil {
			log.Debug().Err(err).Str("portal_mxid", string(portal.MXID)).
				Msg("Failed to read room name for DM name repair")
			continue
		}
		liveName := ""
		if live != nil {
			if content, ok := live.Content.Parsed.(*event.RoomNameEventContent); ok && content != nil {
				liveName = content.Name
			}
		}
		if !dmNameNeedsRepair(liveName, want) {
			// Matrix is right. The stored row may still be stale in the other
			// direction; leave it, since nothing renders from it once it agrees.
			continue
		}

		budgetLeft--
		// Mirror sendRoomMeta's own output: the implicit flag (the name is
		// derived, not custom) and no timeline entry, so a correction doesn't
		// read as someone renaming the room.
		_, err = c.Main.Bridge.Bot.SendState(ctx, portal.MXID, event.StateRoomName, "", &event.Content{
			Parsed: &event.RoomNameEventContent{Name: want},
			Raw: map[string]any{
				"fi.mau.implicit_name":             true,
				"com.beeper.exclude_from_timeline": true,
			},
		}, time.Time{})
		if err != nil {
			failed++
			log.Warn().Err(err).Str("portal_mxid", string(portal.MXID)).
				Msg("Failed to re-push diverged DM room name")
			continue
		}
		repaired++
		log.Info().
			Str("portal_id", string(portal.ID)).
			Str("portal_mxid", string(portal.MXID)).
			Bool("live_name_was_empty", liveName == "").
			Msg("Repaired DM room name that had diverged from the bridge's stored name")

		// Bring the row in line with what Matrix now holds, so the framework's
		// early-return is telling the truth from here on.
		if portal.Name != want || !portal.NameSet {
			portal.Name = want
			portal.NameSet = true
			if err := portal.Save(ctx); err != nil {
				log.Warn().Err(err).Str("portal_mxid", string(portal.MXID)).
					Msg("Failed to save portal after repairing its room name")
			}
		}
	}

	if failed > 0 {
		log.Info().Int("checked", checked).Int("repaired", repaired).Int("failed", failed).
			Msg("DM name repair finished with failures — will retry on next startup")
		return
	}
	c.Main.Bridge.DB.KV.Set(ctx, dmNameRepairKey, time.Now().Format(time.RFC3339))
	log.Info().Int("checked", checked).Int("repaired", repaired).
		Msg("DM name repair complete")
}

// dmNameRepairEligible reports whether a portal's room name is the framework's
// to derive from a ghost — the only case this sweep may re-push.
//
// NameIsCustom means the title is ours (the focus moon, a self-chat), and the
// moon refresh already maintains those; group rooms get an explicit name from
// the participant roster, not from a ghost.
func dmNameRepairEligible(portal *bridgev2.Portal, receiver networkid.UserLoginID) bool {
	return portal.Receiver == receiver &&
		portal.MXID != "" &&
		portal.RoomType == database.RoomTypeDM &&
		!portal.NameIsCustom
}

// dmNameNeedsRepair reports whether the live Matrix room name disagrees with the
// name the framework would derive. An empty target means there is nothing better
// to offer, so the room is left as it is rather than blanked.
func dmNameNeedsRepair(live, want string) bool {
	return want != "" && live != want
}

// dmGhostName returns the name the framework would derive for a DM room: the
// other user's ghost name, falling back to the stored portal name.
func (c *IMClient) dmGhostName(ctx context.Context, portal *bridgev2.Portal) string {
	for _, uid := range []string{string(portal.OtherUserID), string(portal.ID)} {
		if uid == "" {
			continue
		}
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, networkid.UserID(uid))
		if err == nil && ghost != nil && ghost.Name != "" {
			return ghost.Name
		}
	}
	return portal.Name
}

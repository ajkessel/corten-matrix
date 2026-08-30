// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// One-time repair for DM room names that diverged from the bridge's own state.

package connector

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
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

	// Only meaningful when the framework owns DM names. Under
	// private_chat_portal_meta: false bridgev2 deliberately sets no
	// m.room.name on a DM at all (UpdateInfoFromGhost and updateDMPortals both
	// early-return), leaving clients to derive the title from the member. On
	// such an install every non-custom DM would look like damage — an absent
	// name event — and this sweep would seize up to a budget's worth of titles
	// the framework intentionally left implicit.
	if !dmNameRepairEnabled(c.Main.Bridge.Config.PrivateChatPortalMeta,
		c.Main.Bridge.DB.KV.Get(ctx, dmNameRepairKey) != "") {
		return
	}
	// Dispatched from setContactsReady, which fires on every contact-sync tick
	// (~15 min), not just at startup. Without this a pass that ends with a
	// permanently failing room — the bot lacking the power level for
	// m.room.name, say — would never write the flag and would re-sweep every
	// tick forever. One attempt per process; a genuine retry costs a restart.
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

	// Claim the single per-process attempt only once the sweep can actually
	// start. Placed above the two exits before it, a failure to even load the
	// portal list would have burned the attempt for the whole process.
	if c.dmNameRepairRan.Swap(true) {
		return
	}

	var checked, repaired, failed, transient int
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
		var pushErr error
		_, pushErr = c.Main.Bridge.Bot.SendState(ctx, portal.MXID, event.StateRoomName, "", &event.Content{
			Parsed: &event.RoomNameEventContent{Name: want},
			Raw: map[string]any{
				"fi.mau.implicit_name":             true,
				"com.beeper.exclude_from_timeline": true,
			},
		}, time.Time{})
		if pushErr != nil {
			failed++
			if !dmNameRepairFailurePermanent(pushErr) {
				transient++
			}
			log.Warn().Err(pushErr).Str("portal_mxid", string(portal.MXID)).
				Bool("permanent", dmNameRepairFailurePermanent(pushErr)).
				Msg("Failed to re-push diverged DM room name")
			continue
		}
		repaired++
		log.Info().
			Str("portal_id", string(portal.ID)).
			Str("portal_mxid", string(portal.MXID)).
			Bool("live_name_was_empty", liveName == "").
			Msg("Repaired DM room name that had diverged from the bridge's stored name")

		// Deliberately NOT writing portal.Name/NameSet here.
		// GetAllPortalsWithMXID hands back the bridge's CACHED *Portal
		// instances, and lockedUpdateInfoFromGhost takes roomCreateLock (which
		// this package cannot reach) precisely to serialize those fields — so
		// mutating them here would race it. The push alone is the repair: the
		// room now carries the ghost's name, which is what the stored row
		// already claimed in every observed case. If a row does disagree, the
		// framework's next ghost-driven update pushes the same value again,
		// which is a no-op on the homeserver.
	}

	if failed > 0 && repaired > 0 {
		// Made progress but not cleanly: leave the flag unwritten so the next
		// startup finishes the rest. The once-per-process guard above keeps
		// that a per-restart retry rather than a per-tick one.
		log.Info().Int("checked", checked).Int("repaired", repaired).Int("failed", failed).
			Msg("DM name repair finished with failures — will retry on next startup")
		return
	}
	if failed > 0 && transient > 0 {
		// Nothing repaired, but at least one failure could clear on its own — a
		// 5xx or a network error, e.g. a homeserver restart spanning the sweep.
		// Retiring the repair on that would strand the damage permanently, so
		// leave the flag for the next startup.
		log.Info().Int("checked", checked).Int("failed", failed).Int("transient", transient).
			Msg("DM name repair failed transiently — will retry on next startup")
		return
	}
	if failed > 0 {
		// Repaired nothing and every failure is structural (the bot lacking the
		// power level for m.room.name, or no longer being in the room), so a
		// retry would fail identically. Write the flag and stop sweeping
		// thousands of rooms on every restart forever.
		log.Warn().Int("checked", checked).Int("failed", failed).
			Msg("DM name repair could not repair any room — recording it as done rather than retrying")
	}
	c.Main.Bridge.DB.KV.Set(ctx, dmNameRepairKey, time.Now().Format(time.RFC3339))
	log.Info().Int("checked", checked).Int("repaired", repaired).
		Msg("DM name repair complete")
}

// dmNameRepairFailurePermanent reports whether a failed name push will fail the
// same way on the next attempt.
//
// M_FORBIDDEN is the structural case this sweep actually hits: the bridge bot
// lacks the power level for m.room.name, or is no longer in the room. Those
// don't heal, so a pass consisting only of them should record itself as done.
// A 5xx, a timeout or a transport error is the opposite — a homeserver restart
// spanning the sweep produces the identical "nothing repaired" signature, and
// retiring the repair on that would strand the damage for good.
func dmNameRepairFailurePermanent(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.RespError != nil {
		switch httpErr.RespError.ErrCode {
		case mautrix.MForbidden.ErrCode, mautrix.MNotFound.ErrCode, mautrix.MUnrecognized.ErrCode:
			return true
		case mautrix.MLimitExceeded.ErrCode:
			return false
		}
	}
	if httpErr.Response == nil {
		return false
	}
	// Any other 4xx is a request we will keep getting wrong — except 429, which
	// is the homeserver asking us to come back later.
	if httpErr.Response.StatusCode == http.StatusTooManyRequests {
		return false
	}
	return httpErr.Response.StatusCode >= 400 && httpErr.Response.StatusCode < 500
}

// dmNameRepairEnabled reports whether the sweep should run at all.
//
// privateChatPortalMeta is load-bearing: with it off, bridgev2 deliberately
// sets no m.room.name on DMs (UpdateInfoFromGhost and updateDMPortals both
// early-return), so every non-custom DM would present as damage — an absent
// name event — and the sweep would seize titles the framework intentionally
// left for the client to derive. The divergence this repairs is created by the
// implicit-name path, which only exists when the setting is on.
func dmNameRepairEnabled(privateChatPortalMeta, alreadyDone bool) bool {
	return privateChatPortalMeta && !alreadyDone
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

package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

type CashSessionIDs struct {
	Canonical    string
	LegacySheets string
}

func (ids CashSessionIDs) Matches(id string) bool {
	return id == ids.Canonical || id == ids.LegacySheets
}

func CashSessionIdentities(date time.Time, firstHandID string) CashSessionIDs {
	hash := func(clock string) string {
		sum := sha256.Sum256([]byte(clock + "|" + firstHandID))
		return hex.EncodeToString(sum[:])[:12]
	}
	return CashSessionIDs{Canonical: hash(date.UTC().Format(time.RFC3339)), LegacySheets: hash(date.Format("2006-01-02 15:04"))}
}

type CashWrite struct {
	New, Recorded int
}

func CashHandIDs(hands []parsers.CashHand) []string {
	ids := make([]string, len(hands))
	for i, hand := range hands {
		ids[i] = hand.ID
	}
	return ids
}

func ReconcileCashHands(rows []CashSessionRecord, session parsers.CashSession, hands []parsers.CashHand, liveSource string) ([]CashSessionRecord, CashWrite, error) {
	var result CashWrite
	identities := CashSessionIdentities(session.Date, session.FirstHandID)
	target := -1
	for i, row := range rows {
		if identities.Matches(row.SessionID) {
			if target >= 0 {
				return nil, result, fmt.Errorf("cash session has conflicting legacy identities")
			}
			target = i
		}
	}

	live := liveSource != ""
	dirty := make(map[int]bool)
	adopted := false
	if live && target >= 0 {
		present := make(map[string]bool, len(hands))
		for _, hand := range hands {
			present[hand.ID] = true
		}
		row := &rows[target]
		retained := slices.DeleteFunc(slices.Clone(row.HandDetails), func(hand parsers.CashHand) bool {
			return hand.LiveSource == liveSource && !present[hand.ID]
		})
		if len(retained) > 0 && len(retained) < len(row.HandDetails) {
			row.HandDetails = retained
			dirty[target] = true
		}
	}
	if target >= 0 && len(rows[target].HandDetails) == 0 {
		old := &rows[target]
		if len(hands) < old.Hands || old.Hands == 0 {
			return nil, CashWrite{Recorded: min(len(hands), old.Hands)}, nil
		}
		prefix, err := parsers.SessionFromCashHands(session.Stake, hands[:old.Hands])
		if err != nil {
			return nil, result, err
		}
		if prefix.NetCents != old.NetCents || prefix.RakeCents != old.RakeCents ||
			prefix.Stake != old.Stake || prefix.DurationMin != old.DurationMin ||
			!sameNumber(prefix.VPIP, old.VPIP, 1e-9) || !sameNumber(prefix.PFR, old.PFR, 1e-9) {
			return nil, result, nil
		}
		if live && canAttachCashHands(rows, hands[:old.Hands]) {
			old.HandDetails = slices.Clone(hands[:old.Hands])
			for i := range old.HandDetails {
				old.HandDetails[i].LiveSource = liveSource
			}
			adopted = true
			dirty[target] = true
		} else {
			result.Recorded = old.Hands
			hands = hands[old.Hands:]
			target = -1
		}
	}

	type position struct{ row, hand int }
	owners := make(map[string]position)
	for i, row := range rows {
		for j, hand := range row.HandDetails {
			if _, exists := owners[hand.ID]; exists {
				return nil, result, fmt.Errorf("cash hand %s has conflicting owners", hand.ID)
			}
			owners[hand.ID] = position{i, j}
		}
	}

	for _, hand := range hands {
		if hand.ID == "" || hand.At.IsZero() || hand.BigBlindCents <= 0 {
			return nil, result, fmt.Errorf("invalid cash hand contribution")
		}
		if owner, found := owners[hand.ID]; found {
			result.Recorded++
			row := &rows[owner.row]
			hand.LiveSource = row.HandDetails[owner.hand].LiveSource
			if live && row.HandDetails[owner.hand] != hand {
				if !dirty[owner.row] {
					row.HandDetails = slices.Clone(row.HandDetails)
				}
				row.HandDetails[owner.hand] = hand
				dirty[owner.row] = true
			}
			continue
		}
		if target < 0 {
			if owner, known := owners[hands[0].ID]; known {
				target = owner.row
			}
		}
		if target < 0 {
			target = len(rows)
			id := CashSessionIdentities(hand.At, hand.ID).Canonical
			date := hand.At
			if result.Recorded == 0 {
				id, date = identities.Canonical, session.Date
			}
			rows = append(rows, CashSessionRecord{
				CashSession: parsers.CashSession{Date: date, Stake: session.Stake},
				SessionID:   id,
			})
		}
		row := &rows[target]
		if !dirty[target] {
			row.HandDetails = slices.Clone(row.HandDetails)
		}
		owners[hand.ID] = position{target, len(row.HandDetails)}
		hand.LiveSource = liveSource
		row.HandDetails = append(row.HandDetails, hand)
		dirty[target] = true
		result.New++
	}

	var changed []CashSessionRecord
	for i, row := range rows {
		if !dirty[i] {
			continue
		}
		if adopted && i == target && result.New == 0 {
			changed = append(changed, row)
			continue
		}
		total, err := parsers.SessionFromCashHands(row.Stake, row.HandDetails)
		if err != nil {
			return nil, result, err
		}
		total.Date = row.Date
		row.CashSession = total
		changed = append(changed, row)
	}
	return changed, result, nil
}

func canAttachCashHands(rows []CashSessionRecord, prefix []parsers.CashHand) bool {
	ids := make(map[string]bool, len(prefix))
	for _, hand := range prefix {
		if ids[hand.ID] {
			return false
		}
		ids[hand.ID] = true
	}
	for _, row := range rows {
		for _, hand := range row.HandDetails {
			if ids[hand.ID] {
				return false
			}
		}
	}
	return true
}

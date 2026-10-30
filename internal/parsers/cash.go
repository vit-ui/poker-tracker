package parsers

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
)

// CashSession holds aggregated stats for a single cash game session file.
type CashSession struct {
	Date        time.Time
	Stake       string
	BuyInCents  money.Cents
	Hands       int
	DurationMin int
	NetCents    money.Cents
	RakeCents   money.Cents
	BB100       float64
	VPIP        float64
	PFR         float64

	// FirstHandID is the PokerStars Hand # of the first hand in the file —
	// globally unique and assigned once, so it never changes as PokerStars
	// appends later hands to the same file. Storage backends use it (with
	// Date) to identify this session across repeated re-parses of the same
	// growing file, the same role TournamentID plays for tournaments.
	FirstHandID string
}

// TournamentHandStats holds stats extracted from a tournament hand history
// file. Returned by ParseCashFile when the file is a tournament session.
// Knockout-bounty winnings only ever appear here — the tournament summary
// file has no concept of them at all, only the hand history file's
// elimination lines do (why two files: plan §Why Tournaments Need Two
// Files) — so BountyCents and IsKnockout, like VPIP/PFR, are always populated
// from this struct, never from ParseTournamentFile.
type TournamentHandStats struct {
	TournamentID string
	VPIP         float64
	PFR          float64

	// Hands is the number of hands dealt to screenName in this tournament so
	// far — the same weight CashSession.Hands provides for cash sessions,
	// letting /stats weight a tournament's VPIP/PFR by how much of a sample
	// they actually represent. Recomputed from scratch on every parse, same
	// as VPIP/PFR/BountyCents, since the file keeps growing across the whole
	// tournament.
	Hands int

	// BountyCents is the cumulative knockout-bounty cash won by screenName
	// across every "<name> wins $X.XX for eliminating ..." line in the file,
	// recomputed from scratch on every parse — the same file keeps growing
	// with every hand across the whole tournament (not per table), so this
	// is always the tournament's true cumulative bounty total, not a
	// per-call increment. Zero for a non-knockout tournament.
	BountyCents money.Cents

	// IsKnockout is true if any seat in the file shows a bounty ("$X.XX
	// bounty)" in the pre-hand seat listing) — present for every seat in a
	// knockout tournament regardless of whether screenName ever eliminates
	// anyone, unlike BountyCents which can legitimately be zero in a knockout
	// tournament screenName didn't score a KO in.
	IsKnockout bool
}

// cashAgg holds the file-level accumulators a hand-history parse builds up as
// it scans forward. Every field is monotonic across the strictly append-only
// file (PokerStars only ever adds complete hands at the end), so a later parse
// can resume from a byte offset carrying these values instead of rescanning
// from zero — see ParseState / ParseCashFileFrom.
type cashAgg struct {
	firstTime, lastTime time.Time
	bigBlindCents       money.Cents
	stake               string
	firstHandID         string
	hands               int
	netCents, rakeCents money.Cents
	vpipCount, pfrCount int

	isTournament bool
	tournamentID string
	bountyCents  money.Cents
	isKnockout   bool
}

// ParseState is the carry-forward state that lets ParseCashFileFrom resume a
// growing hand-history file instead of re-parsing it from byte 0 on every
// event. It holds the accumulators for every hand that starts strictly before
// consumedOffset, and consumedOffset itself — the byte offset of the last hand
// header seen, always a completed-hand boundary. The last hand is deliberately
// left out of the carried aggregates and re-parsed on the next resume, so a
// hand that was mid-write when first seen self-corrects. Callers treat it as
// opaque: obtain it from ParseCashFileFrom and pass it back next time.
type ParseState struct {
	agg            cashAgg
	consumedOffset int64
}

// ParseCashFile parses a PokerStars hand history file from the hand history
// folder. Returns a CashSession for cash game files, or TournamentHandStats
// for tournament hand history files. Exactly one of the two will be non-nil.
func ParseCashFile(path, screenName string) (session *CashSession, tourneyStats *TournamentHandStats, err error) {
	session, tourneyStats, _, err = ParseCashFileFrom(path, screenName, nil)
	return session, tourneyStats, err
}

// ParseCashFileFrom is ParseCashFile with an incremental fast path. With a nil
// prior it parses the whole file from byte 0 (identical to ParseCashFile).
// With a non-nil prior — the ParseState returned by the previous parse of the
// same growing file — it seeks to prior.consumedOffset and scans only the new
// bytes, folding them into the carried accumulators. The returned ParseState
// must be passed back on the next parse of this file.
//
// The optimization never affects correctness: if the file is shorter than
// prior.consumedOffset, or the resume point no longer begins with a hand
// header (the file was rewritten, not appended to), it silently discards the
// carried state and re-parses from byte 0. Because storage upserts by session
// ID / TournamentID, a redundant full re-parse is always safe.
func ParseCashFileFrom(path, screenName string, prior *ParseState) (*CashSession, *TournamentHandStats, *ParseState, error) {
	if prior != nil {
		session, stats, st, err, ok := parseCashFrom(path, screenName, prior)
		if ok {
			return session, stats, st, err
		}
		// Resume point invalid (file rewritten/truncated) — fall back to a
		// full re-parse, which is always correct.
	}
	session, stats, st, err, _ := parseCashFrom(path, screenName, nil)
	return session, stats, st, err
}

// parseCashFrom does the actual scan. ok is false only when a non-nil prior's
// resume point is no longer valid, signalling ParseCashFileFrom to fall back to
// a full parse; a nil prior always returns ok true.
func parseCashFrom(path, screenName string, prior *ParseState) (*CashSession, *TournamentHandStats, *ParseState, error, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening %s: %w", path, err), true
	}
	defer f.Close()

	var (
		startOffset int64
		base        cashAgg
	)
	if prior != nil {
		info, err := f.Stat()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err), true
		}
		if prior.consumedOffset > info.Size() {
			return nil, nil, nil, nil, false // file shrank — fall back
		}
		if _, err := f.Seek(prior.consumedOffset, io.SeekStart); err != nil {
			return nil, nil, nil, nil, false
		}
		startOffset = prior.consumedOffset
		base = prior.agg
	}

	r := bufio.NewReader(f)

	// st is the working aggregate (includes every hand finalized so far);
	// committed is a snapshot taken at each hand header, so it always excludes
	// the hand starting at newConsumedOffset. The returned ParseState carries
	// committed + newConsumedOffset; the returned result is built from st.
	st := base
	committed := base
	newConsumedOffset := startOffset

	var (
		handActive         bool
		inPreflop          bool
		playerDealt        bool
		playerVPIP         bool
		playerPFR          bool
		streetTotalCents   money.Cents
		handInvestedCents  money.Cents
		handCollectedCents money.Cents
		handUncalledCents  money.Cents
		handRakeCents      money.Cents
	)

	ap := screenName + ": "
	dealtPfx := "Dealt to " + screenName + " ["
	collectedPfx := screenName + " collected "
	uncalledPfx := "Uncalled bet ("
	uncalledSfx := ") returned to " + screenName
	bountyWinPfx := screenName + " wins $"
	const bountySeatMarker = " bounty)"

	var amountErr error
	add := func(targetCents *money.Cents, valueCents money.Cents) {
		if amountErr == nil {
			*targetCents, amountErr = money.Add(*targetCents, valueCents)
		}
	}
	parseAmount := func(text string) (money.Cents, bool) {
		valueCents, ok := parseLeadingAmount(text)
		if !ok && amountErr == nil {
			amountErr = fmt.Errorf("invalid hand amount %q", text)
		}
		return valueCents, ok
	}
	finalizeHand := func() {
		if !handActive {
			return
		}
		if playerDealt {
			st.hands++
			handNetCents, err := money.Add(handCollectedCents, handUncalledCents)
			if err == nil {
				handNetCents, err = money.Subtract(handNetCents, handInvestedCents)
			}
			if err != nil && amountErr == nil {
				amountErr = err
			}
			add(&st.netCents, handNetCents)
			add(&st.rakeCents, handRakeCents)
			if playerVPIP {
				st.vpipCount++
			}
			if playerPFR {
				st.pfrCount++
			}
		}
		handActive = false
		inPreflop = false
		playerDealt = false
		playerVPIP = false
		playerPFR = false
		streetTotalCents = 0
		handInvestedCents = 0
		handCollectedCents = 0
		handUncalledCents = 0
		handRakeCents = 0
	}

	offset := startOffset
	firstLine := true
	for {
		raw, readErr := r.ReadString('\n')
		if len(raw) > 0 {
			lineStart := offset
			offset += int64(len(raw))
			line := trimLineEnding(raw)
			if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
				line = line[3:]
			}

			// A non-nil prior stores the byte offset of a hand header; if the
			// first line we read there isn't one, the file was rewritten in
			// place, so bail to the full-parse fallback.
			if firstLine && prior != nil && !isHandHeader(line) {
				return nil, nil, nil, nil, false
			}
			firstLine = false

			switch {
			case isHandHeader(line):
				finalizeHand()
				committed = st
				newConsumedOffset = lineStart

				if strings.Contains(line, "Tournament #") {
					if !st.isTournament {
						st.isTournament = true
						if tidx := strings.Index(line, "Tournament #"); tidx >= 0 {
							rest := line[tidx+len("Tournament #"):]
							if end := strings.Index(rest, ","); end > 0 {
								st.tournamentID = rest[:end]
							}
						}
					}
				} else {
					if st.bigBlindCents == 0 {
						st.firstHandID = extractHandID(line)

						parenIdx := strings.Index(line, "($")
						if parenIdx == -1 {
							return nil, nil, nil, fmt.Errorf("no blinds in header: %s", line), true
						}
						afterParen := line[parenIdx+2:]
						slashIdx := strings.Index(afterParen, "/")
						if slashIdx == -1 {
							return nil, nil, nil, fmt.Errorf("no / in blinds: %s", line), true
						}
						bbStr := afterParen[slashIdx+1:]
						if len(bbStr) > 0 && bbStr[0] == '$' {
							bbStr = bbStr[1:]
						}
						bbCents, ok := parseAmount(bbStr)
						if !ok {
							return nil, nil, nil, fmt.Errorf("bad BB in header: %s", line), true
						}
						st.bigBlindCents = bbCents
						st.stake = "NL" + strconv.FormatInt(int64(bbCents), 10)
					}

					// Timestamp is after the last " - " in the header.
					tsPart := line
					for {
						idx := strings.Index(tsPart, " - ")
						if idx == -1 {
							break
						}
						tsPart = tsPart[idx+3:]
					}
					ts := tsPart
					// PokerStars sometimes emits single-digit hours ("1:01:07" not "01:01:07").
					if len(ts) >= 18 && ts[12] == ':' {
						ts = ts[:11] + "0" + ts[11:]
					}
					if len(ts) >= 19 {
						t, err := time.Parse("2006/01/02 15:04:05", ts[:19])
						if err == nil {
							if st.firstTime.IsZero() {
								st.firstTime = t
							}
							st.lastTime = t
						}
					}
				}

				handActive = true

			case line == "*** HOLE CARDS ***":
				// Only Hold'em/Omaha deal via "*** HOLE CARDS ***", so VPIP/PFR
				// (preflop concepts) are only counted for those games. Stud/draw
				// rounds in a mixed game (HORSE, 8-Game) open with "*** 3rd STREET
				// ***" / "*** DEALING HANDS ***" instead and never set inPreflop,
				// so their hands land in the denominator (playerDealt still fires
				// on "Dealt to") but never register a VPIP/PFR action — VPIP/PFR
				// are deliberately Hold'em/Omaha-only and read as diluted (or 0
				// for an all-stud/draw sample) on mixed formats. Money fields are
				// unaffected: tournament net comes from the summary plus folded
				// bounty, not from these streets.
				inPreflop = true

			case strings.HasPrefix(line, dealtPfx):
				playerDealt = true

			case strings.HasPrefix(line, "Seat ") && strings.Contains(line, bountySeatMarker):
				// Every seat in a knockout tournament shows a bounty in its
				// pre-hand listing ("Seat 2: Name (5000 in chips, $0.36
				// bounty)"), repeated on every hand — checked regardless of
				// screenName, since screenName can finish a knockout tournament
				// without ever scoring a KO themselves (bounty would then
				// stay 0, but it's still a knockout tournament).
				st.isKnockout = true

			case strings.HasPrefix(line, bountyWinPfx):
				// "<screenName> wins $X.XX for eliminating <Name> and their own
				// bounty increases by $Y.YY to $Z.ZZ" — the summary file has no
				// concept of bounties at all, so this is the only source for
				// knockout winnings (why two files: plan §Why Tournaments Need
				// Two Files). The amount right after "wins $" is the real cash
				// credited, which can differ by a cent from the "increases by"
				// figure due to the 50/50 split-and-round on an odd bounty.
				if amtCents, ok := parseAmount(line[len(bountyWinPfx):]); ok {
					add(&st.bountyCents, amtCents)
				}

			case strings.HasPrefix(line, "*** FLOP ***"):
				inPreflop = false
				streetTotalCents = 0

			case strings.HasPrefix(line, "*** TURN ***"):
				streetTotalCents = 0

			case strings.HasPrefix(line, "*** RIVER ***"):
				streetTotalCents = 0

			case line == "*** SHOW DOWN ***", line == "*** SUMMARY ***":
				inPreflop = false

			case strings.HasPrefix(line, ap+"posts "):
				rest := line[len(ap+"posts "):]
				dollarIdx := strings.Index(rest, "$")
				if dollarIdx >= 0 {
					if amtCents, ok := parseAmount(rest[dollarIdx+1:]); ok {
						add(&handInvestedCents, amtCents)
						add(&streetTotalCents, amtCents)
					}
				}

			case strings.HasPrefix(line, ap+"calls "):
				// Both cash ($X) and tournament (X chips) use this prefix.
				if amtCents, ok := parseAmount(line[len(ap+"calls "):]); ok {
					add(&handInvestedCents, amtCents)
					add(&streetTotalCents, amtCents)
					if inPreflop {
						playerVPIP = true
					}
				}

			case strings.HasPrefix(line, ap+"bets "):
				if amtCents, ok := parseAmount(line[len(ap+"bets "):]); ok {
					add(&handInvestedCents, amtCents)
					add(&streetTotalCents, amtCents)
				}

			case strings.HasPrefix(line, ap+"raises "):
				// Cash: "raises $X to $Y"; tournament: "raises X to Y [and is all-in]"
				toIdx := strings.Index(line, " to ")
				if toIdx >= 0 {
					if totalCents, ok := parseAmount(line[toIdx+4:]); ok {
						incrementCents := totalCents - streetTotalCents
						if incrementCents < 0 {
							incrementCents = 0
						}
						add(&handInvestedCents, incrementCents)
						streetTotalCents = totalCents
						if inPreflop {
							playerVPIP = true
							playerPFR = true
						}
					}
				}

			case strings.HasPrefix(line, uncalledPfx) && strings.HasSuffix(line, uncalledSfx):
				closeIdx := strings.Index(line, ")")
				if closeIdx > len(uncalledPfx) {
					if amtCents, ok := parseAmount(line[len(uncalledPfx):closeIdx]); ok {
						add(&handUncalledCents, amtCents)
					}
				}

			case strings.HasPrefix(line, collectedPfx):
				if amtCents, ok := parseAmount(line[len(collectedPfx):]); ok {
					add(&handCollectedCents, amtCents)
				}

			case strings.HasPrefix(line, "Total pot ") && strings.Contains(line, "| Rake $"):
				rakeIdx := strings.Index(line, "| Rake $")
				if rakeCents, ok := parseAmount(line[rakeIdx+8:]); ok {
					handRakeCents = rakeCents
				}
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				return nil, nil, nil, fmt.Errorf("scanning %s: %w", path, readErr), true
			}
			break
		}
	}

	finalizeHand()

	if amountErr != nil {
		return nil, nil, nil, fmt.Errorf("accumulating hand amounts: %w", amountErr), true
	}
	if st.hands == 0 {
		return nil, nil, nil, fmt.Errorf("no dealt hands found in %s", path), true
	}

	newState := &ParseState{agg: committed, consumedOffset: newConsumedOffset}

	if st.isTournament {
		if st.tournamentID == "" {
			return nil, nil, nil, fmt.Errorf("could not extract tournament ID from %s", path), true
		}
		return nil, &TournamentHandStats{
			TournamentID: st.tournamentID,
			VPIP:         float64(st.vpipCount) / float64(st.hands) * 100,
			PFR:          float64(st.pfrCount) / float64(st.hands) * 100,
			Hands:        st.hands,
			BountyCents:  st.bountyCents,
			IsKnockout:   st.isKnockout,
		}, newState, nil, true
	}

	dur := int(st.lastTime.Sub(st.firstTime).Minutes())

	var bb100 float64
	if st.bigBlindCents > 0 {
		bb100 = money.Ratio(st.netCents, st.bigBlindCents) / float64(st.hands) * 100
	}

	buyInCents, err := money.Multiply(st.bigBlindCents, 100)
	if err != nil {
		return nil, nil, nil, err, true
	}
	return &CashSession{
		Date:        st.firstTime,
		Stake:       st.stake,
		BuyInCents:  buyInCents,
		Hands:       st.hands,
		DurationMin: dur,
		NetCents:    st.netCents,
		RakeCents:   st.rakeCents,
		BB100:       bb100,
		VPIP:        float64(st.vpipCount) / float64(st.hands) * 100,
		PFR:         float64(st.pfrCount) / float64(st.hands) * 100,
		FirstHandID: st.firstHandID,
	}, nil, newState, nil, true
}

// isHandHeader reports whether line is a PokerStars hand header (cash or Zoom
// or tournament — all start with one of these two prefixes).
func isHandHeader(line string) bool {
	return strings.HasPrefix(line, "PokerStars Hand #") || strings.HasPrefix(line, "PokerStars Zoom Hand #")
}

// trimLineEnding strips the trailing "\n" and a single preceding "\r" from a
// raw line, matching bufio.Scanner's ScanLines so byte-offset-tracked reads
// produce the same line text the previous Scanner-based parser saw.
func trimLineEnding(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}

// extractHandID returns the digits between "Hand #" and the following ":" in
// a PokerStars hand header line (cash or Zoom — both contain "Hand #").
func extractHandID(line string) string {
	idx := strings.Index(line, "Hand #")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len("Hand #"):]
	end := strings.Index(rest, ":")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// parseLeadingAmount parses a leading decimal number from s, ignoring a
// leading '$'. Thousands-separator commas are consumed and stripped — but
// only when a digit follows, so "$12,500.00" parses as 12500 while a
// sentence comma after the amount ("$1, and…") still ends the number.
// Without this, "Total Prize Pool: $12,500.00" silently parsed as 12 —
// corrupted money with no error and no user-facing signal.
func parseLeadingAmount(s string) (money.Cents, bool) {
	if len(s) > 0 && s[0] == '$' {
		s = s[1:]
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '.' || (c >= '0' && c <= '9') {
			i++
			continue
		}
		if c == ',' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return 0, false
	}
	vCents, err := money.Parse(strings.ReplaceAll(s[:i], ",", ""))
	return vCents, err == nil
}

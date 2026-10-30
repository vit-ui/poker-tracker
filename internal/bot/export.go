package bot

// The /export command: every row of the three data sets as CSV files sent
// through Telegram, byte-identical on both storage backends.

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

var (
	cashSessionsCSVHeader   = []string{"Date", "Stake", "Hands", "Duration (min)", "Net (USD)", "Rake (USD)", "BB/100", "VPIP", "PFR", "Session ID"}
	tournamentsCSVHeader    = []string{"Date", "Type", "Tournament ID", "Buy-In (USD)", "Prize Pool (USD)", "Multiplier", "Finish Position", "Total Players", "Net (USD)", "VPIP", "PFR", "Bounty Won (USD)", "Prize Won (USD)", "Hands"}
	bankrollLedgerCSVHeader = []string{"Date", "Type", "Amount (USD)", "Running Total (USD)", "Session ID"}
)

// fullPrecision tells formatFloat/formatFloatPtr to render the shortest
// string that round-trips the value exactly, rather than a fixed decimal
// count — pass this for every numeric CSV column except VPIP/PFR.
const fullPrecision = -1

// vpipPfrPrecision is the fixed decimal count VPIP/PFR are rounded to.
// Both are raw-division results (e.g. 25.36231884057971) with no observable
// meaning beyond a couple of decimal places, unlike every other numeric CSV
// column, which keeps full precision.
const vpipPfrPrecision = 2

// bb100Precision is the fixed decimal count BB/100 is rounded to in CSV
// output, matching how Telegram already displays it ("%.1f" in
// cashStatsMessage, buildWeeklySummary, buildMonthlySummary).
const bb100Precision = 1

// formatFloat renders v to prec decimal places (fullPrecision for the
// shortest exact round-trip, a fixed count to round off meaningless
// precision).
func formatFloat(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// formatFloatPtr is formatFloat for a possibly-nil pointer (e.g. an MTT's
// Multiplier, or a tournament's VPIP/PFR before its hand file has arrived),
// rendering an empty cell for nil rather than a misleading "0".
func formatFloatPtr(v *float64, prec int) string {
	if v == nil {
		return ""
	}
	return formatFloat(*v, prec)
}

// formatIntPtr renders an empty cell for nil (a tournament's Hands, before
// its hand file has arrived) rather than a misleading "0".
func formatIntPtr(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

// prizePoolCSVCell coalesces Tournament's two mutually exclusive prize pool
// fields into the single cell the sheet itself stores — description text for
// a non-cash-denominated pool, otherwise the numeric amount. Without this,
// /export would either emit two columns or silently drop one, breaking the
// byte-identical output CLAUDE.md requires between backends (SQLite splits
// this into two nullable columns internally; the sheet never did).
func prizePoolCSVCell(t parsers.Tournament) string {
	if t.PrizePoolDescription != "" {
		return t.PrizePoolDescription
	}
	return t.PrizePoolCents.String()
}

func cashSessionsCSVRows(sessions []storage.CashSessionRecord) [][]string {
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, []string{
			s.Date.UTC().Format(time.RFC3339),
			s.Stake,
			strconv.Itoa(s.Hands),
			strconv.Itoa(s.DurationMin),
			s.NetCents.String(),
			s.RakeCents.String(),
			formatFloat(s.BB100, bb100Precision),
			formatFloat(s.VPIP, vpipPfrPrecision),
			formatFloat(s.PFR, vpipPfrPrecision),
			s.SessionID,
		})
	}
	return rows
}

func tournamentsCSVRows(tournaments []storage.TournamentRecord) [][]string {
	rows := make([][]string, 0, len(tournaments))
	for _, t := range tournaments {
		rows = append(rows, []string{
			t.Date.UTC().Format(time.RFC3339),
			t.Type,
			t.TournamentID,
			t.BuyInCents.String(),
			prizePoolCSVCell(t.Tournament),
			formatFloatPtr(t.Multiplier, fullPrecision),
			strconv.Itoa(t.FinishPos),
			strconv.Itoa(t.TotalPlayers),
			t.NetCents.String(),
			formatFloatPtr(t.VPIP, vpipPfrPrecision),
			formatFloatPtr(t.PFR, vpipPfrPrecision),
			t.BountyWonCents.String(),
			t.PrizeWonCents.String(),
			formatIntPtr(t.Hands),
		})
	}
	return rows
}

func bankrollLedgerCSVRows(entries []storage.LedgerEntry) [][]string {
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []string{
			e.Date.UTC().Format(time.RFC3339),
			e.Type,
			e.AmountCents.String(),
			e.RunningTotalCents.String(),
			e.SessionID,
		})
	}
	return rows
}

// writeCSVFile writes header followed by rows to path, using encoding/csv.
func writeCSVFile(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return fmt.Errorf("writing header to %s: %w", path, err)
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return fmt.Errorf("writing row to %s: %w", path, err)
		}
	}
	w.Flush()
	return w.Error()
}

// handleExportCommand implements /export: queries every row of all three data
// sets via storage.Store — never a backend-specific type, so the command is
// identical on both backends — writes each to a CSV file in os.TempDir(),
// sends each as a Telegram document, then deletes the temp files. An empty
// data set still sends a header-only CSV. Dates are RFC3339 UTC on both
// backends: the sqlite backend already stores them that way, and the sheets
// backend's Date field is parsed back from its internal
// "2006-01-02 15:04" format by GetAllCashSessions/GetAllTournaments before
// ever reaching here, so formatting with time.RFC3339 here is enough to keep
// output byte-identical between backends.
func (b *Bot) handleExportCommand(chatID int64) {
	cashSessions, err := b.store.GetAllCashSessions()
	if err != nil {
		slog.Error("getting cash sessions for export", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	tournaments, err := b.store.GetAllTournaments()
	if err != nil {
		slog.Error("getting tournaments for export", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	ledger, err := b.store.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		slog.Error("getting ledger entries for export", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	exports := []struct {
		filename string
		header   []string
		rows     [][]string
	}{
		{"cash_sessions.csv", cashSessionsCSVHeader, cashSessionsCSVRows(cashSessions)},
		{"tournaments.csv", tournamentsCSVHeader, tournamentsCSVRows(tournaments)},
		{"bankroll_ledger.csv", bankrollLedgerCSVHeader, bankrollLedgerCSVRows(ledger)},
	}

	var paths []string
	for _, e := range exports {
		path := filepath.Join(os.TempDir(), e.filename)
		if err := writeCSVFile(path, e.header, e.rows); err != nil {
			slog.Error("writing export csv", "file", e.filename, "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			for _, p := range paths {
				os.Remove(p)
			}
			return
		}
		paths = append(paths, path)
	}

	for _, p := range paths {
		if _, err := b.tg.Send(tgbotapi.NewDocument(chatID, tgbotapi.FilePath(p))); err != nil {
			slog.Error("sending export file", "file", p, "err", err)
		}
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			slog.Error("removing export temp file", "file", p, "err", err)
		}
	}

	send(b.tg, chatID, "✅ Export complete — 3 files.")
}

// handleScanCommand implements /scan: it triggers a one-shot backfill that
// walks both watched folders and records every cash session and tournament not
// already stored — useful for a new install seeded from existing history. The
// walk and the recording happen on the collector and server (see
// Collector.rescan / Server.importUpload); the bot only asks. It does not check
// the folder paths here: in the split build the collector's config (not the
// server's, which the bot reads) holds them, so the collector is the one place
// that can guard — an unconfigured install no-ops there with a log line. The
// completion report (with counts) arrives later via alertChan.
func (b *Bot) handleScanCommand(chatID int64) {
	b.requestRescan()
	send(b.tg, chatID, "📥 Scanning your folders for anything not yet recorded — I'll report what I record when it's done.")
}

package bot

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

const fullPrecision = -1

const vpipPfrPrecision = 2

const bb100Precision = 1

func formatFloat(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}

func formatFloatPtr(v *float64, prec int) string {
	if v == nil {
		return ""
	}
	return formatFloat(*v, prec)
}

func formatIntPtr(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

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

func (b *Bot) handleScanCommand(chatID int64) {
	if b.fileScanActive {
		send(b.tg, chatID, "A file scan is already in progress.")
		return
	}
	b.requestRescan()
	send(b.tg, chatID, "📥 Scanning your folders for anything not yet recorded — I'll report what I record when it's done.")
}

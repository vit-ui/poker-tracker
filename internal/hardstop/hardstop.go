// Package hardstop implements the hard stop trigger: detecting loss conditions,
// killing PokerStars, and writing/reading the lockfile.
package hardstop

import (
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// Evaluate checks hard stop conditions after a cash session and triggers a
// hard stop if any condition is met. Implemented in step 6 of the build order.
func Evaluate(session parsers.CashSession, sc *sheets.Client, configPath, lockfilePath string, alertChan chan<- string) {
}

// EvaluateTournament checks hard stop conditions after a tournament. Filters
// by Type == "spingo" internally when checking the daily loss threshold.
// Implemented in step 6 of the build order.
func EvaluateTournament(t parsers.Tournament, sc *sheets.Client, configPath, lockfilePath string, alertChan chan<- string) {
}

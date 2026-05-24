// Command moodle-fsrs is a v0.1 reference example.
//
// It exposes POST /reviews and pipes the request through a single LLM agent
// step that calls a stub FSRS update tool and returns a typed Decision JSON
// response. The FSRS math here is intentionally a placeholder; it is NOT a
// real spaced-repetition scheduler.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

// ReviewEntry is one row of the prior review history for a card.
type ReviewEntry struct {
	At     string `json:"at"`
	Rating int    `json:"rating"`
}

// Decision is the typed result produced by the Pipe.
//
// FSRS produces a next due date, an updated memory stability, an updated
// difficulty, and a lapse counter. We expose those fields here so the
// example mirrors the shape a production scheduler would return.
type Decision struct {
	NextDue    string  `json:"next_due"`
	Stability  float64 `json:"stability"`
	Difficulty float64 `json:"difficulty"`
	Lapses     int     `json:"lapses"`
}

// computeFSRSArgs is the Go tool input. The model passes the rating and the
// prior history; the tool returns a plausible Decision struct.
type computeFSRSArgs struct {
	CardID  string        `json:"card_id"`
	Rating  int           `json:"rating"`
	History []ReviewEntry `json:"history"`
}

// ComputeFSRS is a Go tool that runs a stub FSRS update.
//
// v0.1 reference example: this function does NOT implement real FSRS math.
// It returns a plausible Decision so the pipeline shape is exercised end to
// end. A production worker would call into an FSRS library or its own
// scheduler service here.
func ComputeFSRS(ctx context.Context, args computeFSRSArgs) (Decision, error) {
	if args.CardID == "" {
		return Decision{}, fmt.Errorf("card_id is required")
	}
	if args.Rating < 0 || args.Rating > 3 {
		return Decision{}, fmt.Errorf("rating must be in [0,3], got %d", args.Rating)
	}

	// Stub heuristic: higher rating extends the interval, lower rating
	// shortens it and increments lapses. Not real FSRS.
	intervalDays := []int{1, 3, 7, 14}[args.Rating]
	lapses := 0
	for _, entry := range args.History {
		if entry.Rating == 0 {
			lapses++
		}
	}
	if args.Rating == 0 {
		lapses++
	}

	stability := float64(intervalDays) + float64(len(args.History))*0.5
	difficulty := 5.0 - float64(args.Rating)
	nextDue := time.Now().UTC().AddDate(0, 0, intervalDays).Format("2006-01-02")

	return Decision{
		NextDue:    nextDue,
		Stability:  stability,
		Difficulty: difficulty,
		Lapses:     lapses,
	}, nil
}

func main() {
	if err := ovr.Run(":8080",
		ovr.From("POST /reviews"),
		ovr.Pipe("Update the FSRS schedule for the reviewed card.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Tool("compute_fsrs", ComputeFSRS,
				ovr.ReadOnly(),
				ovr.Describe("Run a stub FSRS update for a card review."),
				ovr.Param("card_id", "Card identifier from the request body."),
				ovr.Param("rating", "Review rating in [0,3] (0=again, 3=easy)."),
				ovr.Param("history", "Prior review entries with at and rating."),
			),
			ovr.Output[Decision](),
		),
		ovr.Reply(ovr.JSON[Decision]()),
	); err != nil {
		log.Fatal(err)
	}
}

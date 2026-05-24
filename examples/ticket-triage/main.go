// Command ticket-triage is the canonical Ouvrier v0.1 reference example.
//
// It exposes POST /tickets/{id} and pipes the request through a single LLM
// agent step that loads the ticket via a Go tool and returns a typed Triage
// JSON response.
package main

import (
	"context"
	"log"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

// Ticket is the in-memory record returned by the load_ticket tool.
type Ticket struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Triage is the typed result produced by the Pipe.
type Triage struct {
	Priority string   `json:"priority"`
	Summary  string   `json:"summary"`
	Tags     []string `json:"tags"`
}

// LoadTicket is a stub Go tool that returns a fixed Ticket payload.
// A production worker would call into the owning system here.
func LoadTicket(ctx context.Context, args struct {
	ID string `json:"id"`
}) (Ticket, error) {
	return Ticket{
		ID:      args.ID,
		Subject: "Login issue",
		Body:    "User cannot sign in to the dashboard after the latest deploy.",
	}, nil
}

func main() {
	if err := ovr.Run(":8080",
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Skill("ticket-triage"),
			ovr.Tool("load_ticket", LoadTicket,
				ovr.ReadOnly(),
				ovr.Describe("Load one support ticket by ID."),
				ovr.Param("id", "Ticket identifier from the request path."),
			),
			ovr.Output[Triage](),
		),
		ovr.Reply(ovr.JSON[Triage]()),
	); err != nil {
		log.Fatal(err)
	}
}

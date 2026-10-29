// Package letters issues the letter once slots are held. A poison reference is committed as rejected.
package referrals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-clinic/internal/platform/httpx"
	"github.com/joshbarros/golang-clinic/internal/platform/messaging"
	"github.com/joshbarros/golang-clinic/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "referrals"
	ConsumerName = "referrals"
)

// IssueOutcome is what issuing did.
type IssueOutcome int

// The ways issuing can end.
const (
	Issued IssueOutcome = iota
	Rejected
	Duplicate
)

// Service is the letters module's logic.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "referrals"}, Log: log}
}

// Handle implements messaging.Handler. A rejected letter returns ErrPoison after its rejection is committed, so the
// message is dead-lettered instead of retried.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	if d.RoutingKey != messaging.HeldKey {
		return fmt.Errorf("letters does not handle %q", d.RoutingKey)
	}
	held, err := messaging.Parse[messaging.SlotHeld](d.Body)
	if err != nil {
		return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
	}
	outcome, err := s.Issue(ctx, d.MessageID, held)
	if err != nil {
		return err
	}
	if outcome == Rejected {
		return fmt.Errorf("%w: the reference carries the poison token; the rejection is committed", messaging.ErrPoison)
	}
	return nil
}

// Issue issues the letter, or rejects it when the reference is poison, in one transaction with the inbox letter.
func (s *Service) Issue(ctx context.Context, messageID uuid.UUID, held messaging.SlotHeld) (IssueOutcome, error) {
	outcome := Issued
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		if messaging.IsPoison(held.Reference) {
			const reason = "Reference contains the poison token."
			outcome = Rejected
			if _, err := tx.Exec(ctx, `INSERT INTO referrals.letters (appointment_id, owner_id, status, reason, created_at) VALUES ($1,$2,'Rejected',$3,now())`,
				held.AppointmentID, held.OwnerID, reason); err != nil {
				return err
			}
			if err := s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, held.AppointmentID, messaging.ReleaseSlots{AppointmentID: held.AppointmentID, Reason: "poison"}); err != nil {
				return err
			}
			return s.Outbox.Enqueue(ctx, tx, messaging.RejectedKey, held.AppointmentID, messaging.LetterRejected{AppointmentID: held.AppointmentID, Reason: reason})
		}
		id := strings.ReplaceAll(held.AppointmentID.String(), "-", "")
		number := "RL-" + strings.ToUpper(id[len(id)-12:])
		if _, err := tx.Exec(ctx, `INSERT INTO referrals.letters (appointment_id, owner_id, number, status, created_at) VALUES ($1,$2,$3,'Issued',now())`,
			held.AppointmentID, held.OwnerID, number); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.IssuedKey, held.AppointmentID, messaging.LetterIssued{AppointmentID: held.AppointmentID, Number: number})
	})
	return outcome, err
}

// Letter is the public view of a letter.
type Letter struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	Number        *string   `json:"number"`
	Status        string    `json:"status"`
	Reason        *string   `json:"reason"`
	CreatedAt     time.Time `json:"createdAt"`
}

// API is the letters HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers GET /api/letters/{appointmentId}: the owner or staff only; anyone else gets a 404.
func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/letters/{appointmentId}", httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, a.get)))
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(r.PathValue("appointmentId"))
	if err != nil {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	var c Letter
	var owner uuid.UUID
	err = a.Pool.QueryRow(r.Context(), `SELECT appointment_id, owner_id, number, status, reason, created_at FROM referrals.letters WHERE appointment_id = $1`, id).
		Scan(&c.AppointmentID, &owner, &c.Number, &c.Status, &c.Reason, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !httpx.PrincipalOf(r.Context()).CanAccess(owner)) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, c)
}

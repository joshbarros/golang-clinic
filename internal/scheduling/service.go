package scheduling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-clinic/internal/platform/messaging"
	"github.com/joshbarros/golang-clinic/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "scheduling"
	ConsumerName = "scheduling"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	Duplicate
)

// Service is the sessions module's logic. It is the only writer of remaining_slots.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "scheduling"}, Log: log}
}

// Handle implements messaging.Handler for the sessions queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReserveSlot](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleaseSlots](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("sessions does not handle %q", d.RoutingKey)
}

// Reserve holds slots with one conditional UPDATE that matches only while the remaining slots still cover the request: when
// two appointments fight for the last slot, one commits a hold and the other is short and writes none.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReserveSlot) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE scheduling.sessions SET remaining_slots = remaining_slots - $2 WHERE id = $1 AND remaining_slots >= $2`, cmd.SessionID, cmd.Slots)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `INSERT INTO scheduling.slot_holds (appointment_id, owner_id, session_id, slots, status, created_at) VALUES ($1,$2,$3,$4,'Held',now())`,
				cmd.AppointmentID, cmd.OwnerID, cmd.SessionID, cmd.Slots); err != nil {
				return err
			}
			return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.AppointmentID,
				messaging.SlotHeld{AppointmentID: cmd.AppointmentID, OwnerID: cmd.OwnerID, SessionID: cmd.SessionID, Slots: cmd.Slots, Reference: cmd.Reference})
		}
		outcome = Short
		var left int
		_ = tx.QueryRow(ctx, `SELECT remaining_slots FROM scheduling.sessions WHERE id = $1`, cmd.SessionID).Scan(&left)
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.AppointmentID,
			messaging.SlotShort{AppointmentID: cmd.AppointmentID, Reference: cmd.Reference, RequestedSlots: cmd.Slots, RemainingSlots: left})
	})
	return outcome, err
}

// Release gives held slots back exactly once. A second release for the same appointment changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleaseSlots) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var sessionID uuid.UUID
		var slots int
		err = tx.QueryRow(ctx, `
			UPDATE scheduling.slot_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE appointment_id = $1 AND status = 'Held' RETURNING session_id, slots`, cmd.AppointmentID, cmd.Reason).Scan(&sessionID, &slots)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE scheduling.sessions SET remaining_slots = remaining_slots + $2 WHERE id = $1 AND remaining_slots + $2 <= capacity_slots`, sessionID, slots); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.AppointmentID, messaging.SlotReleased{AppointmentID: cmd.AppointmentID, Slots: slots, Reason: cmd.Reason})
	})
	return released, err
}

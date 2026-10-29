package appointments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joshbarros/golang-clinic/internal/platform/messaging"
	"github.com/joshbarros/golang-clinic/internal/platform/pgdb"
)

// MarkOutcome is what a mark did.
type MarkOutcome int

// The ways a mark can end.
const (
	Marked MarkOutcome = iota
	Skipped
	Duplicate
)

// Handle consumes the events that move an appointment forward. Each step runs only from the status it expects.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	poison := func(err error) error { return fmt.Errorf("%w: %v", messaging.ErrPoison, err) }
	switch d.RoutingKey {
	case messaging.HeldKey:
		m, err := messaging.Parse[messaging.SlotHeld](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkHeld(ctx, d.MessageID, m.AppointmentID)
		return err
	case messaging.ShortKey:
		m, err := messaging.Parse[messaging.SlotShort](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkShort(ctx, d.MessageID, m)
		return err
	case messaging.IssuedKey:
		m, err := messaging.Parse[messaging.LetterIssued](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkConfirmed(ctx, d.MessageID, m, s.FinalMarkDelay)
		return err
	case messaging.RejectedKey:
		m, err := messaging.Parse[messaging.LetterRejected](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkRejected(ctx, d.MessageID, m)
		return err
	case messaging.ReleasedKey:
		m, err := messaging.Parse[messaging.SlotReleased](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkReleased(ctx, d.MessageID, m)
		return err
	}
	return fmt.Errorf("appointments does not handle %q", d.RoutingKey)
}

// step runs update inside one transaction with the inbox letter. When update changes nothing and the appointment is still waiting
// for an earlier mark (Requested), the message arrived too early: it is retried, not dropped.
func (s *Service) step(ctx context.Context, messageID uuid.UUID, appointmentForAppointmentCheck *uuid.UUID, update func(pgx.Tx) (int64, error)) (MarkOutcome, error) {
	outcome := Marked
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		changed, err := update(tx)
		if err != nil {
			return err
		}
		if changed == 0 && appointmentForAppointmentCheck != nil {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM appointments.appointments WHERE id = $1`, *appointmentForAppointmentCheck).Scan(&status)
			if err == nil && status == Requested {
				return &messaging.OutOfAppointmentError{Reason: "The final mark arrived before the Held mark."}
			}
		}
		if changed == 0 {
			outcome = Skipped
		}
		return nil
	})
	return outcome, err
}

// MarkHeld: the slots are held.
func (s *Service) MarkHeld(ctx context.Context, messageID, appointmentID uuid.UUID) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE appointments.appointments SET status = 'Held', reason = NULL, updated_at = now() WHERE id = $1 AND status = 'Requested'`, appointmentID)
		return tag.RowsAffected(), err
	})
}

// ShortReason words why the reserve was refused.
func ShortReason(m messaging.SlotShort) string {
	return fmt.Sprintf("Short: %d slots requested, %d remaining.", m.RequestedSlots, m.RemainingSlots)
}

// MarkShort: the stock said no.
func (s *Service) MarkShort(ctx context.Context, messageID uuid.UUID, m messaging.SlotShort) (MarkOutcome, error) {
	reason := ShortReason(m)
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE appointments.appointments SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status = 'Requested'`, m.AppointmentID, reason)
		return tag.RowsAffected(), err
	})
}

// MarkConfirmed is the final mark. If a refund got there first the row is no longer Held and nothing changes.
func (s *Service) MarkConfirmed(ctx context.Context, messageID uuid.UUID, m messaging.LetterIssued, delay time.Duration) (MarkOutcome, error) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Skipped, ctx.Err()
		}
	}
	return s.step(ctx, messageID, &m.AppointmentID, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE appointments.appointments SET status = 'Confirmed', letter_number = $2, updated_at = now() WHERE id = $1 AND status = 'Held'`, m.AppointmentID, m.Number)
		return tag.RowsAffected(), err
	})
}

// MarkRejected: the letter was refused (poison).
func (s *Service) MarkRejected(ctx context.Context, messageID uuid.UUID, m messaging.LetterRejected) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE appointments.appointments SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status IN ('Held','Requested')`, m.AppointmentID, m.Reason)
		return tag.RowsAffected(), err
	})
}

// MarkReleased records that the slots came back.
func (s *Service) MarkReleased(ctx context.Context, messageID uuid.UUID, m messaging.SlotReleased) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE appointments.appointments SET slots_released_at = now() WHERE id = $1 AND slots_released_at IS NULL`, m.AppointmentID)
		return tag.RowsAffected(), err
	})
}

var _ = errors.New

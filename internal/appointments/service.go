package appointments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-clinic/internal/platform/httpx"
	"github.com/joshbarros/golang-clinic/internal/platform/messaging"
	"github.com/joshbarros/golang-clinic/internal/platform/pgdb"
	"github.com/joshbarros/golang-clinic/internal/scheduling"
)

// Schema and consumer names.
const (
	Schema       = "appointments"
	ConsumerName = "appointments"
)

// Appointment statuses.
const (
	Requested = "Requested"
	Held      = "Held"
	Confirmed = "Confirmed"
	Cancelled = "Cancelled"
	Rejected  = "Rejected"
)

// Appointment is the public view of an appointment.
type Appointment struct {
	ID             uuid.UUID  `json:"id"`
	OwnerID        uuid.UUID  `json:"ownerId"`
	SessionCode    string     `json:"sessionCode"`
	Reference      string     `json:"reference"`
	Slots          int        `json:"slots"`
	FeeCents       int64      `json:"feeCents"`
	Status         string     `json:"status"`
	Reason         *string    `json:"reason"`
	LetterNumber   *string    `json:"letterNumber"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	SlotReleasedAt *time.Time `json:"slotsReleasedAt"`
}

// SessionCatalog is the one thing Appointments may ask the sessions module.
type SessionCatalog interface {
	Find(ctx context.Context, code string) (*scheduling.Info, error)
}

// Service is the HTTP side of the saga: it writes the appointment and its first outbox row in one local transaction.
type Service struct {
	Pool     *pgxpool.Pool
	Outbox   messaging.Outbox
	Sessions SessionCatalog
	Log      *slog.Logger

	// FinalMarkDelay is a latency knob for the final mark: Kubernetes sets it so the refund race window is wide enough to hit on purpose.
	FinalMarkDelay time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, catalog SessionCatalog, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "appointments"}, Sessions: catalog, Log: log}
}

const appointmentColumns = `id, owner_id, session_code, reference, slots, fee_cents, status, reason, letter_number, created_at, updated_at, slots_released_at`

func scanAppointment(row pgx.Row) (Appointment, error) {
	var o Appointment
	err := row.Scan(&o.ID, &o.OwnerID, &o.SessionCode, &o.Reference, &o.Slots, &o.FeeCents, &o.Status, &o.Reason, &o.LetterNumber, &o.CreatedAt, &o.UpdatedAt, &o.SlotReleasedAt)
	return o, err
}

// CreateRequest is the body of POST /api/appointments.
type CreateRequest struct {
	SessionCode string `json:"sessionCode"`
	Reference   string `json:"reference"`
	Slots       int64  `json:"slots"`
}

// Create reserves slots in the session for the caller: validates, writes the appointment and its reserve command in one transaction.
func (s *Service) Create(ctx context.Context, caller *httpx.Principal, req CreateRequest) (Appointment, error) {
	if caller == nil {
		return Appointment{}, httpx.Fail(http.StatusUnauthorized, "Sign in to book.")
	}
	if strings.TrimSpace(req.SessionCode) == "" {
		return Appointment{}, httpx.Fail(http.StatusBadRequest, "Session code is required.")
	}
	if msg := Validate(req.Reference, req.Slots); msg != "" {
		return Appointment{}, httpx.Fail(http.StatusBadRequest, "%s", msg)
	}
	session, err := s.Sessions.Find(ctx, strings.TrimSpace(req.SessionCode))
	if err != nil {
		return Appointment{}, err
	}
	if session == nil {
		return Appointment{}, httpx.Fail(http.StatusNotFound, "Session '%s' does not exist.", req.SessionCode)
	}
	now := time.Now().UTC()
	slots := int(req.Slots)
	reference := strings.TrimSpace(req.Reference)
	fee, err := FeeCents(slots)
	if err != nil {
		return Appointment{}, httpx.Fail(http.StatusBadRequest, "The fee is too large.")
	}
	id, _ := uuid.NewV7()
	o := Appointment{ID: id, OwnerID: caller.ID, SessionCode: session.Code, Reference: reference, Slots: slots, FeeCents: fee, Status: Requested, CreatedAt: now, UpdatedAt: now}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO appointments.appointments (id, owner_id, session_id, session_code, reference, slots, fee_cents, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'Requested',$8,$8)`, o.ID, o.OwnerID, session.ID, o.SessionCode, o.Reference, o.Slots, o.FeeCents, now); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReserveKey, o.ID,
			messaging.ReserveSlot{AppointmentID: o.ID, OwnerID: caller.ID, SessionID: session.ID, Slots: slots, Reference: reference})
	})
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return Appointment{}, httpx.Fail(http.StatusConflict, "Reference '%s' is already booked on %s.", reference, session.Code)
		}
		return Appointment{}, err
	}
	return o, nil
}

// Get returns an appointment to its owner or to staff. Anyone else gets a 404, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Appointment, error) {
	o, err := scanAppointment(s.Pool.QueryRow(ctx, `SELECT `+appointmentColumns+` FROM appointments.appointments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !caller.CanAccess(o.OwnerID)) {
		return Appointment{}, httpx.Fail(http.StatusNotFound, "Appointment not found.")
	}
	return o, err
}

// List returns the caller's appointments (staff: everyone's), newest first, a page at a time, with the total.
func (s *Service) List(ctx context.Context, caller *httpx.Principal, sessionCode string, page, pageSize int) ([]Appointment, int, error) {
	if caller == nil {
		return nil, 0, httpx.Fail(http.StatusUnauthorized, "Sign in to see appointments.")
	}
	where := []string{"true"}
	args := []any{}
	if !caller.IsStaff() {
		args = append(args, caller.ID)
		where = append(where, "owner_id = $"+strconv.Itoa(len(args)))
	}
	if strings.TrimSpace(sessionCode) != "" {
		args = append(args, sessionCode)
		where = append(where, "session_code = $"+strconv.Itoa(len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM appointments.appointments WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM appointments.appointments WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		appointmentColumns, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Appointment{}
	for rows.Next() {
		o, err := scanAppointment(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
	}
	return items, total, rows.Err()
}

// Refund moves Held or Confirmed to Cancelled and asks Sessions to give the slots back. The letter is not touched.
func (s *Service) Refund(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Appointment, error) {
	if _, err := s.Get(ctx, caller, id); err != nil { // 404 unless the caller may see it
		return Appointment{}, err
	}
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE appointments.appointments SET status = 'Cancelled', reason = 'Cancelled by request.', updated_at = now()
			WHERE id = $1 AND status IN ('Held','Confirmed')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var current string
			if err := tx.QueryRow(ctx, `SELECT status FROM appointments.appointments WHERE id = $1`, id).Scan(&current); err != nil {
				return err
			}
			return httpx.Fail(http.StatusConflict, "An appointment in status %s cannot be cancelled.", current)
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, id, messaging.ReleaseSlots{AppointmentID: id, Reason: "refund"})
	})
	if err != nil {
		return Appointment{}, err
	}
	return scanAppointment(s.Pool.QueryRow(ctx, `SELECT `+appointmentColumns+` FROM appointments.appointments WHERE id = $1`, id))
}

package scheduling

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-clinic/internal/platform/httpx"
)

const maxCapacitySlots = 200

// Session is the public view of a session.
type Session struct {
	ID             uuid.UUID `json:"id"`
	Code           string    `json:"code"`
	Provider       string    `json:"provider"`
	Specialty      string    `json:"specialty"`
	Location       string    `json:"location"`
	StartsAt       time.Time `json:"startsAt"`
	CapacitySlots  int       `json:"capacitySlots"`
	RemainingSlots int       `json:"remainingSlots"`
}

// Info is what other modules may ask the sessions module (Appointments never reads this schema).
type Info struct {
	ID   uuid.UUID
	Code string
}

// Catalog answers the one cross-module read: a session's existence.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a session up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code FROM scheduling.sessions WHERE code = $1`, code).Scan(&i.ID, &i.Code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the sessions HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the session endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/sessions", authed(a.list))
	mux.HandleFunc("GET /api/sessions/{code}", authed(a.get))
	mux.HandleFunc("POST /api/sessions", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const sessionSelect = `SELECT id, code, provider, specialty, location, starts_at, capacity_slots, remaining_slots FROM scheduling.sessions`

func scanSession(row pgx.Row) (Session, error) {
	var d Session
	err := row.Scan(&d.ID, &d.Code, &d.Provider, &d.Specialty, &d.Location, &d.StartsAt, &d.CapacitySlots, &d.RemainingSlots)
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), sessionSelect+` ORDER BY starts_at, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		d, err := scanSession(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanSession(a.Pool.QueryRow(r.Context(), sessionSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code          string     `json:"code"`
	Provider      string     `json:"provider"`
	Specialty     string     `json:"specialty"`
	Location      string     `json:"location"`
	StartsAt      *time.Time `json:"startsAt"`
	CapacitySlots int64      `json:"capacitySlots"`
}

// create opens a session. Staff only. This is the only place remaining_slots is initialised; after that only the reserve and release
// handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Provider) == "" || strings.TrimSpace(req.Specialty) == "" || strings.TrimSpace(req.Location) == "" {
		return httpx.Fail(http.StatusBadRequest, "Provider, specialty and location are required.")
	}
	if req.CapacitySlots < 1 || req.CapacitySlots > maxCapacitySlots {
		return httpx.Fail(http.StatusBadRequest, "Capacity must be between 1 and %d slots.", maxCapacitySlots)
	}
	when := time.Now().AddDate(0, 0, 14)
	if req.StartsAt != nil {
		when = *req.StartsAt
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO scheduling.sessions (id, code, provider, specialty, location, starts_at, capacity_slots, remaining_slots, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,now())`,
		id, code, strings.TrimSpace(req.Provider), strings.TrimSpace(req.Specialty), strings.TrimSpace(req.Location), when, req.CapacitySlots)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Session '%s' already exists.", code)
		}
		return err
	}
	d, err := scanSession(a.Pool.QueryRow(r.Context(), sessionSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/sessions/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}

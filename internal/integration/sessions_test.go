package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-clinic/internal/platform/messaging"
	"github.com/joshbarros/golang-clinic/internal/scheduling"
)

func (e *env) rawSession(slots int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO scheduling.sessions (id, code, provider, specialty, location, starts_at, capacity_slots, remaining_slots, created_at)
		VALUES ($1,$2,'a','b','c',now(),$3,$3,now())`, id, code(), slots)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, session uuid.UUID, slots int) scheduling.ReserveOutcome {
	out, err := e.app.Sessions.Reserve(context.Background(), uuid.New(), messaging.ReserveSlot{AppointmentID: uuid.New(), OwnerID: uuid.New(), SessionID: session, Slots: slots, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	session := e.rawSession(30)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, session, 1) == scheduling.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT remaining_slots FROM scheduling.sessions WHERE id = $1`, session).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 appointments, 30 slots: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM scheduling.slot_holds WHERE session_id = $1`, session) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestAShortReserveWritesNoHoldAndTakesNothing(t *testing.T) {
	e := newEnv(t)
	session := e.rawSession(3)
	if reserve(e, session, 2) != scheduling.Held || reserve(e, session, 2) != scheduling.Short {
		t.Fatal("2 of 3 held, then 2 wanted and 1 left is short")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT remaining_slots FROM scheduling.sessions WHERE id = $1`, session).Scan(&left)
	if left != 1 || e.count(`SELECT count(*) FROM scheduling.slot_holds WHERE session_id = $1`, session) != 1 {
		t.Fatalf("the short try left no trace: %d left", left)
	}
	if reserve(e, session, 1) != scheduling.Held {
		t.Fatal("the last slot is still there")
	}
}

func TestReleaseGivesBackExactlyOnce(t *testing.T) {
	e := newEnv(t)
	session := e.rawSession(5)
	appointment := uuid.New()
	if out, _ := e.app.Sessions.Reserve(context.Background(), uuid.New(), messaging.ReserveSlot{AppointmentID: appointment, OwnerID: uuid.New(), SessionID: session, Slots: 2, Reference: "R"}); out != scheduling.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.app.Sessions.Release(context.Background(), uuid.New(), messaging.ReleaseSlots{AppointmentID: appointment, Reason: "refund"}); err != nil {
			t.Fatal(err)
		}
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT remaining_slots FROM scheduling.sessions WHERE id = $1`, session).Scan(&left)
	if left != 5 || e.outbox("scheduling", messaging.ReleasedKey) != 1 {
		t.Fatalf("given back once, not three times: %d left", left)
	}
}

func TestOpeningASessionValidatesIt(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := func() map[string]any {
		return map[string]any{"code": code(), "provider": "a", "specialty": "b", "location": "c", "capacitySlots": 10}
	}
	if r := e.post("/api/sessions", good(), adm.Token); r.Status != 201 || r.Body["remainingSlots"] != float64(10) {
		t.Fatalf("a session opens full: %d %s", r.Status, r.Raw)
	}
	for name, mut := range map[string]func(map[string]any){
		"no code":   func(m map[string]any) { m["code"] = "" },
		"long code": func(m map[string]any) { m["code"] = "ABCDEFGHIJKLMNOPQRSTU" },
		"no text":   func(m map[string]any) { m["specialty"] = " " },
		"zero":      func(m map[string]any) { m["capacitySlots"] = 0 },
		"too many":  func(m map[string]any) { m["capacitySlots"] = 100_000_000 },
	} {
		bad := good()
		mut(bad)
		if r := e.post("/api/sessions", bad, adm.Token); r.Status != 400 {
			t.Errorf("%s: %d", name, r.Status)
		}
	}
	dup := good()
	e.post("/api/sessions", dup, adm.Token)
	if r := e.post("/api/sessions", dup, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/sessions", good(), e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a session: %d", r.Status)
	}
}

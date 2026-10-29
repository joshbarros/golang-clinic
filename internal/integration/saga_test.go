package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-clinic/internal/appointments"
	"github.com/joshbarros/golang-clinic/internal/platform/messaging"
	"github.com/joshbarros/golang-clinic/internal/referrals"
)

func (e *env) outbox(schema, key string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+schema+`.outbox_messages WHERE routing_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) count(sql string, args ...any) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) remainingSlots(sessionCode string) float64 {
	e.t.Helper()
	v, _ := e.get("/api/sessions/"+sessionCode, e.admin().Token).Body["remainingSlots"].(float64)
	return v
}

func status(e *env, s session, id string) string { return e.appointment(s, id).str("status") }

func feeOf(t *testing.T, slots int) float64 {
	t.Helper()
	fee, err := appointments.FeeCents(slots)
	if err != nil {
		t.Fatal(err)
	}
	return float64(fee)
}

func TestTheWholeSagaFromAppointmentToConfirmedAndTheLetter(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(10)
	ana := e.register()
	created := e.enter(ana, session, "COL-1", 2)
	id := created.str("id")
	if created.Status != 202 || created.str("status") != "Requested" || created.Body["feeCents"] != feeOf(t, 2) {
		t.Fatalf("accepted as Requested with the fee: %s", created.Raw)
	}
	if dead := e.drain(); dead != 0 {
		t.Fatalf("nothing should dead-letter: %d", dead)
	}
	o := e.appointment(ana, id)
	if o.str("status") != "Confirmed" || !strings.HasPrefix(o.str("letterNumber"), "RL-") || o.Body["reason"] != nil {
		t.Fatalf("sold with a letter number and no leftover reason: %s", o.Raw)
	}
	if e.remainingSlots(session) != 8 {
		t.Fatal("2 of 10 slots are held")
	}
	c := e.get("/api/letters/"+id, ana.Token)
	if c.Status != 200 || c.str("status") != "Issued" || c.str("number") != o.str("letterNumber") {
		t.Fatalf("the letter: %d %s", c.Status, c.Raw)
	}
}

func TestWhenTwoAppointmentsFightForTheLastSlotsOneCommitsAndTheOtherIsShortAndWritesNoHold(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(3)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, session, "COL-A", 2).str("id")
	e.drain()
	b := e.enter(bob, session, "COL-B", 2).str("id")
	e.drain()
	if status(e, ana, a) != "Confirmed" {
		t.Fatal("the first appointment is sold")
	}
	o := e.appointment(bob, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Short: 2 slots requested, 1 remaining." {
		t.Fatalf("the second is short: %s", o.Raw)
	}
	if e.get("/api/letters/"+b, bob.Token).Status != 404 || e.count(`SELECT count(*) FROM scheduling.slot_holds`) != 1 {
		t.Fatal("no letter and no hold for a refused appointment")
	}
	if e.remainingSlots(session) != 1 {
		t.Fatal("the refused appointment took nothing")
	}
}

func TestPoisonIsRejectedAndTheSlotsComeBack(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana := e.register()
	id := e.enter(ana, session, "COL-POISON-1", 2).str("id")
	if dead := e.drain(); dead != 1 {
		t.Fatalf("the poison letter is dead-lettered once: %d", dead)
	}
	o := e.appointment(ana, id)
	if o.str("status") != "Rejected" || !strings.Contains(o.str("reason"), "poison") || o.Body["slotsReleasedAt"] == nil {
		t.Fatalf("rejected, with the reason, and the release recorded: %s", o.Raw)
	}
	if e.remainingSlots(session) != 5 {
		t.Fatal("the slots are back")
	}
	if c := e.get("/api/letters/"+id, ana.Token); c.str("status") != "Rejected" {
		t.Fatalf("the rejection is committed on the letter: %s", c.Raw)
	}
}

func TestRefundAfterSaleReturnsTheSlotsButKeepsTheLetter(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana := e.register()
	id := e.enter(ana, session, "COL-1", 2).str("id")
	e.drain()
	if r := e.post("/api/appointments/"+id+"/cancellation", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Cancelled" || e.remainingSlots(session) != 5 {
		t.Fatal("refunded and the slots are back on the shelf")
	}
	if c := e.get("/api/letters/"+id, ana.Token); c.str("status") != "Issued" {
		t.Fatal("a refund does not roll back the letter that was issued")
	}
	if r := e.post("/api/appointments/"+id+"/cancellation", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a second refund is a conflict: %d", r.Status)
	}
}

func TestRefundBeatsAFinalMarkThatArrivesLate(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, session, "COL-1", 1).str("id"))
	for pass := 0; pass < 4; pass++ {
		for _, row := range e.unsent() {
			if row.d.RoutingKey == messaging.IssuedKey {
				continue
			}
			e.markSent(row)
			_, _ = e.deliver(row.d.RoutingKey, row.d)
		}
	}
	if got := status(e, ana, id.String()); got != "Held" {
		t.Fatalf("held, letter issued, final mark not yet landed: %s", got)
	}
	if r := e.post("/api/appointments/"+id.String()+"/cancellation", nil, ana.Token); r.Status != 202 {
		t.Fatal(r.Status)
	}
	e.drain()
	o := e.appointment(ana, id.String())
	if o.str("status") != "Cancelled" || o.Body["letterNumber"] != nil {
		t.Fatalf("the late final mark must not undo the refund: %s", o.Raw)
	}
	if c := e.get("/api/letters/"+id.String(), ana.Token); c.str("status") != "Issued" {
		t.Fatal("the issued letter stays")
	}
}

func TestADeliveryRepeatedByTheBrokerHasNoEffectTheSecondTime(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana := e.register()
	e.enter(ana, session, "COL-1", 1)
	rows := e.unsent()
	if len(rows) != 1 {
		t.Fatal("one reserve command")
	}
	d := rows[0].d
	for i := 0; i < 3; i++ {
		if _, err := e.deliver(d.RoutingKey, d); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM scheduling.slot_holds`) != 1 || e.outbox("scheduling", messaging.HeldKey) != 1 || e.remainingSlots(session) != 4 {
		t.Fatal("at-least-once delivery, exactly-once effect: the inbox absorbs the repeats")
	}
}

func TestAFinalMarkBeforeTheHeldMarkIsRetriedNotSessionped(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, session, "COL-1", 1).str("id"))
	_, err := e.app.Appointments.MarkConfirmed(context.Background(), uuid.New(), messaging.LetterIssued{AppointmentID: id, Number: "RL-TEST"}, 0)
	var ooo *messaging.OutOfAppointmentError
	if err == nil || !errorsAs(err, &ooo) {
		t.Fatalf("the final mark must ask to be retried, got %v", err)
	}
	if status(e, ana, id.String()) != "Requested" {
		t.Fatal("and change nothing")
	}
	e.drain()
	if status(e, ana, id.String()) != "Confirmed" {
		t.Fatal("the normal flow still completes")
	}
}

func TestIssueIsIdempotentAndPoisonIsAFlagNotAnException(t *testing.T) {
	e := newEnv(t)
	id, owner := uuid.New(), uuid.New()
	msg := uuid.New()
	held := messaging.SlotHeld{AppointmentID: id, OwnerID: owner, SessionID: uuid.New(), Slots: 1, Reference: "COL-1"}
	first, err := e.app.Letters.Issue(context.Background(), msg, held)
	again, err2 := e.app.Letters.Issue(context.Background(), msg, held)
	if err != nil || err2 != nil || first != referrals.Issued || again != referrals.Duplicate {
		t.Fatalf("issue then duplicate: %v %v %v %v", first, err, again, err2)
	}
	poison := messaging.SlotHeld{AppointmentID: uuid.New(), OwnerID: owner, SessionID: uuid.New(), Slots: 1, Reference: "xPOISONx"}
	if out, _ := e.app.Letters.Issue(context.Background(), uuid.New(), poison); out != referrals.Rejected {
		t.Fatal("a poison reference is rejected")
	}
	if e.outbox("referrals", messaging.ReleaseKey) != 1 || e.outbox("referrals", messaging.RejectedKey) != 1 || e.outbox("referrals", messaging.IssuedKey) != 1 {
		t.Fatal("one issued, one release command and one rejection")
	}
}

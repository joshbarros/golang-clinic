package integration

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestEveryRouteNeedsASignedInUser(t *testing.T) {
	e := newEnv(t)
	id := "01a1831c-3d9d-72e6-8f15-269852212912"
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/auth/me"}, {"POST", "/api/auth/change-password"}, {"POST", "/api/auth/logout-everywhere"},
		{"GET", "/api/auth/mfa"}, {"POST", "/api/auth/mfa/setup"}, {"POST", "/api/auth/mfa/enable"}, {"POST", "/api/auth/mfa/disable"},
		{"GET", "/api/sessions"}, {"GET", "/api/sessions/X"}, {"POST", "/api/sessions"},
		{"GET", "/api/appointments"}, {"POST", "/api/appointments"}, {"GET", "/api/appointments/" + id}, {"POST", "/api/appointments/" + id + "/cancellation"}, {"GET", "/api/appointments/" + id + "/events"},
		{"GET", "/api/letters/" + id}, {"GET", "/api/admin/dead-letters"}, {"POST", "/api/admin/dead-letters/referrals.work/replay"},
	} {
		if r := e.do(call{method: c.method, path: c.path}); r.Status != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", c.method, c.path, r.Status)
		}
	}
	for _, path := range []string{"/api/health", "/api/health/ready"} {
		if e.get(path, "").Status != 200 {
			t.Errorf("%s is public", path)
		}
	}
}

func TestStaffRoutesRefuseCustomers(t *testing.T) {
	e := newEnv(t)
	customer := e.register()
	id := "01a1831c-3d9d-72e6-8f15-269852212912"
	if r := e.post("/api/sessions", map[string]any{"code": "X"}, customer.Token); r.Status != 403 {
		t.Errorf("a customer must not open sessions: %d", r.Status)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/appointments/" + id + "/events"}, {"GET", "/api/admin/dead-letters"}, {"POST", "/api/admin/dead-letters/referrals.work/replay"},
	} {
		if r := e.do(call{method: c.method, path: c.path, token: customer.Token}); r.Status != 403 {
			t.Errorf("%s %s as a customer: %d", c.method, c.path, r.Status)
		}
	}
	admin := e.admin()
	if r := e.get("/api/admin/dead-letters", admin.Token); r.Status != 503 {
		t.Errorf("with messaging off the staff get a clear 503, not a crash: %d", r.Status)
	}
}

func TestAnotherCustomersAppointmentIsNotFoundNotForbidden(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana, bob := e.register(), e.register()
	id := e.enter(ana, session, "COL-ANA", 1).str("id")
	e.drain()

	if r := e.appointment(ana, id); r.Status != 200 || r.str("status") != "Confirmed" {
		t.Fatalf("the owner sees it: %d %s", r.Status, r.Raw)
	}
	if r := e.appointment(bob, id); r.Status != 404 {
		t.Fatalf("someone else gets 404, so ids cannot be probed: %d", r.Status)
	}
	if r := e.post("/api/appointments/"+id+"/cancellation", nil, bob.Token); r.Status != 404 {
		t.Fatalf("someone else cannot refund it: %d", r.Status)
	}
	if r := e.get("/api/letters/"+id, bob.Token); r.Status != 404 {
		t.Fatalf("or read its letter: %d", r.Status)
	}
	if r := e.appointment(e.admin(), id); r.Status != 200 {
		t.Fatalf("staff see everything: %d", r.Status)
	}
	if r := e.get("/api/appointments/not-a-guid", ana.Token); r.Status != 404 {
		t.Fatalf("a malformed id is a 404: %d", r.Status)
	}
}

func TestTheAppointmentIsOwnedByTheCallerNotByWhatTheBodySays(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	ana, bob := e.register(), e.register()
	r := e.post("/api/appointments", map[string]any{"sessionCode": session, "reference": "COL-X", "slots": 1, "ownerId": bob.ID.String()}, ana.Token)
	if r.Status != 202 || r.str("ownerId") != ana.ID.String() {
		t.Fatalf("the owner is always the caller: %d %s", r.Status, r.Raw)
	}
}

func TestListShowsACustomerOnlyTheirOwnAppointmentsAndPagesNewestFirst(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(50)
	ana, bob := e.register(), e.register()
	refs := []string{"A-1", "A-2", "A-3", "A-4", "A-5"}
	// the same person may hold several appointments at the door; only the reference must be unique
	for _, ref := range refs {
		if r := e.enter(ana, session, ref, 1); r.Status != 202 {
			t.Fatalf("%s: %d %s", ref, r.Status, r.Raw)
		}
		time.Sleep(3 * time.Millisecond)
	}
	e.enter(bob, session, "B-1", 1)

	page := e.do(call{method: "GET", path: "/api/appointments?pageSize=2&page=2", token: ana.Token})
	var items []map[string]any
	_ = jsonInto(page.Raw, &items)
	if page.Header.Get("X-Total-Count") != "5" || len(items) != 2 || items[0]["reference"] != "A-3" || items[1]["reference"] != "A-2" {
		t.Fatalf("page 2 of 5 newest first: total %s, %v", page.Header.Get("X-Total-Count"), items)
	}
	all := e.do(call{method: "GET", path: "/api/appointments?sessionCode=" + session, token: e.admin().Token})
	if n, _ := strconv.Atoi(all.Header.Get("X-Total-Count")); n != 6 {
		t.Fatalf("staff see everyone's: %d", n)
	}
	if r := e.do(call{method: "GET", path: "/api/appointments?pageSize=100000", token: ana.Token}); r.Status != 200 {
		t.Fatal("an absurd page size is clamped, not an error")
	}
}

func TestValidationAtTheDoor(t *testing.T) {
	e := newEnv(t)
	session := e.openSession(5)
	u := e.register()
	cases := map[string]map[string]any{
		"no reference": {"sessionCode": session, "reference": "", "slots": 1},
		"zero slots":   {"sessionCode": session, "reference": "R", "slots": 0},
		"too many":     {"sessionCode": session, "reference": "R", "slots": 5},
		"would wrap":   {"sessionCode": session, "reference": "R", "slots": 4294967297},
		"huge":         {"sessionCode": session, "reference": "R", "slots": 9223372036854775807},
		"no session":   {"reference": "R", "slots": 1},
	}
	for name, body := range cases {
		if r := e.post("/api/appointments", body, u.Token); r.Status != 400 {
			t.Errorf("%s: %d %s", name, r.Status, r.Raw)
		}
	}
	if r := e.post("/api/appointments", map[string]any{"sessionCode": "NOPE", "reference": "R", "slots": 1}, u.Token); r.Status != 404 {
		t.Errorf("unknown session: %d", r.Status)
	}
	if r := e.enter(u, session, "R", 1); r.Status != 202 {
		t.Fatalf("a good appointment: %d", r.Status)
	}
	if r := e.enter(u, session, "R", 1); r.Status != 409 {
		t.Errorf("the same reference twice is a 409: %d", r.Status)
	}
	if r := e.do(call{method: "POST", path: "/api/appointments", token: u.Token, headers: map[string]string{"Content-Type": "application/json"}, body: "{broken"}); r.Status == 202 {
		t.Error("a malformed body must not create an appointment")
	}
}

func TestUnknownRoutesAreProblemJSON(t *testing.T) {
	e := newEnv(t)
	r := e.get("/api/nope", "")
	if r.Status != 404 || r.Header.Get("Content-Type") != "application/problem+json" || r.Header.Get("X-Correlation-Id") == "" {
		t.Fatalf("%d %s %v", r.Status, r.Raw, r.Header)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers on every response")
	}
}

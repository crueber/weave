package authz_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// placementWorld: card "c" lives on board A (home, project pa) and, through extra rows written by SQL (the ops that
// place cards come later), on B (project pb, private) and P (project pp, public). Card "solo" has one placement (A).
func placementWorld(t *testing.T) *ops.Env {
	t.Helper()
	e := testutil.Env(t, nil)
	x := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := "2026-01-01T00:00:00Z"
	for _, u := range []struct{ id, role string }{
		{"amgr", "boardUser"}, {"bview", "boardUser"}, {"bedit", "boardUser"}, {"both", "boardUser"}, {"worker", "worker"},
		{"outsider", "boardUser"}, {"admin", "admin"}, {"twoedit", "boardUser"},
	} {
		x(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,?)`, u.id, u.id+"@example.com", u.id, u.role, now)
	}
	x(`INSERT INTO projects(id,name,created_at) VALUES('pa','PA',?),('pb','PB',?),('pp','PP',?)`, now, now, now)
	x(`INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES('pm1','pa','amgr',?),('pm2','pa','both',?)`, now, now)
	x(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('A','pa',1,'A',?),('B','pb',1,'B',?),('P','pp',1,'P',?)`, now, now, now)
	x(`UPDATE boards SET is_public=1 WHERE id='P'`)
	x(`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('la','A','active',1,'A1',?),('lb','B','active',1,'B1',?),('lp','P','active',1,'P1',?)`, now, now, now)
	for i, m := range []struct{ user, board, role string }{
		{"bview", "B", "viewer"}, {"bedit", "B", "editor"}, {"both", "B", "viewer"}, {"worker", "B", "viewer"},
		{"twoedit", "B", "editor"}, {"twoedit", "P", "editor"},
	} {
		x(`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, "bm"+string(rune('a'+i)), m.board, m.user, m.role, now)
	}
	x(`INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,list_changed_at,created_at) VALUES
		('c','A','la','amgr',1,'Shared',?,?),('solo','A','la','amgr',2,'Solo',?,?)`, now, now, now, now)
	x(`INSERT INTO card_memberships(card_id,user_id) VALUES('c','worker')`)
	x(`INSERT INTO card_placements(card_id,board_id,list_id,position,list_changed_at,is_home,placed_by,placed_at) VALUES
		('c','B','lb',1,?,0,'amgr','2026-02-01T00:00:00Z'),('c','P','lp',1,?,0,'amgr','2026-03-01T00:00:00Z')`, now, now)
	return e
}

func as(e *ops.Env, id, role string) *ops.Env {
	if id == "" {
		return testutil.As(e, nil)
	}
	return testutil.As(e, &ops.Principal{UserID: id, Role: role})
}

func code(err error) string {
	var oe *ops.Error
	if errors.As(err, &oe) {
		return oe.Code + ":" + oe.Msg
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestCardAccessOverPlacements(t *testing.T) {
	e := placementWorld(t)
	for _, tc := range []struct {
		name, user, role, hint string
		level                  authz.Level
		ctx, home              string
		boards                 map[string]authz.Level
		worker                 bool
	}{
		// the maximum over viewable placements; hidden placements are never named
		{"home manager", "amgr", "boardUser", "", authz.Manage, "A", "A", map[string]authz.Level{"A": authz.Manage, "P": authz.View}, false},
		{"B viewer sees B (and the public P), never A", "bview", "boardUser", "", authz.View, "B", "", map[string]authz.Level{"B": authz.View, "P": authz.View}, false},
		{"B editor", "bedit", "boardUser", "", authz.Edit, "B", "", map[string]authz.Level{"B": authz.Edit, "P": authz.View}, false},
		{"max over placements, context = home", "both", "boardUser", "", authz.Manage, "A", "A", map[string]authz.Level{"A": authz.Manage, "B": authz.View, "P": authz.View}, false},
		{"a viewable hint wins", "both", "boardUser", "B", authz.Manage, "B", "A", map[string]authz.Level{"A": authz.Manage, "B": authz.View, "P": authz.View}, false},
		{"a hidden hint is ignored", "bview", "boardUser", "A", authz.View, "B", "", map[string]authz.Level{"B": authz.View, "P": authz.View}, false},
		{"equal levels: the oldest placement", "twoedit", "boardUser", "", authz.Edit, "B", "", map[string]authz.Level{"B": authz.Edit, "P": authz.Edit}, false},
		{"worker member keeps the narrow rule apart", "worker", "worker", "", authz.View, "B", "", map[string]authz.Level{"B": authz.View, "P": authz.View}, true},
		{"anonymous through the public placement", "", "", "", authz.View, "P", "", map[string]authz.Level{"P": authz.View}, false},
		{"outsider through the public placement", "outsider", "boardUser", "", authz.View, "P", "", map[string]authz.Level{"P": authz.View}, false},
		{"admin without membership = outsider", "admin", "admin", "", authz.View, "P", "", map[string]authz.Level{"P": authz.View}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := authz.CardAccessFor(as(e, tc.user, tc.role), "c", tc.hint)
			if err != nil {
				t.Fatal(err)
			}
			if a.Level != tc.level || a.BoardID != tc.ctx || a.HomeBoardID != tc.home || !reflect.DeepEqual(a.Boards, tc.boards) || a.WorkerMember != tc.worker {
				t.Fatalf("got level=%v ctx=%q home=%q boards=%v worker=%v", a.Level, a.BoardID, a.HomeBoardID, a.Boards, a.WorkerMember)
			}
		})
	}

	// drop the public placement: outsiders, anonymous callers and the admin lose the card entirely
	if _, err := e.DB.Exec(`DELETE FROM card_placements WHERE card_id='c' AND board_id='P'`); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ id, role string }{{"outsider", "boardUser"}, {"", ""}, {"admin", "admin"}} {
		pe := as(e, u.id, u.role)
		if _, err := authz.CardAccessFor(pe, "c", ""); code(err) != "not_found:card not found" {
			t.Errorf("%q: CardAccessFor = %v", u.id, err)
		}
		if _, err := authz.RequireCard(pe, "c", authz.View); code(err) != "not_found:board not found" {
			t.Errorf("%q: RequireCard = %v", u.id, err)
		}
		if cc := authz.Card(pe, "c"); cc.CanView || cc.BoardID != "" {
			t.Errorf("%q: caps %+v name a board", u.id, cc)
		}
	}
	if _, err := authz.CardAccessFor(as(e, "amgr", "boardUser"), "missing", ""); code(err) != "not_found:card not found" {
		t.Errorf("missing card: %v", err)
	}

	// RequireCard / RequireCardEdit / Card use the same answer
	bv, be, w := as(e, "bview", "boardUser"), as(e, "bedit", "boardUser"), as(e, "worker", "worker")
	if b, err := authz.RequireCard(bv, "c", authz.View); err != nil || b != "B" {
		t.Errorf("viewer RequireCard(View) = %q %v", b, err)
	}
	if _, err := authz.RequireCard(bv, "c", authz.Edit); code(err) != "forbidden:not allowed" {
		t.Errorf("viewer RequireCard(Edit) = %v", err)
	}
	if b, err := authz.RequireCardEdit(be, "c"); err != nil || b != "B" {
		t.Errorf("editor RequireCardEdit = %q %v", b, err)
	}
	if _, err := authz.RequireCardEdit(bv, "c"); code(err) != "forbidden:not allowed" {
		t.Errorf("viewer RequireCardEdit = %v", err)
	}
	if b, err := authz.RequireCardEdit(w, "c"); err != nil || b != "B" { // the worker rule
		t.Errorf("worker RequireCardEdit = %q %v", b, err)
	}
	if _, err := authz.RequireCard(w, "c", authz.Edit); code(err) != "forbidden:not allowed" { // ...is not a level
		t.Errorf("worker RequireCard(Edit) = %v", err)
	}
	if cc := authz.Card(w, "c"); !cc.CanEdit || cc.CanEditExtras || cc.BoardID != "B" {
		t.Errorf("worker caps %+v", cc)
	}

	// RequireCardOn is placement-scoped
	for _, tc := range []struct {
		pe      *ops.Env
		board   string
		min     authz.Level
		want    string
		comment string
	}{
		{be, "B", authz.Edit, "", "Edit on B"},
		{bv, "B", authz.Edit, "forbidden:not allowed", "View on B"},
		{be, "A", authz.View, "not_found:card not found", "hidden board A"},
		{as(e, "amgr", "boardUser"), "B", authz.View, "not_found:card not found", "placement exists but the board is hidden"},
		{as(e, "amgr", "boardUser"), "P", authz.View, "not_found:card not found", "no placement on P any more"},
		{as(e, "amgr", "boardUser"), "A", authz.Manage, "", "home Manage"},
	} {
		if got := code(authz.RequireCardOn(tc.pe, "c", tc.board, tc.min)); got != tc.want {
			t.Errorf("%s: RequireCardOn = %q, want %q", tc.comment, got, tc.want)
		}
	}

	// CardViewers: one query, home first, hidden placements absent; CardLevels: max over viewable placements
	got := authz.CardViewers(e, "c", []string{"amgr", "bview", "both", "outsider", "admin", "nobody"})
	want := map[string][]string{"amgr": {"A"}, "bview": {"B"}, "both": {"A", "B"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CardViewers = %v, want %v", got, want)
	}
	if l := authz.CardLevels(bv, []string{"c", "solo", "missing"}); !reflect.DeepEqual(l, map[string]authz.Level{"c": authz.View}) {
		t.Errorf("CardLevels(bview) = %v", l)
	}
	if l := authz.CardLevels(as(e, "both", "boardUser"), []string{"c", "solo"}); !reflect.DeepEqual(l, map[string]authz.Level{"c": authz.Manage, "solo": authz.Manage}) {
		t.Errorf("CardLevels(both) = %v", l)
	}
}

// With a single placement the new checks give exactly what the single-board checks gave: the old implementation is
// kept here as the reference and compared for every persona and level (codes, messages and returned board).
func TestSinglePlacementAccessUnchanged(t *testing.T) {
	e := placementWorld(t)
	if _, err := e.DB.Exec(`DELETE FROM card_placements WHERE is_home=0`); err != nil {
		t.Fatal(err)
	}
	oldRequireCard := func(pe *ops.Env, id string, min authz.Level) (string, error) {
		var b string
		if pe.DB.QueryRow(`SELECT board_id FROM cards WHERE id=?`, id).Scan(&b) != nil {
			return "", ops.NotFound("card")
		}
		return b, authz.RequireBoard(pe, b, min)
	}
	oldCanEdit := func(pe *ops.Env, id string) bool {
		var b string
		if pe.DB.QueryRow(`SELECT board_id FROM cards WHERE id=?`, id).Scan(&b) != nil {
			return false
		}
		l := authz.BoardLevel(pe, b)
		if l >= authz.Edit {
			return true
		}
		if l < authz.View || pe.User == nil || pe.User.Role != "worker" {
			return false
		}
		var n int
		_ = pe.DB.QueryRow(`SELECT COUNT(*) FROM card_memberships WHERE card_id=? AND user_id=?`, id, pe.User.UserID).Scan(&n)
		return n > 0
	}
	// the worker becomes a viewer of A (the home) so the worker rule is exercised on a single placement
	if _, err := e.DB.Exec(`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('wa','A','worker','viewer','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ id, role string }{{"amgr", "boardUser"}, {"bview", "boardUser"}, {"bedit", "boardUser"}, {"both", "boardUser"},
		{"worker", "worker"}, {"outsider", "boardUser"}, {"admin", "admin"}, {"", ""}} {
		pe := as(e, u.id, u.role)
		for _, card := range []string{"c", "solo", "missing"} {
			for _, min := range []authz.Level{authz.View, authz.Comment, authz.Edit, authz.Manage} {
				ob, oerr := oldRequireCard(pe, card, min)
				nb, nerr := authz.RequireCard(pe, card, min)
				if code(oerr) != code(nerr) || (oerr == nil && ob != nb) {
					t.Errorf("%q %s min %v: RequireCard %q %q, was %q %q", u.id, card, min, nb, code(nerr), ob, code(oerr))
				}
			}
			if cc := authz.Card(pe, card); cc.CanEdit != oldCanEdit(pe, card) {
				t.Errorf("%q %s: CanEdit %v, was %v", u.id, card, cc.CanEdit, oldCanEdit(pe, card))
			}
		}
	}
}

// The bulk form (search, dashboard, watching) decides exactly as CardAccessFor for every persona and hint, and the
// "also on" counts name only viewable other placements.
func TestCardAccessesMatchCardAccessFor(t *testing.T) {
	e := placementWorld(t)
	ids := []string{"c", "solo", "missing"}
	for _, u := range []struct{ id, role string }{{"amgr", "boardUser"}, {"bview", "boardUser"}, {"bedit", "boardUser"}, {"both", "boardUser"},
		{"worker", "worker"}, {"outsider", "boardUser"}, {"admin", "admin"}, {"twoedit", "boardUser"}, {"", ""}} {
		pe := as(e, u.id, u.role)
		for _, hint := range []string{"", "A", "B", "P", "nope"} {
			bulk := authz.CardAccesses(pe, ids, hint)
			for _, id := range ids {
				one, err := authz.CardAccessFor(pe, id, hint)
				got, ok := bulk[id]
				if (err == nil) != ok {
					t.Errorf("%q %s hint %q: bulk present %v, single err %v", u.id, id, hint, ok, err)
					continue
				}
				if !ok {
					continue
				}
				one.WorkerMember = false
				if !reflect.DeepEqual(got, one) {
					t.Errorf("%q %s hint %q: bulk %+v, single %+v", u.id, id, hint, got, one)
				}
			}
		}
		// AlsoOn from board B: c's other viewable placements (A, P), never a hidden one
		also := authz.AlsoOn(pe, "B", nil)
		want := 0
		for _, b := range []string{"A", "P"} {
			if authz.BoardLevel(pe, b) >= authz.View {
				want++
			}
		}
		if also["c"] != want || len(also) > 1 {
			t.Errorf("%q: AlsoOn(B) = %v, want c:%d", u.id, also, want)
		}
	}
}

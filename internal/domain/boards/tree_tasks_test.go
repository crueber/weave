package boards_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

func treeCard(t *testing.T, tree boards.TreeOut, id string) events.CardSnap {
	t.Helper()
	for _, l := range tree.Lists {
		for _, c := range l.Cards {
			if c.ID == id {
				return c
			}
		}
	}
	t.Fatalf("card %s not in tree", id)
	return events.CardSnap{}
}

// board.tree shows checklist progress per card (an agent adding tasks must be able to verify them
// from the tree) and the card members (assignees).
func TestBoardTreeTaskCountsAndMembers(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Tasks")
	l := addList(t, e, b.ID, "Todo")
	addCard(t, e, b.ID, l.ID, "c-a", "With tasks", 1, "")
	addCard(t, e, b.ID, l.ID, "c-b", "No tasks", 2, "")

	tl := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c-a", Name: "Checklist"})
	tl2 := call[cardextras.TaskListItem](t, e, "tasklist.create", cardextras.TaskListCreateIn{CardID: "c-a", Name: "Second"})
	first := call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "one"})
	call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl.ID, Name: "two"})
	call[cardextras.TaskItem](t, e, "task.create", cardextras.TaskCreateIn{TaskListID: tl2.ID, Name: "three", IsCompleted: true})
	done := true
	call[cardextras.TaskItem](t, e, "task.update", cardextras.TaskUpdateIn{ID: first.ID, IsCompleted: &done})
	if _, err := e.DB.Exec(`INSERT INTO card_memberships(card_id,user_id) VALUES('c-a',?)`, editor.UserID); err != nil {
		t.Fatal(err)
	}

	tree := call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: b.ID})
	a := treeCard(t, tree, "c-a")
	if a.TasksTotal != 3 || a.TasksCompleted != 2 {
		t.Fatalf("c-a tasks %d/%d, want 2/3", a.TasksCompleted, a.TasksTotal)
	}
	if len(a.Members) != 1 || a.Members[0].ID != editor.UserID || a.Members[0].Name != editor.Name {
		t.Fatalf("members: %+v", a.Members)
	}
	if bb := treeCard(t, tree, "c-b"); bb.TasksTotal != 0 || bb.TasksCompleted != 0 {
		t.Fatalf("c-b: %+v", bb)
	}
	// the same snapshot feeds card.list, card.next and event payloads
	if s := events.Snapshot(e, "c-a"); s == nil || s.TasksTotal != 3 || s.TasksCompleted != 2 {
		t.Fatalf("snapshot: %+v", s)
	}
}

// seedBigBoard inserts n cards over 5 lists, each with a task list of 3 tasks (one done).
func seedBigBoard(tb testing.TB, e *ops.Env, n int) string {
	tb.Helper()
	b, err := ops.Typed[boards.Board](e, "board.create", boards.BoardCreateIn{ProjectID: "p1", Name: "Big"})
	if err != nil {
		tb.Fatal(err)
	}
	var lists []string
	for i := 0; i < 5; i++ {
		l, err := ops.Typed[boards.List](e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: fmt.Sprint("L", i)})
		if err != nil {
			tb.Fatal(err)
		}
		lists = append(lists, l.ID)
	}
	tx, err := e.DB.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		cid, tlid := fmt.Sprintf("big-c%05d", i), fmt.Sprintf("big-tl%05d", i)
		if _, err := tx.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES(?,?,?,?,?,'2024-01-01T00:00:00Z','2024-01-01T00:00:00Z')`,
			cid, b.ID, lists[i%5], float64(i), "card "+cid); err != nil {
			tb.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO task_lists(id,card_id,position,name,created_at) VALUES(?,?,1,'cl','2024-01-01T00:00:00Z')`, tlid, cid); err != nil {
			tb.Fatal(err)
		}
		for k := 0; k < 3; k++ {
			if _, err := tx.Exec(`INSERT INTO tasks(id,task_list_id,position,name,is_completed,created_at) VALUES(?,?,?,?,?,'2024-01-01T00:00:00Z')`,
				fmt.Sprintf("%s-t%d", tlid, k), tlid, float64(k), "t", k == 0); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return b.ID
}

// A 1000-card board stays one batched read: the task counts are one aggregate query per 400-card
// chunk, not a query per card.
func TestBoardTreeLargeBoard(t *testing.T) {
	e := fixture(t)
	bid := seedBigBoard(t, e, 1000)
	start := time.Now()
	tree := call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: bid})
	took := time.Since(start)
	n := 0
	for _, l := range tree.Lists {
		for _, c := range l.Cards {
			n++
			if c.TasksTotal != 3 || c.TasksCompleted != 1 {
				t.Fatalf("%s: %d/%d", c.ID, c.TasksCompleted, c.TasksTotal)
			}
		}
	}
	if n != 1000 {
		t.Fatalf("cards %d", n)
	}
	t.Logf("board.tree on 1000 cards: %v", took)
	if took > 3*time.Second { // generous: ~10ms locally; an N+1 regression under -race is far slower
		t.Fatalf("board.tree took %v on 1000 cards", took)
	}
}

func BenchmarkBoardTree1000(b *testing.B) {
	e := fixture(b)
	bid := seedBigBoard(b, e, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ops.Typed[boards.TreeOut](e, "board.tree", boards.TreeIn{ID: bid}); err != nil {
			b.Fatal(err)
		}
	}
}

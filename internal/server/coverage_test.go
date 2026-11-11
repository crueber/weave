package server_test

import "testing"

func TestEveryObservableMutationEmits(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Coverage")
	u := e.newUser("m@x.io", "boardUser")
	_, me := e.api(u, "GET", "/auth/me", nil)
	uid := id(me["item"].(map[string]any))
	e.must(e.admin, "POST", "/boards/"+bd.id+"/members", map[string]any{"userId": uid, "role": "editor"})
	lab := id(e.must(e.admin, "POST", "/boards/"+bd.id+"/labels", map[string]any{"name": "L", "color": "berry-red"}))
	s := e.open(e.admin, "boards="+bd.id, nil)

	c := e.card(bd.a, "subject")
	e.must(e.admin, "PATCH", "/cards/"+c, map[string]any{"name": "renamed"})
	e.must(e.admin, "POST", "/cards/"+c+"/labels", map[string]any{"labelId": lab})
	e.must(e.admin, "DELETE", "/cards/"+c+"/labels/"+lab, nil)
	e.must(e.admin, "POST", "/cards/"+c+"/members", map[string]any{"userId": uid})
	e.must(e.admin, "DELETE", "/cards/"+c+"/members/"+uid, nil)
	e.must(e.admin, "POST", "/cards/"+c+"/comments", map[string]any{"text": "hi"})
	tl := id(e.must(e.admin, "POST", "/cards/"+c+"/task-lists", map[string]any{"name": "todo"}))
	tk := id(e.must(e.admin, "POST", "/task-lists/"+tl+"/tasks", map[string]any{"name": "t"}))
	e.must(e.admin, "PATCH", "/tasks/"+tk, map[string]any{"isCompleted": true})
	e.must(e.admin, "POST", "/cards/"+c+"/attachments", map[string]any{"url": "https://example.com/x", "name": "link"})
	e.must(e.admin, "POST", "/cards/"+c+"/archive", map[string]any{})
	e.must(e.admin, "POST", "/cards/"+c+"/restore", map[string]any{})
	e.must(e.admin, "POST", "/boards/"+bd.id+"/lists", map[string]any{"name": "D"})
	e.must(e.admin, "PATCH", "/lists/"+bd.c, map[string]any{"name": "C2"})

	want := []string{"card.created", "card.updated", "label.added", "label.removed", "member.added", "member.removed", "comment.created",
		"tasklist.created", "task.created", "task.updated", "attachment.created", "card.archived", "card.restored", "list.created", "list.updated"}
	got := s.events(len(want))
	for i, w := range want {
		if str(got[i], "type") != w {
			t.Errorf("event %d = %s, want %s", i, str(got[i], "type"), w)
		}
		if i < 11 && str(got[i], "listId") != bd.a {
			t.Errorf("event %s listId=%q, want %s", w, str(got[i], "listId"), bd.a)
		}
	}
	if str(got[13], "listId") == "" || str(got[14], "listId") != bd.c {
		t.Errorf("list events must carry the list id: %v %v", got[13], got[14])
	}
}

func TestBulkListOperationsEmitPerCardEvents(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Bulk")
	c1, c2 := e.card(bd.a, "one"), e.card(bd.a, "two")
	s := e.open(e.admin, "lists="+bd.b, nil)
	e.must(e.admin, "POST", "/lists/"+bd.a+"/move-cards", map[string]any{"listId": bd.b})
	got := s.events(2)
	for i, c := range []string{c1, c2} {
		if str(got[i], "type") != "card.moved" || str(got[i], "cardId") != c || str(got[i], "fromListId") != bd.a || str(got[i], "toListId") != bd.b {
			t.Fatalf("bulk event %d: %v", i, got[i])
		}
	}
	// retiring a list archives its cards: listeners of the retired list hear it
	s2 := e.open(e.admin, "lists="+bd.b, nil)
	e.must(e.admin, "POST", "/lists/"+bd.b+"/archive", nil)
	a := s2.events(3) // two card.archived (from b) + list.deleted
	if str(a[0], "type") != "card.archived" || str(a[0], "fromListId") != bd.b {
		t.Fatalf("retire event %v", a[0])
	}
}

func TestBoardTree(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Tree")
	c1 := e.card(bd.a, "one")
	e.card(bd.a, "two")
	e.card(bd.b, "three")
	closed := e.card(bd.c, "four")
	e.must(e.admin, "POST", "/cards/"+closed+"/close", map[string]any{})
	e.must(e.admin, "PATCH", "/cards/"+c1, map[string]any{"description": "desc"})
	tree := e.must(e.admin, "GET", "/boards/"+bd.id+"/tree", nil)
	lists := tree["lists"].([]any)
	count := func(ls []any) (n int) {
		for _, l := range ls {
			n += len(l.(map[string]any)["cards"].([]any))
		}
		return
	}
	if tree["board"].(map[string]any)["id"] != bd.id || len(lists) < 3 || count(lists) != 3 {
		t.Fatalf("tree lists=%d cards=%d: %v", len(lists), count(lists), tree)
	}
	first := lists[0].(map[string]any)["cards"].([]any)[0].(map[string]any)
	if first["name"] != "one" || first["description"] != "desc" || first["labels"] == nil || first["members"] == nil || first["listId"] != bd.a {
		t.Fatalf("compact card %v", first)
	}
	if _, ok := first["claimedBy"]; !ok {
		t.Fatal("compact card must carry claimedBy")
	}
	all := e.must(e.admin, "GET", "/boards/"+bd.id+"/tree?includeClosed=true", nil)
	if n := count(all["lists"].([]any)); n != 4 {
		t.Fatalf("includeClosed cards=%d, want 4", n)
	}
	only := e.must(e.admin, "GET", "/boards/"+bd.id+"/tree?listIds="+bd.b, nil)
	if ls := only["lists"].([]any); len(ls) != 1 || count(ls) != 1 {
		t.Fatalf("listIds filter %v", ls)
	}
}

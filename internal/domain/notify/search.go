package notify

import (
	"database/sql"
	"html"
	"regexp"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

type SearchIn struct {
	Q       string `query:"q" required:"true" desc:"search text (words are AND-ed, the last one matches as a prefix)"`
	Kind    string `query:"kind" desc:"restrict to card | comment | board | project"`
	BoardID string `query:"boardId" desc:"restrict to one board"`
	Limit   int    `query:"limit" desc:"max results (default 30, max 100)"`
}

type SearchHit struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	BoardID     string `json:"boardId,omitempty"`
	BoardName   string `json:"boardName,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
	CardID      string `json:"cardId,omitempty"`
	Title       string `json:"title"`
	SnippetHTML string `json:"snippetHtml,omitempty"`
	URL         string `json:"url"`
}
type SearchOut struct {
	Query string      `json:"query"`
	Items []SearchHit `json:"items"`
}
type ReindexOut struct {
	Indexed int `json:"indexed"`
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// ftsQuery turns free text into a safe FTS5 query: every word quoted, last one a prefix.
func ftsQuery(q string) string {
	ws := wordRe.FindAllString(q, 12)
	for i, w := range ws {
		ws[i] = `"` + w + `"`
	}
	if len(ws) == 0 {
		return ""
	}
	ws[len(ws)-1] += "*"
	return strings.Join(ws, " ")
}

// markSnippet escapes an FTS snippet delimited by \x02 \x03 and turns the delimiters into <mark>.
func markSnippet(s string) string {
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, "\x02", "<mark>")
	return strings.ReplaceAll(s, "\x03", "</mark>")
}

type boardMeta struct {
	name, projectID, project string
}

// Search runs the global search as the caller; hits are filtered by board/project access.
func Search(e *ops.Env, in SearchIn) (SearchOut, error) {
	out := SearchOut{Query: in.Q, Items: []SearchHit{}}
	fq := ftsQuery(in.Q)
	if fq == "" {
		return out, ops.Invalid("empty search query")
	}
	if in.BoardID != "" && authz.BoardLevel(e, in.BoardID) < authz.View {
		return out, nil
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := `SELECT kind,ref_id,board_id,title,snippet(search_idx,4,char(2),char(3),'…',14) FROM search_idx WHERE search_idx MATCH ?`
	args := []any{fq}
	if in.Kind != "" {
		q += ` AND kind=?`
		args = append(args, in.Kind)
	}
	if in.BoardID != "" {
		// the index keeps ONE row per card or comment, under the card's home board; a card placed on this board (and
		// its comments) matches too
		q += ` AND (board_id=? OR (kind='card' AND EXISTS(SELECT 1 FROM card_placements p WHERE p.card_id=search_idx.ref_id AND p.board_id=?))
			OR (kind='comment' AND EXISTS(SELECT 1 FROM comments m JOIN card_placements p ON p.card_id=m.card_id WHERE m.id=search_idx.ref_id AND p.board_id=?)))`
		args = append(args, in.BoardID, in.BoardID, in.BoardID)
	}
	q += ` ORDER BY rank LIMIT ?`
	args = append(args, limit*8)
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return out, err
	}
	type cand struct{ kind, ref, board, title, snip string }
	var cands []cand
	for rows.Next() {
		var c cand
		_ = rows.Scan(&c.kind, &c.ref, &c.board, &c.title, &c.snip)
		cands = append(cands, c)
	}
	rows.Close()
	// card and comment hits: access over the card's placements (one bulk call), shown on the context board (the
	// boardId filter when the card is placed there, else the home when viewable, else the board with the most
	// access); the board of the index row (the home) is never named unless the caller can view it
	cardOf := map[string]string{} // comment id -> card id
	var cardIDs, commentIDs, projectIDs, boardKindIDs []string
	seenProj, seenBoard := map[string]bool{}, map[string]bool{}
	for _, c := range cands {
		switch c.kind {
		case "card":
			cardIDs = append(cardIDs, c.ref)
		case "comment":
			commentIDs = append(commentIDs, c.ref)
		case "project":
			if !seenProj[c.ref] {
				seenProj[c.ref] = true
				projectIDs = append(projectIDs, c.ref)
			}
		case "board":
			if !seenBoard[c.board] {
				seenBoard[c.board] = true
				boardKindIDs = append(boardKindIDs, c.board)
			}
		}
	}
	if len(commentIDs) > 0 { // one query (at most limit*8 = 800 candidates)
		ph := "?" + strings.Repeat(",?", len(commentIDs)-1)
		cargs := make([]any, len(commentIDs))
		for i, id := range commentIDs {
			cargs[i] = id
		}
		if rs, err := e.DB.QueryContext(e.Ctx, `SELECT id,card_id FROM comments WHERE id IN (`+ph+`)`, cargs...); err == nil {
			for rs.Next() {
				var id, cid string
				if rs.Scan(&id, &cid) == nil {
					cardOf[id] = cid
					cardIDs = append(cardIDs, cid)
				}
			}
			rs.Close()
		}
	}
	access := authz.CardAccesses(e, cardIDs, in.BoardID)
	projLevels := authz.ProjectLevels(e, projectIDs)
	boardLevels := authz.BoardLevels(e, boardKindIDs)

	// batch load board/project names for every viewable board referenced by any candidate hit
	var metaIDs []any
	metaSeen := map[string]bool{}
	addMeta := func(id string) {
		if id != "" && !metaSeen[id] {
			metaSeen[id] = true
			metaIDs = append(metaIDs, id)
		}
	}
	for _, id := range boardKindIDs {
		if boardLevels[id] >= authz.View {
			addMeta(id)
		}
	}
	for _, a := range access {
		if in.BoardID == "" || a.BoardID == in.BoardID {
			addMeta(a.BoardID)
		}
	}
	boards := map[string]boardMeta{}
	if len(metaIDs) > 0 {
		ph := "?" + strings.Repeat(",?", len(metaIDs)-1)
		if rs, err := e.DB.QueryContext(e.Ctx, `SELECT b.id,b.name,b.project_id,p.name FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.id IN (`+ph+`)`, metaIDs...); err == nil {
			for rs.Next() {
				var id string
				var b boardMeta
				if rs.Scan(&id, &b.name, &b.projectID, &b.project) == nil {
					boards[id] = b
				}
			}
			rs.Close()
		}
	}
	for _, c := range cands {
		if len(out.Items) >= limit {
			break
		}
		h := SearchHit{Kind: c.kind, ID: c.ref, Title: c.title, SnippetHTML: markSnippet(c.snip)}
		switch c.kind {
		case "project":
			if projLevels[c.ref] < authz.View {
				continue
			}
			h.ProjectID, h.URL = c.ref, "/projects/"+c.ref
			h.ProjectName = c.title
		case "board":
			if boardLevels[c.board] < authz.View {
				continue
			}
			b, ok := boards[c.board]
			if !ok {
				continue
			}
			h.BoardID, h.BoardName, h.ProjectID, h.ProjectName, h.URL = c.board, b.name, b.projectID, b.project, "/boards/"+c.board
		case "card", "comment":
			h.CardID = c.ref
			if c.kind == "comment" {
				cid, ok := cardOf[c.ref]
				if !ok {
					continue
				}
				h.CardID = cid
			}
			a, ok := access[h.CardID]
			if !ok || (in.BoardID != "" && a.BoardID != in.BoardID) {
				// with a boardId filter, only cards the caller can see ON that board: a hit shown elsewhere would
				// reveal a placement on a board the caller cannot view
				continue
			}
			b, ok := boards[a.BoardID]
			if !ok {
				continue
			}
			h.BoardID, h.BoardName, h.ProjectID, h.ProjectName = a.BoardID, b.name, b.projectID, b.project
			h.URL = "/cards/" + h.CardID
		default:
			continue
		}
		out.Items = append(out.Items, h)
	}
	return out, nil
}

func reindexTx(tx *sql.Tx) (n int, err error) {
	stmts := []string{
		`DELETE FROM search_idx`, `DELETE FROM search_map`,
		`INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'card',id,board_id,name,COALESCE(description,'') FROM cards WHERE is_draft=0 AND is_template=0`,
		`INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'comment',m.id,c.board_id,c.name,m.text FROM comments m JOIN cards c ON c.id=m.card_id`,
		`INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'board',id,id,name,'' FROM boards WHERE is_template=0`,
		`INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'project',id,'',name,COALESCE(description,'') FROM projects`,
		`INSERT OR REPLACE INTO search_map SELECT kind,ref_id,rowid FROM search_idx`,
	}
	for _, s := range stmts {
		if _, err = tx.Exec(s); err != nil {
			return
		}
	}
	err = tx.QueryRow(`SELECT COUNT(*) FROM search_map`).Scan(&n)
	return
}

// Reindex rebuilds the whole search index from the source tables.
func Reindex(e *ops.Env) (int, error) {
	n := 0
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) (err error) { n, err = reindexTx(tx); return })
	return n, err
}

// IndexCard (re)indexes one card and its comments (normally done by DB triggers;
// exported for bulk importers that disable them or write rows in odd orders).
func IndexCard(e *ops.Env, cardID string) error {
	return e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		del := func(kind, id string) error {
			if _, err := tx.Exec(`DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind=? AND ref_id=?)`, kind, id); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM search_map WHERE kind=? AND ref_id=?`, kind, id)
			return err
		}
		var ids []string
		rows, err := tx.Query(`SELECT id FROM comments WHERE card_id=?`, cardID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if err := del("comment", id); err != nil {
				return err
			}
		}
		if err := del("card", cardID); err != nil {
			return err
		}
		add := func(q, kind string, id string) error {
			r, err := tx.Exec(q, id)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return nil
			}
			rid, err := r.LastInsertId()
			if err != nil {
				return err
			}
			_, err = tx.Exec(`INSERT OR REPLACE INTO search_map VALUES(?,?,?)`, kind, id, rid)
			return err
		}
		if err := add(`INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'card',id,board_id,name,COALESCE(description,'') FROM cards WHERE id=? AND is_draft=0 AND is_template=0`, "card", cardID); err != nil {
			return err
		}
		for _, id := range ids {
			if err := add(`INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'comment',m.id,c.board_id,c.name,m.text FROM comments m JOIN cards c ON c.id=m.card_id WHERE m.id=?`, "comment", id); err != nil {
				return err
			}
		}
		return nil
	})
}

func init() {
	ops.Register(ops.Op{Name: "search.query", Summary: "Global full-text search over cards, comments, boards and projects the caller can see", Method: "GET", Path: "/search", UI: "GET /search"},
		func(e *ops.Env, in SearchIn) (SearchOut, error) { return Search(e, in) })
	ops.Register(ops.Op{Name: "search.reindex", Summary: "Rebuild the search index from scratch (admin)", Method: "POST", Path: "/search/reindex", Auth: ops.AuthAdmin, UI: "POST /ui/admin/search/reindex"},
		func(e *ops.Env, _ NoInput) (ReindexOut, error) {
			n, err := Reindex(e)
			return ReindexOut{n}, err
		})
}

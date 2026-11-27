package cardextras

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
)

// Board export/import and board templates carry custom fields through boards.ExportCustomFields / ImportCustomFields
// (boards cannot import this package). The portable shape is boards.FieldGroupDoc / FieldValueDoc.

const (
	maxDocGroups = 50
	maxDocFields = 100
)

func init() {
	boards.ExportCustomFields = exportBoardFields
	boards.ImportCustomFields = importBoardFields
}

func exportBoardFields(ctx context.Context, q boards.Querier, boardID string, withValues bool) ([]boards.FieldGroupDoc, map[string][]boards.FieldValueDoc, error) {
	type gr struct {
		id, name, card string
		rep            bool
		base           sql.NullString
	}
	scan := func(r *sql.Rows) (gr, error) {
		var g gr
		var card sql.NullString
		err := r.Scan(&g.id, &g.name, &g.rep, &g.base, &card)
		g.card = card.String
		return g, err
	}
	sel := `SELECT g.id,COALESCE(g.name,b.name,''),g.repeatable,g.base_group_id,g.card_id FROM custom_field_groups g LEFT JOIN base_custom_field_groups b ON b.id=g.base_group_id WHERE `
	gs, err := query(ctx, q, scan, sel+`g.board_id=? ORDER BY g.position,g.id`, boardID)
	if err != nil {
		return nil, nil, err
	}
	if withValues {
		cg, err := query(ctx, q, scan, sel+`g.card_id IN (SELECT card_id FROM card_placements WHERE board_id=?) ORDER BY g.position,g.id`, boardID)
		if err != nil {
			return nil, nil, err
		}
		gs = append(gs, cg...)
	}
	var groups []boards.FieldGroupDoc
	known := map[string]map[string]bool{} // group -> field ids
	if len(gs) > 0 {
		type frow struct {
			doc     boards.FieldDoc
			groupID string
			baseID  string
			pos     float64
		}
		gIDs := make([]any, 0, len(gs))
		var bIDs []any
		seenBase := map[string]bool{}
		for _, g := range gs {
			gIDs = append(gIDs, g.id)
			if g.base.Valid && g.base.String != "" && !seenBase[g.base.String] {
				seenBase[g.base.String] = true
				bIDs = append(bIDs, g.base.String)
			}
		}
		fq := `SELECT id,group_id,base_group_id,name,type,options,position,show_on_front_of_card FROM custom_fields WHERE group_id IN (` + inPlaceholders(len(gIDs)) + `)`
		fargs := gIDs
		if len(bIDs) > 0 {
			fq += ` OR base_group_id IN (` + inPlaceholders(len(bIDs)) + `)`
			fargs = append(fargs, bIDs...)
		}
		fq += ` ORDER BY position,id`
		frows, err := query(ctx, q, func(r *sql.Rows) (frow, error) {
			var x frow
			var gid, bid, opts sql.NullString
			var front bool
			err := r.Scan(&x.doc.ID, &gid, &bid, &x.doc.Name, &x.doc.Type, &opts, &x.pos, &front)
			x.groupID, x.baseID = gid.String, bid.String
			x.doc.ShowOnFrontOfCard = front
			if opts.Valid {
				_ = json.Unmarshal([]byte(opts.String), &x.doc.Options)
			}
			return x, err
		}, fq, fargs...)
		if err != nil {
			return nil, nil, err
		}
		byGroup := map[string][]frow{}
		byBase := map[string][]frow{}
		for _, fr := range frows {
			if fr.groupID != "" {
				byGroup[fr.groupID] = append(byGroup[fr.groupID], fr)
			}
			if fr.baseID != "" {
				byBase[fr.baseID] = append(byBase[fr.baseID], fr)
			}
		}
		for _, g := range gs {
			// A group derived from a project base group is flattened: its fields (own and inherited) become plain fields.
			own := byGroup[g.id]
			var merged []frow
			if g.base.Valid && g.base.String != "" {
				base := byBase[g.base.String]
				if len(own) == 0 {
					merged = base
				} else if len(base) == 0 {
					merged = own
				} else {
					merged = make([]frow, 0, len(own)+len(base))
					merged = append(merged, own...)
					merged = append(merged, base...)
					sort.Slice(merged, func(i, j int) bool {
						if merged[i].pos != merged[j].pos {
							return merged[i].pos < merged[j].pos
						}
						return merged[i].doc.ID < merged[j].doc.ID
					})
				}
			} else {
				merged = own
			}
			fs := make([]boards.FieldDoc, 0, len(merged))
			known[g.id] = map[string]bool{}
			for _, fr := range merged {
				fs = append(fs, fr.doc)
				known[g.id][fr.doc.ID] = true
			}
			groups = append(groups, boards.FieldGroupDoc{ID: g.id, Name: g.name, Repeatable: g.rep, Card: g.card, Fields: fs})
		}
	}
	if !withValues || len(groups) == 0 {
		return groups, nil, nil
	}
	// repeatable rows are addressed by their order within (card, group)
	type rk struct{ card, group, row string }
	rowIdx := map[rk]int{}
	counts := map[[2]string]int{}
	rows, err := query(ctx, q, func(r *sql.Rows) ([3]string, error) {
		var x [3]string
		return x, r.Scan(&x[0], &x[1], &x[2])
	}, `SELECT r.card_id,r.group_id,r.id FROM custom_field_rows r JOIN card_placements p ON p.card_id=r.card_id WHERE p.board_id=? ORDER BY r.position,r.id`, boardID)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		k := [2]string{r[0], r[1]}
		rowIdx[rk{r[0], r[1], r[2]}] = counts[k]
		counts[k]++
	}
	vs, err := query(ctx, q, func(r *sql.Rows) ([5]string, error) {
		var x [5]string
		return x, r.Scan(&x[0], &x[1], &x[2], &x[3], &x[4])
	}, `SELECT v.card_id,v.group_id,v.field_id,v.row_id,COALESCE(v.content,'') FROM custom_field_values v JOIN card_placements p ON p.card_id=v.card_id WHERE p.board_id=? ORDER BY v.card_id`, boardID)
	if err != nil {
		return nil, nil, err
	}
	values := map[string][]boards.FieldValueDoc{}
	for _, v := range vs {
		if !known[v[1]][v[2]] || v[4] == "" {
			continue
		}
		row := 0
		if v[3] != "" {
			i, ok := rowIdx[rk{v[0], v[1], v[3]}]
			if !ok {
				continue
			}
			row = i
		}
		values[v[0]] = append(values[v[0]], boards.FieldValueDoc{GroupID: v[1], FieldID: v[2], Row: row, Content: v[4]})
	}
	return groups, values, nil
}

func importBoardFields(ctx context.Context, tx *sql.Tx, boardID string, groups []boards.FieldGroupDoc, values map[string][]boards.FieldValueDoc, cardIDs map[string]string) error {
	now := db.Now()
	type fk struct {
		id, typ string
		opts    []string
	}
	type gk struct {
		id, card string // new group id, new card id for card-level groups
		rep      bool
		fields   map[string]fk
	}
	made := map[string]gk{}
	// Board-wide groups go after the board's own (0 on a new board; board.template.apply adds to an existing one).
	var base float64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0) FROM custom_field_groups WHERE board_id=?`, boardID).Scan(&base); err != nil {
		return err
	}
	for i, g := range groups {
		if i >= maxDocGroups {
			break
		}
		name := strings.TrimSpace(g.Name)
		if _, dup := made[g.ID]; dup || len(name) > 255 {
			continue
		}
		var board, card any
		var newCard string
		if g.Card == "" {
			board = boardID
		} else if newCard = cardIDs[g.Card]; newCard != "" {
			card = newCard
		} else {
			continue // a card-level group of a card that is not part of the copy
		}
		gid := db.NewID()
		pos := float64(i+1) * db.PositionGap
		if g.Card == "" {
			pos += base
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO custom_field_groups(id,board_id,card_id,name,repeatable,position,created_at) VALUES(?,?,?,?,?,?,?)`,
			gid, board, card, nz(name), b2i(g.Repeatable), pos, now); err != nil {
			return err
		}
		made[g.ID] = gk{id: gid, card: newCard, rep: g.Repeatable, fields: map[string]fk{}}
		for j, f := range g.Fields {
			if j >= maxDocFields || strings.TrimSpace(f.Name) == "" || len(f.Name) > 255 {
				continue
			}
			typ := f.Type
			if !fieldTypes[typ] {
				typ = "text"
			}
			opts := cleanOptions(f.Options)
			var ojson any
			if typ == "select" {
				b, _ := json.Marshal(opts)
				ojson = string(b)
			}
			fid := db.NewID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO custom_fields(id,group_id,name,type,options,position,show_on_front_of_card) VALUES(?,?,?,?,?,?,?)`,
				fid, gid, strings.TrimSpace(f.Name), typ, ojson, float64(j+1)*db.PositionGap, b2i(f.ShowOnFrontOfCard)); err != nil {
				return err
			}
			made[g.ID].fields[f.ID] = fk{fid, typ, opts}
		}
	}
	type rkey struct {
		card, group string
		row         int
	}
	rowIDs := map[rkey]string{}
	for key, vs := range values {
		cid := cardIDs[key]
		if cid == "" {
			continue
		}
		for _, v := range vs {
			g, ok := made[v.GroupID]
			if !ok || (g.card != "" && g.card != cid) {
				continue
			}
			f, ok := g.fields[v.FieldID]
			if !ok || v.Content == "" {
				continue
			}
			content, err := normalizeValue(f.typ, f.opts, v.Content)
			if err != nil {
				continue // an invalid value in a file is skipped, not fatal
			}
			rowID := ""
			if g.rep {
				if v.Row < 0 || v.Row > 1000 {
					continue
				}
				k := rkey{cid, g.id, v.Row}
				if rowID = rowIDs[k]; rowID == "" {
					rowID = db.NewID()
					rowIDs[k] = rowID
					if _, err := tx.ExecContext(ctx, `INSERT INTO custom_field_rows(id,card_id,group_id,position,created_at) VALUES(?,?,?,?,?)`,
						rowID, cid, g.id, float64(v.Row+1)*db.PositionGap, now); err != nil {
						return err
					}
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO custom_field_values(id,card_id,group_id,field_id,row_id,content) VALUES(?,?,?,?,?,?)`,
				db.NewID(), cid, g.id, f.id, rowID, content); err != nil {
				return err
			}
		}
	}
	return nil
}

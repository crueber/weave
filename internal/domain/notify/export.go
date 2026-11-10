package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

type ExportBoardIn struct {
	ID     string `path:"id" desc:"board id"`
	Format string `query:"format" desc:"html (standalone page, default) | pdf (see description)"`
}
type ExportCardIn struct {
	ID     string `path:"id" desc:"card id"`
	Format string `query:"format" desc:"html (standalone page, default) | pdf (see description)"`
}

type exLabel struct{ Name, Color string }
type exTask struct {
	Name string
	Done bool
}
type exTaskList struct {
	Name  string
	Tasks []exTask
}
type exComment struct{ User, Text, At string }
type exField struct{ Group, Name, Value string }
type exCard struct {
	ID, Name, Description, Due, List, Type string
	Closed, DueDone                        bool
	Labels                                 []exLabel
	Members                                []string
	TaskLists                              []exTaskList
	Comments                               []exComment
	Attachments                            []string
	Fields                                 []exField
}
type exList struct {
	Name  string
	Cards []*exCard
}
type exDoc struct {
	Title, Project, Board, Kind string
	Generated                   string
	Print                       bool
	PrintJS                     template.JS
	Lists                       []exList
	Cards                       []*exCard // single-card export
}

// printJS is the only script in an export (the print button). Exports must not need external files
// because they are also saved and opened offline, so it is inline; the page that serves it inline
// allows exactly this script by hash (see serveExport).
const printJS = template.JS(`document.getElementById("weave-print").addEventListener("click",function(){window.print()})`)

// exportCSP is the policy for an export shown in the browser: no network, only the print script.
func exportCSP() string {
	h := sha256.Sum256([]byte(printJS))
	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(h[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox allow-scripts allow-modals"
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(s, "-"), "-")
	if s == "" {
		return "export"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// loadCards loads cards (with labels, members, tasks, comments, attachments, custom fields) for a
// condition on the cards table alias c, e.g. "c.board_id=?" / "c.id=?".
func loadCards(e *ops.Env, where string, arg any) ([]*exCard, error) {
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT c.id,c.name,COALESCE(c.description,''),COALESCE(c.due_date,''),COALESCE(c.is_due_completed,0),c.is_closed,c.type,COALESCE(l.name,l.type),l.type
	 FROM cards c JOIN lists l ON l.id=c.list_id WHERE c.is_draft=0 AND c.is_template=0 AND `+where+` ORDER BY l.position, c.position`, arg)
	if err != nil {
		return nil, err
	}
	var cards []*exCard
	byID := map[string]*exCard{}
	for rows.Next() {
		c := &exCard{}
		var ltype string
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.Due, &c.DueDone, &c.Closed, &c.Type, &c.List, &ltype); err != nil {
			rows.Close()
			return nil, err
		}
		if ltype == "archive" || ltype == "trash" {
			// archived/trashed lists are not part of an export of the live board
			if !strings.Contains(where, "c.id=?") {
				continue
			}
		}
		cards = append(cards, c)
		byID[c.ID] = c
	}
	rows.Close()
	query := func(q string, fn func(scan func(...any) error)) {
		rs, err := e.DB.QueryContext(e.Ctx, q, arg)
		if err != nil {
			return
		}
		defer rs.Close()
		for rs.Next() {
			fn(rs.Scan)
		}
	}
	query(`SELECT cl.card_id,COALESCE(l.name,''),l.color FROM card_labels cl JOIN labels l ON l.id=cl.label_id JOIN cards c ON c.id=cl.card_id WHERE `+where+` ORDER BY l.position`, func(s func(...any) error) {
		var id string
		var l exLabel
		_ = s(&id, &l.Name, &l.Color)
		if c := byID[id]; c != nil {
			c.Labels = append(c.Labels, l)
		}
	})
	query(`SELECT m.card_id,u.name FROM card_memberships m JOIN users u ON u.id=m.user_id JOIN cards c ON c.id=m.card_id WHERE `+where+` ORDER BY u.name`, func(s func(...any) error) {
		var id, n string
		_ = s(&id, &n)
		if c := byID[id]; c != nil {
			c.Members = append(c.Members, n)
		}
	})
	type tlref struct {
		c *exCard
		i int
	}
	tls := map[string]tlref{}
	query(`SELECT tl.id,tl.card_id,tl.name FROM task_lists tl JOIN cards c ON c.id=tl.card_id WHERE `+where+` ORDER BY tl.position`, func(s func(...any) error) {
		var id, cid, n string
		_ = s(&id, &cid, &n)
		if c := byID[cid]; c != nil {
			c.TaskLists = append(c.TaskLists, exTaskList{Name: n})
			tls[id] = tlref{c, len(c.TaskLists) - 1}
		}
	})
	query(`SELECT t.task_list_id,t.name,t.is_completed FROM tasks t JOIN task_lists tl ON tl.id=t.task_list_id JOIN cards c ON c.id=tl.card_id WHERE `+where+` ORDER BY t.position`, func(s func(...any) error) {
		var id string
		var t exTask
		_ = s(&id, &t.Name, &t.Done)
		if r, ok := tls[id]; ok {
			r.c.TaskLists[r.i].Tasks = append(r.c.TaskLists[r.i].Tasks, t)
		}
	})
	query(`SELECT m.card_id,COALESCE(u.name,'Unknown'),m.text,m.created_at FROM comments m LEFT JOIN users u ON u.id=m.user_id JOIN cards c ON c.id=m.card_id WHERE `+where+` ORDER BY m.created_at`, func(s func(...any) error) {
		var id string
		var cm exComment
		_ = s(&id, &cm.User, &cm.Text, &cm.At)
		if c := byID[id]; c != nil {
			c.Comments = append(c.Comments, cm)
		}
	})
	query(`SELECT a.card_id,a.name,COALESCE(a.url,'') FROM attachments a JOIN cards c ON c.id=a.card_id WHERE `+where+` ORDER BY a.created_at`, func(s func(...any) error) {
		var id, n, u string
		_ = s(&id, &n, &u)
		if c := byID[id]; c != nil {
			if u != "" {
				n += " (" + u + ")"
			}
			c.Attachments = append(c.Attachments, n)
		}
	})
	query(`SELECT v.card_id,COALESCE(g.name,''),f.name,COALESCE(v.content,'') FROM custom_field_values v JOIN custom_fields f ON f.id=v.field_id JOIN custom_field_groups g ON g.id=v.group_id JOIN cards c ON c.id=v.card_id WHERE `+where+` ORDER BY g.position,f.position`, func(s func(...any) error) {
		var id string
		var f exField
		_ = s(&id, &f.Group, &f.Name, &f.Value)
		if c := byID[id]; c != nil && f.Value != "" {
			c.Fields = append(c.Fields, f)
		}
	})
	return cards, nil
}

var exportTpl = template.Must(template.New("export").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light}
body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;color:#1d2433;margin:0;padding:24px;background:#fff}
h1{font-size:24px;margin:0 0 4px}h2{font-size:18px;margin:28px 0 10px;padding-bottom:4px;border-bottom:2px solid #d5dbe6}
h3{font-size:15px;margin:0 0 4px}.meta{color:#5b6579;font-size:12px}
.card{border:1px solid #d5dbe6;border-radius:6px;padding:10px 12px;margin:10px 0;break-inside:avoid;page-break-inside:avoid}
.card.closed h3{text-decoration:line-through;color:#5b6579}
.desc{white-space:pre-wrap;margin:6px 0}.label{display:inline-block;padding:0 8px;border-radius:10px;background:#e8ecf4;font-size:11px;margin-right:4px}
ul.tasks{list-style:none;margin:4px 0;padding-left:0}ul.tasks li:before{content:"\2610  "}ul.tasks li.done:before{content:"\2611  "}ul.tasks li.done{color:#5b6579;text-decoration:line-through}
.comment{border-left:3px solid #d5dbe6;padding:2px 8px;margin:6px 0}.comment .who{font-size:12px;color:#5b6579}
dl{margin:6px 0}dt{font-weight:600;float:left;margin-right:6px}dd{margin:0}
.noprint{margin-bottom:16px}.noprint button{font:inherit;padding:6px 12px}
@page{size:A4;margin:14mm}
@media print{.noprint{display:none}body{padding:0}h2{break-after:avoid}}
</style></head><body>
{{if .Print}}<div class="noprint"><button id="weave-print">Print / Save as PDF</button></div><script>{{.PrintJS}}</script>{{end}}
<h1>{{.Title}}</h1>
<div class="meta">{{if .Project}}Project: {{.Project}} &middot; {{end}}{{if .Board}}Board: {{.Board}} &middot; {{end}}Exported {{.Generated}}</div>
{{define "card"}}<div class="card{{if .Closed}} closed{{end}}"><h3>{{.Name}}</h3>
<div class="meta">{{.List}}{{if .Due}} &middot; Due {{.Due}}{{if .DueDone}} (done){{end}}{{end}}{{if .Members}} &middot; Members: {{range $i,$m := .Members}}{{if $i}}, {{end}}{{$m}}{{end}}{{end}}</div>
{{if .Labels}}<div>{{range .Labels}}<span class="label">{{if .Name}}{{.Name}}{{else}}{{.Color}}{{end}}</span>{{end}}</div>{{end}}
{{if .Description}}<div class="desc">{{.Description}}</div>{{end}}
{{range .TaskLists}}<div><strong>{{.Name}}</strong><ul class="tasks">{{range .Tasks}}<li{{if .Done}} class="done"{{end}}>{{.Name}}</li>{{end}}</ul></div>{{end}}
{{if .Fields}}<dl>{{range .Fields}}<dt>{{if .Group}}{{.Group}} / {{end}}{{.Name}}:</dt><dd>{{.Value}}</dd><br>{{end}}</dl>{{end}}
{{if .Attachments}}<div class="meta">Attachments: {{range $i,$a := .Attachments}}{{if $i}}, {{end}}{{$a}}{{end}}</div>{{end}}
{{range .Comments}}<div class="comment"><div class="who">{{.User}} &middot; {{.At}}</div><div class="desc">{{.Text}}</div></div>{{end}}
</div>{{end}}
{{range .Lists}}<h2>{{.Name}} <span class="meta">({{len .Cards}})</span></h2>{{range .Cards}}{{template "card" .}}{{end}}{{end}}
{{range .Cards}}{{template "card" .}}{{end}}
</body></html>`))

// PDFCommand, when non-empty (env WEAVE_PDF_CMD, e.g. "wkhtmltopdf -q - -"), renders the HTML
// to a real PDF: HTML on stdin, PDF on stdout.
func pdfCommand() string { return os.Getenv("WEAVE_PDF_CMD") }

func finish(doc exDoc, format, base string) (*ops.Stream, error) {
	switch format {
	case "", "html", "pdf":
	default:
		return nil, ops.Invalid("format must be html or pdf")
	}
	doc.Print = format == "pdf"
	doc.PrintJS = printJS
	doc.Generated = time.Now().UTC().Format("2006-01-02 15:04 UTC")
	var buf bytes.Buffer
	if err := exportTpl.Execute(&buf, doc); err != nil {
		return nil, err
	}
	if format == "pdf" {
		if cmd := strings.Fields(pdfCommand()); len(cmd) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
			c.Stdin = bytes.NewReader(buf.Bytes())
			var out bytes.Buffer
			c.Stdout = &out
			if err := c.Run(); err == nil && bytes.HasPrefix(out.Bytes(), []byte("%PDF")) {
				return &ops.Stream{Name: base + ".pdf", ContentType: "application/pdf", Data: out.Bytes()}, nil
			}
			// engine failed: fall through to print-optimised HTML
		}
		// No PDF engine available: print-optimised HTML (use the browser's "Save as PDF").
		return &ops.Stream{Name: base + ".print.html", ContentType: "text/html; charset=utf-8", Data: buf.Bytes()}, nil
	}
	return &ops.Stream{Name: base + ".html", ContentType: "text/html; charset=utf-8", Data: buf.Bytes()}, nil
}

func init() {
	ops.Register(ops.Op{Name: "export.board", Summary: "Export a board as standalone HTML, or print-optimised HTML / PDF (PDF only with WEAVE_PDF_CMD; otherwise print-ready HTML to save as PDF from the browser)", Method: "GET", Path: "/export/boards/{id}", Binary: true, UI: "GET /export/boards/{id}"},
		func(e *ops.Env, in ExportBoardIn) (*ops.Stream, error) {
			if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
				return nil, err
			}
			doc := exDoc{Kind: "board"}
			if err := e.DB.QueryRowContext(e.Ctx, `SELECT b.name,p.name FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.id=?`, in.ID).Scan(&doc.Board, &doc.Project); err != nil {
				return nil, ops.NotFound("board")
			}
			doc.Title = doc.Board
			cards, err := loadCards(e, "c.board_id=?", in.ID)
			if err != nil {
				return nil, err
			}
			idx := map[string]int{}
			for _, c := range cards {
				i, ok := idx[c.List]
				if !ok {
					i = len(doc.Lists)
					idx[c.List] = i
					doc.Lists = append(doc.Lists, exList{Name: c.List})
				}
				doc.Lists[i].Cards = append(doc.Lists[i].Cards, c)
			}
			return finish(doc, in.Format, slug(doc.Board))
		})
	ops.Register(ops.Op{Name: "export.card", Summary: "Export a card as standalone HTML, or print-optimised HTML / PDF (see export.board)", Method: "GET", Path: "/export/cards/{id}", Binary: true, UI: "GET /export/cards/{id}"},
		func(e *ops.Env, in ExportCardIn) (*ops.Stream, error) {
			board, err := authz.RequireCard(e, in.ID, authz.View)
			if err != nil {
				return nil, err
			}
			doc := exDoc{Kind: "card"}
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT b.name,p.name FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.id=?`, board).Scan(&doc.Board, &doc.Project)
			cards, err := loadCards(e, "c.id=?", in.ID)
			if err != nil || len(cards) == 0 {
				return nil, ops.NotFound("card")
			}
			doc.Cards, doc.Title = cards, cards[0].Name
			return finish(doc, in.Format, slug(cards[0].Name))
		})
}

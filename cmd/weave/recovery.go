package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/projects"
)

// Host-only recovery commands for the move to ownership-based permissions (docs/MAINTENANCE.md).

const preflightUsage = "usage: weave admin preflight [--email EMAIL] [--json] [--db PATH]"

// preflight prints, read-only, what an upgrade to ownership-based permissions changes for this data directory:
// projects with no active manager, projects an admin will no longer see, and boards only admins manage. It opens
// the database with db.OpenReadOnly (no migrations), so it is safe on a deployed, not yet upgraded instance while
// the old server runs. Exit 0 = nothing to fix, 1 = action needed (or an error), 2 = usage.
func preflight(args []string, stdout, stderr io.Writer) int {
	var email, path string
	var asJSON bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--email", "--db":
			if i+1 >= len(args) || args[i+1] == "" {
				fmt.Fprintln(stderr, preflightUsage)
				return 2
			}
			if args[i] == "--email" {
				email = args[i+1]
			} else {
				path = args[i+1]
			}
			i++
		case "--json":
			asJSON = true
		default:
			fmt.Fprintln(stderr, preflightUsage)
			return 2
		}
	}
	if path == "" {
		path = config.Load().DataDir + "/weave.db"
	}
	d, err := db.OpenReadOnly(path)
	if err != nil {
		fmt.Fprintln(stderr, "weave: open", path+":", err)
		return 1
	}
	defer d.Close()
	r, err := projects.Preflight(context.Background(), d, email)
	if err != nil {
		fmt.Fprintln(stderr, "weave:", err)
		return 1
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"item": r, "needsAction": r.NeedsAction()}, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		writePreflight(stdout, path, email, r)
	}
	if r.NeedsAction() {
		return 1
	}
	return 0
}

// clean strips control characters from names before they reach the operator's terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func writePreflight(w io.Writer, path, email string, r projects.PreflightReport) {
	fmt.Fprintf(w, "Weave preflight for ownership-based permissions (read-only: %s)\n", clean(path))
	fmt.Fprintf(w, "%s, %s.\n", plural(r.Projects, "project", "projects"), plural(r.Boards, "board", "boards"))

	fmt.Fprintf(w, "\n(a) Projects with no active manager: %d\n", len(r.Ownerless))
	fmt.Fprintln(w, "    Every manager is deactivated, deleted, a guest or an agent account. After the upgrade only the recovery path")
	fmt.Fprintln(w, "    reaches them: Admin console > Orphaned projects, or weave admin adopt --project ID --email E.")
	if len(r.Ownerless) == 0 {
		fmt.Fprintln(w, "    none")
	}
	for _, o := range r.Ownerless {
		line := fmt.Sprintf("  - %s [%s] %s", clean(o.Name), o.ID, plural(o.Boards, "board", "boards"))
		if o.BoardsWithoutOwner > 0 {
			line += fmt.Sprintf(" (%d without an active board owner)", o.BoardsWithoutOwner)
		}
		if len(o.FormerManagers) == 0 {
			line += "; former managers: none left (deleted)"
		} else {
			var fm []string
			for _, m := range o.FormerManagers {
				fm = append(fm, fmt.Sprintf("%s <%s> (%s)", clean(m.Name), clean(m.Email), m.State))
			}
			line += "; former managers: " + strings.Join(fm, ", ")
		}
		fmt.Fprintln(w, line)
	}

	who := "each active admin"
	if email != "" {
		who = clean(email)
	}
	lost := 0
	fmt.Fprintf(w, "\n(b) Projects %s will no longer see (not a manager, not a member of any of its boards, directly or through a group)\n", who)
	fmt.Fprintln(w, "    Before the upgrade an admin can still fix this: Project settings > Managers (project.manager.add), or board memberships.")
	if len(r.Admins) == 0 {
		fmt.Fprintln(w, "    no active admin accounts")
	}
	for _, a := range r.Admins {
		lost += len(a.Unreachable)
		role := ""
		if a.Role != "admin" {
			role = ", role " + clean(a.Role)
		}
		fmt.Fprintf(w, "  %s <%s>%s: %d of %s\n", clean(a.Name), clean(a.Email), role, len(a.Unreachable), plural(a.Projects, "project", "projects"))
		for _, p := range a.Unreachable {
			fmt.Fprintf(w, "    - %s [%s] %s\n", clean(p.Name), p.ID, plural(p.Boards, "board", "boards"))
		}
	}

	fmt.Fprintf(w, "\n(c) Boards only admins manage (for information; does not change the exit code): %d\n", len(r.AdminOnlyBoards))
	fmt.Fprintln(w, "    Every active manager of the board (its project's managers plus its board owners) is an admin. Nothing changes")
	fmt.Fprintln(w, "    for them on upgrade, but nobody else can manage these boards: consider a non-admin manager or board owner.")
	if len(r.AdminOnlyBoards) == 0 {
		fmt.Fprintln(w, "    none")
	}
	for _, b := range r.AdminOnlyBoards {
		var ms []string
		for _, m := range b.Managers {
			ms = append(ms, clean(m))
		}
		fmt.Fprintf(w, "  - %s / %s [%s]: %s\n", clean(b.ProjectName), clean(b.Name), b.ID, strings.Join(ms, ", "))
	}

	verdict := "OK: nothing to fix before upgrading"
	if r.NeedsAction() {
		verdict = "action needed before upgrading"
	}
	fmt.Fprintf(w, "\nSummary: %s without an active manager, %s out of an admin's reach (%s only admins manage): %s.\n",
		plural(len(r.Ownerless), "project", "projects"), plural(lost, "project", "projects"), plural(len(r.AdminOnlyBoards), "board", "boards"), verdict)
}

const adoptUsage = "usage: weave admin adopt --project PROJECT_ID --email EMAIL"

// adopt is the break-glass path when no admin can reach the UI: it makes the person with that e-mail address a
// manager of the project (any project, ownerless or not), audited in the recovery trail (source host) and the
// server log. Like mint-key it opens the database normally (migrations apply), so run it with the binary of the
// deployed version.
func adopt(args []string, stdout, stderr io.Writer) int {
	var project, email string
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) || args[i+1] == "" {
			fmt.Fprintln(stderr, adoptUsage)
			return 2
		}
		switch args[i] {
		case "--project":
			project = args[i+1]
		case "--email":
			email = args[i+1]
		default:
			fmt.Fprintln(stderr, adoptUsage)
			return 2
		}
		i++
	}
	if project == "" || email == "" {
		fmt.Fprintln(stderr, adoptUsage)
		return 2
	}
	d, err := db.Open(config.Load().DataDir + "/weave.db")
	if err != nil {
		fmt.Fprintln(stderr, "weave:", err)
		return 1
	}
	defer d.Close()
	r, err := projects.Adopt(context.Background(), d, project, email)
	if err != nil {
		fmt.Fprintln(stderr, "weave:", err)
		return 1
	}
	b, _ := json.Marshal(map[string]any{"item": r})
	fmt.Fprintln(stdout, string(b))
	return 0
}

package boards

import (
	"context"
	"database/sql"
)

// Custom fields belong to cardextras, which imports boards, so the board export/import and the template engines reach
// them through two hooks that cardextras installs in its init(). Without the hook (a build that does not link
// cardextras) custom fields are simply left out of exports and ignored on import.
//
// The portable shape is deliberately plain: groups with their fields, and per-card values addressed by group/field ids
// that are only meaningful inside one document. Repeatable groups address rows by their zero-based index.

// FieldDoc is one custom field of a group.
type FieldDoc struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Type              string   `json:"type"` // text|number|date|checkbox|select|url
	Options           []string `json:"options,omitempty"`
	ShowOnFrontOfCard bool     `json:"showOnFrontOfCard,omitempty"`
}

// FieldGroupDoc is a custom-field group. Card is empty for a board-wide group (a group derived from a project base group
// is flattened into one of these) or the document id of the one card a card-level group belongs to.
type FieldGroupDoc struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Repeatable bool       `json:"repeatable,omitempty"`
	Card       string     `json:"card,omitempty"`
	Fields     []FieldDoc `json:"fields"`
}

// FieldValueDoc is one stored value; Row is the zero-based row of a repeatable group (0 otherwise).
type FieldValueDoc struct {
	GroupID string `json:"groupId"`
	FieldID string `json:"fieldId"`
	Row     int    `json:"row,omitempty"`
	Content string `json:"content"`
}

// Querier is the read/write subset shared by *sql.DB and *sql.Tx that the hooks need.
type Querier = querier

// ExportCustomFields returns a board's custom-field groups and, when withValues is set, the values of its cards keyed by
// card id (card-level groups are only included together with values). Installed by cardextras.
var ExportCustomFields func(ctx context.Context, q Querier, boardID string, withValues bool) (groups []FieldGroupDoc, values map[string][]FieldValueDoc, err error)

// ImportCustomFields creates the groups on boardID and stores values for the cards in cardIDs (document card id -> new
// card id; values of unknown cards and card-level groups of unknown cards are dropped). Everything coming from a file is
// untrusted and is re-validated by the owner. Installed by cardextras.
var ImportCustomFields func(ctx context.Context, tx *sql.Tx, boardID string, groups []FieldGroupDoc, values map[string][]FieldValueDoc, cardIDs map[string]string) error

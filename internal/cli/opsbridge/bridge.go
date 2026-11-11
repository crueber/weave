// Package opsbridge converts the in-process op registry into the CLI's schema types. It is the
// only place that links both internal/ops (database) and internal/cli, so the slim CLI binary
// never depends on it; the full binary and the generators do.
package opsbridge

import (
	"github.com/crueber/weave/internal/cli"
	"github.com/crueber/weave/internal/ops"
)

// FromRegistry returns every registered op as CLI schema, sorted by name.
func FromRegistry() []cli.Op {
	all := ops.All()
	out := make([]cli.Op, 0, len(all))
	for _, o := range all {
		s := o.Schema()
		c := cli.Op{Name: s.Name, Tag: s.Tag, Summary: s.Summary, Method: s.Method, Path: s.Path, Auth: s.Auth, UI: s.UI}
		for _, f := range s.Fields {
			c.Fields = append(c.Fields, cli.Field{Name: f.Name, In: f.In, Type: f.Type, Required: f.Required, Desc: f.Desc})
		}
		out = append(out, c)
	}
	return out
}

// Install makes the full binary's CLI use the live registry as its offline schema.
func Install() { cli.LocalSchema = FromRegistry }

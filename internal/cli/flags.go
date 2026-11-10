package cli

import (
	"strings"
)

// FlagSpec describes one flag. Names are matched case-insensitively and ignoring - and _,
// so --list-id, --list_id and --listId are the same flag.
type FlagSpec struct {
	Name    string
	Aliases []string
	Short   string // single letter, used as -o
	Bool    bool
	Multi   bool // repeatable (values accumulate)
}

func norm(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	return strings.ReplaceAll(s, "_", "")
}

// Parsed is the result of ParseFlags.
type Parsed struct {
	vals map[string][]string
	Pos  []string
	Rest []string // everything after a bare "--"
}

func (p *Parsed) Has(name string) bool { _, ok := p.vals[name]; return ok }
func (p *Parsed) Str(name string) string {
	if v := p.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}
func (p *Parsed) All(name string) []string { return p.vals[name] }
func (p *Parsed) Bool(name string) bool {
	v := p.Str(name)
	return v == "true" || v == "1" || v == "yes"
}

// Names returns the set flags in no particular order.
func (p *Parsed) Names() []string {
	var n []string
	for k := range p.vals {
		n = append(n, k)
	}
	return n
}

// ParseFlags parses args against specs; unknown flags are a usage error. Flags and positionals
// may be interleaved. Non-bool flags always take the next argument (or --flag=value); bool flags
// take it only when it is literally true/false.
func ParseFlags(args []string, specs []FlagSpec) (*Parsed, error) {
	byKey := map[string]*FlagSpec{}
	byShort := map[string]*FlagSpec{}
	for i := range specs {
		s := &specs[i]
		if _, dup := byKey[norm(s.Name)]; !dup {
			byKey[norm(s.Name)] = s
		}
		for _, a := range s.Aliases {
			if _, dup := byKey[norm(a)]; !dup {
				byKey[norm(a)] = s
			}
		}
		if s.Short != "" {
			byShort[s.Short] = s
		}
	}
	p := &Parsed{vals: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.Rest = append(p.Rest, args[i+1:]...)
			break
		}
		var spec *FlagSpec
		var val string
		var hasVal bool
		switch {
		case strings.HasPrefix(a, "--") && len(a) > 2:
			name, v, eq := strings.Cut(a[2:], "=")
			spec = byKey[norm(name)]
			if spec == nil {
				return nil, usageErr("unknown flag --" + name)
			}
			val, hasVal = v, eq
		case len(a) == 2 && a[0] == '-' && a[1] != '-' && byShort[a[1:]] != nil:
			spec = byShort[a[1:]]
		default:
			p.Pos = append(p.Pos, a)
			continue
		}
		if spec.Bool {
			if !hasVal {
				val = "true"
				if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
					i++
					val = args[i]
				}
			}
		} else if !hasVal {
			if i+1 >= len(args) {
				return nil, usageErr("flag --" + spec.Name + " needs a value")
			}
			i++
			val = args[i]
		}
		if spec.Multi {
			p.vals[spec.Name] = append(p.vals[spec.Name], val)
		} else {
			p.vals[spec.Name] = []string{val}
		}
	}
	return p, nil
}

// Global flags accepted by every command. A command's own flag with the same name wins.
func globalSpecs() []FlagSpec {
	return []FlagSpec{
		{Name: "url"}, {Name: "token"}, {Name: "profile"},
		{Name: "json", Bool: true}, {Name: "output"}, {Name: "fields"}, {Name: "jq"},
		{Name: "output-file", Aliases: []string{"out"}, Short: "o"},
		{Name: "limit"}, {Name: "cursor"}, {Name: "all", Bool: true},
		{Name: "dry-run", Bool: true}, {Name: "yes", Short: "y", Bool: true},
		{Name: "timeout"}, {Name: "quiet", Short: "q", Bool: true}, {Name: "no-color", Bool: true},
		{Name: "body"}, {Name: "stdin", Bool: true}, {Name: "refresh-schema", Bool: true},
		{Name: "help", Short: "h", Bool: true},
		{Name: "board", Aliases: []string{"board-id"}}, {Name: "project", Aliases: []string{"project-id"}},
	}
}

func mergeSpecs(own []FlagSpec) []FlagSpec {
	have := map[string]bool{}
	for _, s := range own {
		have[norm(s.Name)] = true
		for _, a := range s.Aliases {
			have[norm(a)] = true
		}
	}
	out := append([]FlagSpec{}, own...)
	for _, g := range globalSpecs() {
		if !have[norm(g.Name)] && (g.Short == "" || !shortTaken(own, g.Short)) {
			out = append(out, g)
		}
	}
	return out
}

func shortTaken(own []FlagSpec, s string) bool {
	for _, o := range own {
		if o.Short == s {
			return true
		}
	}
	return false
}

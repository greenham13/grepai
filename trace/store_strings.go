package trace

// Gob decodes each string occurrence separately. Share repeated paths and
// names while loading; discard the pool so deleted files are not retained.
type symbolStrings map[string]string

func (p symbolStrings) intern(value string) string {
	if old, ok := p[value]; ok {
		return old
	}
	p[value] = value
	return value
}

func (p symbolStrings) symbol(v *Symbol) {
	v.Name = p.intern(v.Name)
	v.Kind = SymbolKind(p.intern(string(v.Kind)))
	v.File = p.intern(v.File)
	v.Signature = p.intern(v.Signature)
	v.Receiver = p.intern(v.Receiver)
	v.Package = p.intern(v.Package)
	v.Language = p.intern(v.Language)
	v.Docstring = p.intern(v.Docstring)
	v.FeaturePath = p.intern(v.FeaturePath)
}

func (p symbolStrings) reference(v *Reference) {
	v.SymbolName = p.intern(v.SymbolName)
	v.Kind = p.intern(v.Kind)
	v.File = p.intern(v.File)
	v.Context = p.intern(v.Context)
	v.CallerName = p.intern(v.CallerName)
	v.CallerFile = p.intern(v.CallerFile)
}

func (p symbolStrings) record(v *symbolLogRecord) {
	v.File = p.intern(v.File)
	v.Hash = p.intern(v.Hash)
	v.Version = p.intern(v.Version)
	for i := range v.Symbols {
		p.symbol(&v.Symbols[i])
	}
	for i := range v.Refs {
		p.reference(&v.Refs[i])
	}
	for i := range v.Edges {
		p.edge(&v.Edges[i])
	}
}

func (p symbolStrings) edge(v *CallEdge) {
	v.Caller = p.intern(v.Caller)
	v.Callee = p.intern(v.Callee)
	v.File = p.intern(v.File)
	v.CallType = p.intern(v.CallType)
}

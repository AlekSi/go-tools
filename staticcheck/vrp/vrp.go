package vrp

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"math/big"
	"sort"
	"strings"

	"honnef.co/go/ssa"
)

type Future interface {
	Futures() []ssa.Value
}

type Range interface {
	Union(other Range) Range
	IsKnown() bool
}

type Constraint interface {
	Y() ssa.Value
	isConstraint()
	String() string
	Eval(*Graph) Range
	Operands() []ssa.Value
}

type aConstraint struct {
	y ssa.Value
}

func (aConstraint) isConstraint()  {}
func (c aConstraint) Y() ssa.Value { return c.y }

type PhiConstraint struct {
	aConstraint
	Vars []ssa.Value
}

func (c *PhiConstraint) Operands() []ssa.Value {
	return c.Vars
}

func (c *PhiConstraint) Eval(g *Graph) Range {
	i := Range(nil)
	for _, v := range c.Vars {
		i = g.Range(v).Union(i)
	}
	return i
}

func (c *PhiConstraint) String() string {
	names := make([]string, len(c.Vars))
	for i, v := range c.Vars {
		names[i] = v.Name()
	}
	return fmt.Sprintf("%s = φ(%s)", c.Y().Name(), strings.Join(names, ", "))
}

func isSupportedType(typ types.Type) bool {
	switch typ := typ.Underlying().(type) {
	case *types.Basic:
		if (typ.Info() & types.IsInteger) == 0 {
			return false
		}
	default:
		return false
	}
	return true
}

func BuildGraph(f *ssa.Function) *Graph {
	g := &Graph{
		Vertices: map[interface{}]*Vertex{},
		ranges:   map[ssa.Value]Range{},
	}
	var cs []Constraint
	for _, block := range f.Blocks {
		for _, ins := range block.Instrs {
			switch ins := ins.(type) {
			case *ssa.Convert:
				switch v := ins.Type().Underlying().(type) {
				case *types.Basic:
					if (v.Info() & types.IsInteger) == 0 {
						continue
					}
					c := &IntConversionConstraint{
						aConstraint: aConstraint{
							y: ins,
						},
						X: ins.X,
					}
					cs = append(cs, c)
				}
			case *ssa.BinOp:
				ops := ins.Operands(nil)
				basic, ok := (*ops[0]).Type().Underlying().(*types.Basic)
				if !ok {
					continue
				}
				if (basic.Info() & types.IsInteger) != 0 {
					fns := map[token.Token]func(ssa.Value, ssa.Value, ssa.Value) Constraint{
						token.ADD: NewAddConstraint,
						token.SUB: NewSubConstraint,
						token.MUL: NewMulConstraint,
						// XXX support QUO, REM, SHL, SHR
					}
					fn, ok := fns[ins.Op]
					if ok {
						cs = append(cs, fn(*ops[0], *ops[1], ins))
					}
					switch ins.Op {
					case token.REM:
						// XXX
						continue
						// XXX deal with sign/bits
						v, _ := constant.Int64Val((*ops[1]).(*ssa.Const).Value)
						v--
						i := NewInterval(NewZ(&big.Int{}), NewZ(big.NewInt(v)))
						c := &IntervalConstraint{
							aConstraint: aConstraint{
								y: ins,
							},
							I: i,
						}
						cs = append(cs, c)
					}
				}
			case *ssa.Slice:
				_, ok := ins.X.Type().Underlying().(*types.Basic)
				if !ok {
					continue
				}
				c := &StringSliceConstraint{
					aConstraint: aConstraint{
						y: ins,
					},
					X:     ins.X,
					Lower: ins.Low,
					Upper: ins.High,
				}
				cs = append(cs, c)
			case *ssa.Phi:
				if !isSupportedType(ins.Type()) {
					continue
				}
				ops := ins.Operands(nil)
				dops := make([]ssa.Value, len(ops))
				for i, op := range ops {
					dops[i] = *op

					if op, ok := (*op).(*ssa.Const); ok {
						switch typ := op.Type().Underlying().(type) {
						case *types.Basic:
							if (typ.Info() & types.IsInteger) != 0 {
								// XXX signs/bits
								val, _ := constant.Int64Val(op.Value)
								c := &IntervalConstraint{
									aConstraint: aConstraint{
										y: op,
									},
									I: NewInterval(NewZ(big.NewInt(val)), NewZ(big.NewInt(val))),
								}
								cs = append(cs, c)
							}
						}
					}
				}
				c := &PhiConstraint{
					aConstraint: aConstraint{
						y: ins,
					},
					Vars: dops,
				}
				cs = append(cs, c)
			case *ssa.Sigma:
				pred := ins.Block().Preds[0]
				instrs := pred.Instrs
				cond, ok := instrs[len(instrs)-1].(*ssa.If).Cond.(*ssa.BinOp)
				ops := cond.Operands(nil)
				if !ok {
					continue
				}
				switch typ := ins.Type().Underlying().(type) {
				case *types.Basic:
					if (typ.Info() & types.IsInteger) == 0 {
						continue
					}

					_, ok1 := (*ops[0]).(*ssa.Const)
					_, ok2 := (*ops[1]).(*ssa.Const)
					var logic func(op token.Token)
					if !ok1 && !ok2 {
						logic = func(op token.Token) {
							c := &FutureIntersectionConstraint{
								aConstraint: aConstraint{
									y: ins,
								},
								ranges:      g.ranges,
								lowerOffset: NewZ(&big.Int{}),
								upperOffset: NewZ(&big.Int{}),
							}
							var other ssa.Value
							if (*ops[0]) == ins.X {
								c.X = *ops[0]
								other = *ops[1]
							} else {
								c.X = *ops[1]
								other = *ops[0]
								op = invertToken(op)
							}

							switch op {
							case token.EQL:
								c.lower = other
								c.upper = other
							case token.GTR, token.GEQ:
								off := int64(0)
								if cond.Op == token.GTR {
									off = 1
								}
								c.lower = other
								c.lowerOffset = NewZ(big.NewInt(off))
								c.upper = nil
								c.upperOffset = PInfinity
							case token.LSS, token.LEQ:
								off := int64(0)
								if cond.Op == token.LSS {
									off = -1
								}
								c.lower = nil
								c.lowerOffset = NInfinity
								c.upper = other
								c.upperOffset = NewZ(big.NewInt(off))
							default:
								return
							}
							cs = append(cs, c)
						}
					} else {
						logic = func(op token.Token) {
							k, ok := (*ops[1]).(*ssa.Const)
							// XXX investigate in what cases this wouldn't be a Const
							if !ok {
								return
							}
							// XXX signs, bits
							v, _ := constant.Int64Val(k.Value)
							c := &IntersectionConstraint{
								aConstraint: aConstraint{
									y: ins,
								},
								X: *ops[0],
							}
							switch op {
							case token.EQL:
								c.I = NewInterval(NewZ(big.NewInt(v)), NewZ(big.NewInt(v)))
							case token.GTR, token.GEQ:
								off := int64(0)
								if cond.Op == token.GTR {
									off = 1
								}
								c.I = NewInterval(
									NewZ(big.NewInt(v+off)),
									PInfinity,
								)
							case token.LSS, token.LEQ:
								off := int64(0)
								if cond.Op == token.LSS {
									off = -1
								}
								c.I = NewInterval(
									NInfinity,
									NewZ(big.NewInt(v+off)),
								)
							default:
								return
							}
							cs = append(cs, c)
						}
					}

					op := cond.Op
					if !ins.Branch {
						op = (invertToken(op))
					}
					logic(op)
				default:
					//log.Printf("unsupported sigma type %T", typ) // XXX
				}
			}
		}
	}

	for _, c := range cs {
		// If V is used in constraint C, then we create an edge V->C
		for _, op := range c.Operands() {
			g.AddEdge(op, c, false)
		}
		if c, ok := c.(Future); ok {
			for _, op := range c.Futures() {
				g.AddEdge(op, c, true)
			}
		}
		// If constraint C defines variable V, then we create an edge
		// C->V
		g.AddEdge(c, c.Y(), false)
	}

	g.FindSCCs()
	return g
}

func (g *Graph) Solve() {
	for _, n := range g.Vertices {
		if v, ok := n.Value.(*ssa.Const); ok {
			switch typ := v.Type().Underlying().(type) {
			case *types.Basic:
				if (typ.Info() & types.IsInteger) != 0 {
					c, _ := constant.Int64Val(v.Value)
					g.SetRange(v, NewInterval(NewZ(big.NewInt(c)), NewZ(big.NewInt(c))))
				}
			}
		}
	}

	var consts []Z
	for _, n := range g.Vertices {
		if c, ok := n.Value.(*ssa.Const); ok {
			basic, ok := c.Type().Underlying().(*types.Basic)
			if !ok {
				continue
			}
			if (basic.Info() & types.IsInteger) != 0 {
				v, _ := constant.Int64Val(c.Value)
				consts = append(consts, NewZ(big.NewInt(v)))
			}
		}

	}
	sort.Sort(Zs(consts))

	for scc, vertices := range g.SCCs {
		n := 0
		n = len(vertices)
		if n == 1 {
			g.resolveFutures(scc)
			v := vertices[0]
			if v, ok := v.Value.(ssa.Value); ok {
				switch typ := v.Type().Underlying().(type) {
				case *types.Basic:
					switch typ.Kind() {
					case types.String:
						if !g.Range(v).(StringRange).IsKnown() {
							g.SetRange(v, StringRange{NewInterval(NewZ(&big.Int{}), PInfinity)})
						}
					default:
						if !g.Range(v).(Interval).IsKnown() {
							g.SetRange(v, InfinityFor(v))
						}
					}
				}
			}
			if c, ok := v.Value.(Constraint); ok {
				g.SetRange(c.Y(), c.Eval(g))
			}
		} else {
			uses := g.uses(scc)
			entries := g.entries(scc)
			for len(entries) > 0 {
				v := entries[0]
				entries = entries[1:] // XXX this leaks memory
				for _, use := range uses[v] {
					if g.widen(use, consts) {
						entries = append(entries, use.Y())
					}
				}
			}

			g.resolveFutures(scc)

			// XXX quoting the original code here: "ensure that this
			// code is really needed"
			for _, n := range vertices {
				if v, ok := n.Value.(ssa.Value); ok {
					i, ok := g.Range(v).(Interval)
					if !ok {
						continue
					}
					if !i.IsKnown() {
						g.SetRange(v, InfinityFor(v))
					}
				}
			}

			actives := g.actives(scc)
			uses = g.uses(scc) // XXX do we need to find uses twice?
			for len(actives) > 0 {
				v := actives[0]
				actives = actives[1:] // XXX this leaks memory
				for _, use := range uses[v] {
					if g.narrow(use, consts) {
						actives = append(actives, use.Y())
					}
				}
			}
		}
		// propagate scc
		for _, edge := range g.Edges {
			if edge.control {
				continue
			}
			if edge.From.SCC != scc {
				continue
			}
			if c, ok := edge.To.Value.(Constraint); ok {
				g.SetRange(c.Y(), c.Eval(g))
			}
			if c, ok := edge.To.Value.(*FutureIntersectionConstraint); ok {
				if !c.I.IsKnown() {
					c.resolved = false
				}
			}
		}
	}

	for v, r := range g.ranges {
		i, ok := r.(Interval)
		if !ok {
			continue
		}
		if (v.Type().Underlying().(*types.Basic).Info() & types.IsUnsigned) != 0 {
			if i.lower.Sign() == -1 {
				i = NewInterval(NewZ(&big.Int{}), PInfinity)
			}
		}
		if (v.Type().Underlying().(*types.Basic).Info() & types.IsUnsigned) == 0 {
			if i.upper == PInfinity {
				i = NewInterval(NInfinity, PInfinity)
			}
			if i.upper != PInfinity {
				s := &types.StdSizes{
					// XXX is it okay to assume the largest word size, or do we
					// need to be platform specific?
					WordSize: 8,
					MaxAlign: 1,
				}
				bits := (s.Sizeof(v.Type()) * 8) - 1
				n := big.NewInt(1)
				n = n.Lsh(n, uint(bits))
				upper, lower := &big.Int{}, &big.Int{}
				upper.Sub(n, big.NewInt(1))
				lower.Neg(n)

				if i.upper.Cmp(NewZ(upper)) == 1 {
					i = NewInterval(NInfinity, PInfinity)
				} else if i.lower.Cmp(NewZ(lower)) == -1 {
					i = NewInterval(NInfinity, PInfinity)
				}
			}
		}

		g.ranges[v] = i
	}
}

func VerticeString(v *Vertex) string {
	switch v := v.Value.(type) {
	case Constraint:
		return v.String()
	case ssa.Value:
		return v.Name()
	case nil:
		return "BUG: nil vertex value"
	default:
		panic(fmt.Sprintf("unexpected type %T", v))
	}
}

type Vertex struct {
	Value   interface{} // one of Constraint or ssa.Value
	SCC     int
	index   int
	lowlink int
	stack   bool
}

type Graph struct {
	Vertices map[interface{}]*Vertex
	Edges    []Edge
	SCCs     [][]*Vertex
	ranges   map[ssa.Value]Range
}

func (g Graph) Graphviz() string {
	var lines []string
	lines = append(lines, "digraph{")
	ids := map[interface{}]int{}
	i := 1
	for _, v := range g.Vertices {
		ids[v] = i
		shape := "box"
		if _, ok := v.Value.(ssa.Value); ok {
			shape = "oval"
		}
		lines = append(lines, fmt.Sprintf(`n%d [shape="%s", label="%s", colorscheme=spectral11, style="filled", fillcolor="%d"]`,
			i, shape, VerticeString(v), (v.SCC%11)+1))
		i++
	}
	for _, e := range g.Edges {
		style := "solid"
		if e.control {
			style = "dashed"
		}
		lines = append(lines, fmt.Sprintf(`n%d -> n%d [style="%s"]`, ids[e.From], ids[e.To], style))
	}
	lines = append(lines, "}")
	return strings.Join(lines, "\n")
}

func (g *Graph) SetRange(x ssa.Value, r Range) {
	g.ranges[x] = r
}

func (g *Graph) Range(x ssa.Value) Range {
	// XXX we shouldn't have to do this. all constants should exist in
	// the graph already.
	if x, ok := x.(*ssa.Const); ok {
		switch typ := x.Type().Underlying().(type) {
		case *types.Basic:
			if (typ.Info() & types.IsInteger) != 0 {
				v, _ := constant.Int64Val(x.Value)
				return NewInterval(NewZ(big.NewInt(v)), NewZ(big.NewInt(v)))
			}
		}
	}
	i, ok := g.ranges[x]
	if !ok {
		switch x := x.Type().Underlying().(type) {
		case *types.Basic:
			switch x.Kind() {
			case types.String:
				return StringRange{}
			default:
				return Interval{}
			}
		}
	}
	return i
}

func (g *Graph) Ranges() map[ssa.Value]Range {
	return g.ranges
}

func (g *Graph) widen(c Constraint, consts []Z) bool {
	switch oi := g.Range(c.Y()).(type) {
	case Interval:
		ni := c.Eval(g).(Interval)
		if !ni.IsKnown() {
			return false
		}
		setRange := func(i Interval) {
			g.SetRange(c.Y(), i)
		}
		nlc := NInfinity
		nuc := PInfinity
		for _, co := range consts {
			if co.Cmp(ni.lower) == -1 {
				nlc = co
				break
			}
		}
		for _, co := range consts {
			if co.Cmp(ni.upper) == 1 {
				nuc = co
				break
			}
		}

		if !oi.IsKnown() {
			setRange(ni)
			return true
		}
		if ni.lower.Cmp(oi.lower) == -1 && ni.upper.Cmp(oi.upper) == 1 {
			setRange(NewInterval(nlc, nuc))
			return true
		}
		if ni.lower.Cmp(oi.lower) == -1 {
			setRange(NewInterval(nlc, oi.upper))
			return true
		}
		if ni.upper.Cmp(oi.upper) == 1 {
			setRange(NewInterval(oi.lower, nuc))
			return true
		}
		return false
	default:
		return false
	}
}

func (g *Graph) narrow(c Constraint, consts []Z) bool {
	if _, ok := g.Range(c.Y()).(Interval); !ok {
		return false
	}
	oLower := g.Range(c.Y()).(Interval).lower
	oUpper := g.Range(c.Y()).(Interval).upper
	newInterval := c.Eval(g).(Interval)

	nLower := newInterval.lower
	nUpper := newInterval.upper

	hasChanged := false
	if oLower == NInfinity && nLower != NInfinity {
		g.SetRange(c.Y(), NewInterval(nLower, oUpper))
		hasChanged = true
	} else {
		smin := Min(oLower, nLower)
		if oLower != smin {
			g.SetRange(c.Y(), NewInterval(smin, oUpper))
			hasChanged = true
		}
	}

	if oUpper == PInfinity && nUpper != PInfinity {
		g.SetRange(c.Y(), NewInterval(g.ranges[c.Y()].(Interval).lower, nUpper))
		hasChanged = true
	} else {
		smax := Max(oUpper, nUpper)
		if oUpper != smax {
			g.SetRange(c.Y(), NewInterval(g.ranges[c.Y()].(Interval).lower, smax))
			hasChanged = true
		}
	}
	return hasChanged
}

func (g *Graph) resolveFutures(scc int) {
	// XXX use a map instead of O(n)
	for _, e := range g.Edges {
		if !e.control {
			continue
		}
		if e.From.SCC != scc {
			continue
		}
		if c, ok := e.To.Value.(*FutureIntersectionConstraint); ok {
			c.Resolve()
		}
	}
}

func (g *Graph) entries(scc int) []ssa.Value {
	var entries []ssa.Value
	for _, n := range g.Vertices {
		if n.SCC != scc {
			continue
		}
		if v, ok := n.Value.(ssa.Value); ok {
			// XXX avoid quadratic runtime
			//
			// XXX I cannot think of any code where the future and its
			// variables aren't in the same SCC, in which case this
			// code isn't very useful (the variables won't be resolved
			// yet). Before we have a cross-SCC example, however, we
			// can't really verify that this code is working
			// correctly, or indeed doing anything useful.
			for _, on := range g.Vertices {
				if c, ok := on.Value.(*FutureIntersectionConstraint); ok {
					if c.Y() == v {
						if !c.resolved {
							g.SetRange(c.Y(), c.Eval(g))
							c.resolved = true
						}
						break
					}
				}
			}
			if g.Range(v).IsKnown() {
				entries = append(entries, v)
			}
		}
	}
	return entries
}

func (g *Graph) uses(scc int) map[ssa.Value][]Constraint {
	m := map[ssa.Value][]Constraint{}
	for _, e := range g.Edges {
		if e.control {
			continue
		}
		if e.From.SCC != scc {
			continue
		}
		if v, ok := e.From.Value.(ssa.Value); ok {
			c := e.To.Value.(Constraint)
			sink := c.Y()
			if g.Vertices[sink].SCC == scc {
				m[v] = append(m[v], c)
			}
		}
	}
	return m
}

func (g *Graph) actives(scc int) []ssa.Value {
	var actives []ssa.Value
	for _, n := range g.Vertices {
		if n.SCC != scc {
			continue
		}
		if v, ok := n.Value.(ssa.Value); ok {
			if _, ok := v.(*ssa.Const); !ok {
				actives = append(actives, v)
			}
		}
	}
	return actives
}

func (g *Graph) AddEdge(from, to interface{}, ctrl bool) {
	vf, ok := g.Vertices[from]
	if !ok {
		vf = &Vertex{Value: from}
		g.Vertices[from] = vf
	}
	vt, ok := g.Vertices[to]
	if !ok {
		vt = &Vertex{Value: to}
		g.Vertices[to] = vt
	}
	g.Edges = append(g.Edges, Edge{From: vf, To: vt, control: ctrl})
}

type Edge struct {
	From, To *Vertex
	control  bool
}

func (e Edge) String() string {
	return fmt.Sprintf("%s -> %s", VerticeString(e.From), VerticeString(e.To))
}

func (g *Graph) FindSCCs() {
	// use Tarjan to find the SCCs

	index := 1
	var s []*Vertex

	scc := 0
	var strongconnect func(v *Vertex)
	strongconnect = func(v *Vertex) {
		// set the depth index for v to the smallest unused index
		v.index = index
		v.lowlink = index
		index++
		s = append(s, v)
		v.stack = true

		for _, e := range g.Edges {
			if e.From != v {
				continue
			}
			w := e.To
			if w.index == 0 {
				// successor w has not yet been visited; recurse on it
				strongconnect(w)
				if w.lowlink < v.lowlink {
					v.lowlink = w.lowlink
				}
			} else if w.stack {
				// successor w is in stack s and hence in the current scc
				if w.index < v.lowlink {
					v.lowlink = w.index
				}
			}
		}

		if v.lowlink == v.index {
			for {
				w := s[len(s)-1]
				s = s[:len(s)-1]
				w.stack = false
				w.SCC = scc
				if w == v {
					break
				}
			}
			scc++
		}
	}
	for _, v := range g.Vertices {
		if v.index == 0 {
			strongconnect(v)
		}
	}

	g.SCCs = make([][]*Vertex, scc)
	for _, n := range g.Vertices {
		n.SCC = scc - n.SCC - 1
		g.SCCs[n.SCC] = append(g.SCCs[n.SCC], n)
	}
}

func invertToken(tok token.Token) token.Token {
	switch tok {
	case token.LSS:
		return token.GEQ
	case token.GTR:
		return token.LEQ
	case token.EQL:
		return token.NEQ
	case token.NEQ:
		return token.EQL
	case token.GEQ:
		return token.LSS
	case token.LEQ:
		return token.GTR
	default:
		panic(fmt.Sprintf("unsupported token %s", tok))
	}
}

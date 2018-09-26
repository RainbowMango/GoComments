// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/internal/src"
)

// fuse simplifies control flow by joining basic blocks.
func fuse(f *Func) {
	for changed := true; changed; {
		changed = false
		// Fuse from end to beginning, to avoid quadratic behavior in fuseBlockPlain. See issue 13554.
		for i := len(f.Blocks) - 1; i >= 0; i-- {
			b := f.Blocks[i]
			changed = fuseBlockIf(b) || changed
			changed = fuseBlockPlain(b) || changed
		}
	}
}

// fuseBlockIf handles the following cases where s0 and s1 are empty blocks.
//
//   b        b        b      b
//  / \      | \      / |    | |
// s0  s1    |  s1   s0 |    | |
//  \ /      | /      \ |    | |
//   ss      ss        ss     ss
//
// If all Phi ops in ss have identical variables for slots corresponding to
// s0, s1 and b then the branch can be dropped.
// This optimization often comes up in switch statements with multiple
// expressions in a case clause:
//   switch n {
//     case 1,2,3: return 4
//   }
// TODO: If ss doesn't contain any OpPhis, are s0 and s1 dead code anyway.
func fuseBlockIf(b *Block) bool {
	if b.Kind != BlockIf {
		return false
	}

	var ss0, ss1 *Block
	s0 := b.Succs[0].b
	i0 := b.Succs[0].i
	if s0.Kind != BlockPlain || len(s0.Preds) != 1 || len(s0.Values) != 0 {
		s0, ss0 = b, s0
	} else {
		ss0 = s0.Succs[0].b
		i0 = s0.Succs[0].i
	}
	s1 := b.Succs[1].b
	i1 := b.Succs[1].i
	if s1.Kind != BlockPlain || len(s1.Preds) != 1 || len(s1.Values) != 0 {
		s1, ss1 = b, s1
	} else {
		ss1 = s1.Succs[0].b
		i1 = s1.Succs[0].i
	}

	if ss0 != ss1 {
		return false
	}
	ss := ss0

	// s0 and s1 are equal with b if the corresponding block is missing
	// (2nd, 3rd and 4th case in the figure).

	for _, v := range ss.Values {
		if v.Op == OpPhi && v.Uses > 0 && v.Args[i0] != v.Args[i1] {
			return false
		}
	}

	// Now we have two of following b->ss, b->s0->ss and b->s1->ss,
	// with s0 and s1 empty if exist.
	// We can replace it with b->ss without if all OpPhis in ss
	// have identical predecessors (verified above).
	// No critical edge is introduced because b will have one successor.
	if s0 != b && s1 != b {
		// Replace edge b->s0->ss with b->ss.
		// We need to keep a slot for Phis corresponding to b.
		b.Succs[0] = Edge{ss, i0}
		ss.Preds[i0] = Edge{b, 0}
		b.removeEdge(1)
		s1.removeEdge(0)
	} else if s0 != b {
		b.removeEdge(0)
		s0.removeEdge(0)
	} else if s1 != b {
		b.removeEdge(1)
		s1.removeEdge(0)
	} else {
		b.removeEdge(1)
	}
	b.Kind = BlockPlain
	b.Likely = BranchUnknown
	b.SetControl(nil)

	// Trash the empty blocks s0 & s1.
	if s0 != b {
		s0.Kind = BlockInvalid
		s0.Values = nil
		s0.Succs = nil
		s0.Preds = nil
	}
	if s1 != b {
		s1.Kind = BlockInvalid
		s1.Values = nil
		s1.Succs = nil
		s1.Preds = nil
	}
	return true
}

func fuseBlockPlain(b *Block) bool {
	if b.Kind != BlockPlain {
		return false
	}

	c := b.Succs[0].b
	if len(c.Preds) != 1 {
		return false
	}

	// If a block happened to end in a statement marker,
	// try to preserve it.
	if b.Pos.IsStmt() == src.PosIsStmt {
		l := b.Pos.Line()
		for _, v := range c.Values {
			if v.Pos.IsStmt() == src.PosNotStmt {
				continue
			}
			if l == v.Pos.Line() {
				v.Pos = v.Pos.WithIsStmt()
				l = 0
				break
			}
		}
		if l != 0 && c.Pos.Line() == l {
			c.Pos = c.Pos.WithIsStmt()
		}
	}

	// move all of b's values to c.
	for _, v := range b.Values {
		v.Block = c
	}
	// Use whichever value slice is larger, in the hopes of avoiding growth.
	// However, take care to avoid c.Values pointing to b.valstorage.
	// See golang.org/issue/18602.
	// It's important to keep the elements in the same order; maintenance of
	// debugging information depends on the order of *Values in Blocks.
	// This can also cause changes in the order (which may affect other
	// optimizations and possibly compiler output) for 32-vs-64 bit compilation
	// platforms (word size affects allocation bucket size affects slice capacity).
	if cap(c.Values) >= cap(b.Values) || len(b.Values) <= len(b.valstorage) {
		bl := len(b.Values)
		cl := len(c.Values)
		var t []*Value // construct t = b.Values followed-by c.Values, but with attention to allocation.
		if cap(c.Values) < bl+cl {
			// reallocate
			t = make([]*Value, bl+cl)
		} else {
			// in place.
			t = c.Values[0 : bl+cl]
		}
		copy(t[bl:], c.Values) // possibly in-place
		c.Values = t
		copy(c.Values, b.Values)
	} else {
		c.Values = append(b.Values, c.Values...)
	}

	// replace b->c edge with preds(b) -> c
	c.predstorage[0] = Edge{}
	if len(b.Preds) > len(b.predstorage) {
		c.Preds = b.Preds
	} else {
		c.Preds = append(c.predstorage[:0], b.Preds...)
	}
	for i, e := range c.Preds {
		p := e.b
		p.Succs[e.i] = Edge{c, i}
	}
	f := b.Func
	if f.Entry == b {
		f.Entry = c
	}
	f.invalidateCFG()

	// trash b, just in case
	b.Kind = BlockInvalid
	b.Values = nil
	b.Preds = nil
	b.Succs = nil
	return true
}

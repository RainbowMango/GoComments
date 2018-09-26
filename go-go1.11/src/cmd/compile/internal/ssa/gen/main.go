// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build ignore

// The gen command generates Go code (in the parent directory) for all
// the architecture-specific opcodes, blocks, and rewrites.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"log"
	"path"
	"regexp"
	"sort"
	"strings"
)

type arch struct {
	name            string
	pkg             string // obj package to import for this arch.
	genfile         string // source file containing opcode code generation.
	ops             []opData
	blocks          []blockData
	regnames        []string
	gpregmask       regMask
	fpregmask       regMask
	specialregmask  regMask
	framepointerreg int8
	linkreg         int8
	generic         bool
}

type opData struct {
	name              string
	reg               regInfo
	asm               string
	typ               string // default result type
	aux               string
	rematerializeable bool
	argLength         int32  // number of arguments, if -1, then this operation has a variable number of arguments
	commutative       bool   // this operation is commutative on its first 2 arguments (e.g. addition)
	resultInArg0      bool   // (first, if a tuple) output of v and v.Args[0] must be allocated to the same register
	resultNotInArgs   bool   // outputs must not be allocated to the same registers as inputs
	clobberFlags      bool   // this op clobbers flags register
	call              bool   // is a function call
	nilCheck          bool   // this op is a nil check on arg0
	faultOnNilArg0    bool   // this op will fault if arg0 is nil (and aux encodes a small offset)
	faultOnNilArg1    bool   // this op will fault if arg1 is nil (and aux encodes a small offset)
	usesScratch       bool   // this op requires scratch memory space
	hasSideEffects    bool   // for "reasons", not to be eliminated.  E.g., atomic store, #19182.
	zeroWidth         bool   // op never translates into any machine code. example: copy, which may sometimes translate to machine code, is not zero-width.
	symEffect         string // effect this op has on symbol in aux
}

type blockData struct {
	name string
}

type regInfo struct {
	inputs   []regMask
	clobbers regMask
	outputs  []regMask
}

type regMask uint64

func (a arch) regMaskComment(r regMask) string {
	var buf bytes.Buffer
	for i := uint64(0); r != 0; i++ {
		if r&1 != 0 {
			if buf.Len() == 0 {
				buf.WriteString(" //")
			}
			buf.WriteString(" ")
			buf.WriteString(a.regnames[i])
		}
		r >>= 1
	}
	return buf.String()
}

var archs []arch

func main() {
	flag.Parse()
	sort.Sort(ArchsByName(archs))
	genOp()
	genLower()
}

func genOp() {
	w := new(bytes.Buffer)
	fmt.Fprintf(w, "// Code generated from gen/*Ops.go; DO NOT EDIT.\n")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "package ssa")

	fmt.Fprintln(w, "import (")
	fmt.Fprintln(w, "\"cmd/internal/obj\"")
	for _, a := range archs {
		if a.pkg != "" {
			fmt.Fprintf(w, "%q\n", a.pkg)
		}
	}
	fmt.Fprintln(w, ")")

	// generate Block* declarations
	fmt.Fprintln(w, "const (")
	fmt.Fprintln(w, "BlockInvalid BlockKind = iota")
	for _, a := range archs {
		fmt.Fprintln(w)
		for _, d := range a.blocks {
			fmt.Fprintf(w, "Block%s%s\n", a.Name(), d.name)
		}
	}
	fmt.Fprintln(w, ")")

	// generate block kind string method
	fmt.Fprintln(w, "var blockString = [...]string{")
	fmt.Fprintln(w, "BlockInvalid:\"BlockInvalid\",")
	for _, a := range archs {
		fmt.Fprintln(w)
		for _, b := range a.blocks {
			fmt.Fprintf(w, "Block%s%s:\"%s\",\n", a.Name(), b.name, b.name)
		}
	}
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w, "func (k BlockKind) String() string {return blockString[k]}")

	// generate Op* declarations
	fmt.Fprintln(w, "const (")
	fmt.Fprintln(w, "OpInvalid Op = iota") // make sure OpInvalid is 0.
	for _, a := range archs {
		fmt.Fprintln(w)
		for _, v := range a.ops {
			if v.name == "Invalid" {
				continue
			}
			fmt.Fprintf(w, "Op%s%s\n", a.Name(), v.name)
		}
	}
	fmt.Fprintln(w, ")")

	// generate OpInfo table
	fmt.Fprintln(w, "var opcodeTable = [...]opInfo{")
	fmt.Fprintln(w, " { name: \"OpInvalid\" },")
	for _, a := range archs {
		fmt.Fprintln(w)

		pkg := path.Base(a.pkg)
		for _, v := range a.ops {
			if v.name == "Invalid" {
				continue
			}
			fmt.Fprintln(w, "{")
			fmt.Fprintf(w, "name:\"%s\",\n", v.name)

			// flags
			if v.aux != "" {
				fmt.Fprintf(w, "auxType: aux%s,\n", v.aux)
			}
			fmt.Fprintf(w, "argLen: %d,\n", v.argLength)

			if v.rematerializeable {
				if v.reg.clobbers != 0 {
					log.Fatalf("%s is rematerializeable and clobbers registers", v.name)
				}
				if v.clobberFlags {
					log.Fatalf("%s is rematerializeable and clobbers flags", v.name)
				}
				fmt.Fprintln(w, "rematerializeable: true,")
			}
			if v.commutative {
				fmt.Fprintln(w, "commutative: true,")
			}
			if v.resultInArg0 {
				fmt.Fprintln(w, "resultInArg0: true,")
				// OpConvert's register mask is selected dynamically,
				// so don't try to check it in the static table.
				if v.name != "Convert" && v.reg.inputs[0] != v.reg.outputs[0] {
					log.Fatalf("%s: input[0] and output[0] must use the same registers for %s", a.name, v.name)
				}
				if v.name != "Convert" && v.commutative && v.reg.inputs[1] != v.reg.outputs[0] {
					log.Fatalf("%s: input[1] and output[0] must use the same registers for %s", a.name, v.name)
				}
			}
			if v.resultNotInArgs {
				fmt.Fprintln(w, "resultNotInArgs: true,")
			}
			if v.clobberFlags {
				fmt.Fprintln(w, "clobberFlags: true,")
			}
			if v.call {
				fmt.Fprintln(w, "call: true,")
			}
			if v.nilCheck {
				fmt.Fprintln(w, "nilCheck: true,")
			}
			if v.faultOnNilArg0 {
				fmt.Fprintln(w, "faultOnNilArg0: true,")
				if v.aux != "SymOff" && v.aux != "SymValAndOff" && v.aux != "Int64" && v.aux != "Int32" && v.aux != "" {
					log.Fatalf("faultOnNilArg0 with aux %s not allowed", v.aux)
				}
			}
			if v.faultOnNilArg1 {
				fmt.Fprintln(w, "faultOnNilArg1: true,")
				if v.aux != "SymOff" && v.aux != "SymValAndOff" && v.aux != "Int64" && v.aux != "Int32" && v.aux != "" {
					log.Fatalf("faultOnNilArg1 with aux %s not allowed", v.aux)
				}
			}
			if v.usesScratch {
				fmt.Fprintln(w, "usesScratch: true,")
			}
			if v.hasSideEffects {
				fmt.Fprintln(w, "hasSideEffects: true,")
			}
			if v.zeroWidth {
				fmt.Fprintln(w, "zeroWidth: true,")
			}
			needEffect := strings.HasPrefix(v.aux, "Sym")
			if v.symEffect != "" {
				if !needEffect {
					log.Fatalf("symEffect with aux %s not allowed", v.aux)
				}
				fmt.Fprintf(w, "symEffect: Sym%s,\n", strings.Replace(v.symEffect, ",", "|Sym", -1))
			} else if needEffect {
				log.Fatalf("symEffect needed for aux %s", v.aux)
			}
			if a.name == "generic" {
				fmt.Fprintln(w, "generic:true,")
				fmt.Fprintln(w, "},") // close op
				// generic ops have no reg info or asm
				continue
			}
			if v.asm != "" {
				fmt.Fprintf(w, "asm: %s.A%s,\n", pkg, v.asm)
			}
			fmt.Fprintln(w, "reg:regInfo{")

			// Compute input allocation order. We allocate from the
			// most to the least constrained input. This order guarantees
			// that we will always be able to find a register.
			var s []intPair
			for i, r := range v.reg.inputs {
				if r != 0 {
					s = append(s, intPair{countRegs(r), i})
				}
			}
			if len(s) > 0 {
				sort.Sort(byKey(s))
				fmt.Fprintln(w, "inputs: []inputInfo{")
				for _, p := range s {
					r := v.reg.inputs[p.val]
					fmt.Fprintf(w, "{%d,%d},%s\n", p.val, r, a.regMaskComment(r))
				}
				fmt.Fprintln(w, "},")
			}

			if v.reg.clobbers > 0 {
				fmt.Fprintf(w, "clobbers: %d,%s\n", v.reg.clobbers, a.regMaskComment(v.reg.clobbers))
			}

			// reg outputs
			s = s[:0]
			for i, r := range v.reg.outputs {
				s = append(s, intPair{countRegs(r), i})
			}
			if len(s) > 0 {
				sort.Sort(byKey(s))
				fmt.Fprintln(w, "outputs: []outputInfo{")
				for _, p := range s {
					r := v.reg.outputs[p.val]
					fmt.Fprintf(w, "{%d,%d},%s\n", p.val, r, a.regMaskComment(r))
				}
				fmt.Fprintln(w, "},")
			}
			fmt.Fprintln(w, "},") // close reg info
			fmt.Fprintln(w, "},") // close op
		}
	}
	fmt.Fprintln(w, "}")

	fmt.Fprintln(w, "func (o Op) Asm() obj.As {return opcodeTable[o].asm}")

	// generate op string method
	fmt.Fprintln(w, "func (o Op) String() string {return opcodeTable[o].name }")

	fmt.Fprintln(w, "func (o Op) UsesScratch() bool { return opcodeTable[o].usesScratch }")

	fmt.Fprintln(w, "func (o Op) SymEffect() SymEffect { return opcodeTable[o].symEffect }")
	fmt.Fprintln(w, "func (o Op) IsCall() bool { return opcodeTable[o].call }")

	// generate registers
	for _, a := range archs {
		if a.generic {
			continue
		}
		fmt.Fprintf(w, "var registers%s = [...]Register {\n", a.name)
		var gcRegN int
		for i, r := range a.regnames {
			pkg := a.pkg[len("cmd/internal/obj/"):]
			var objname string // name in cmd/internal/obj/$ARCH
			switch r {
			case "SB":
				// SB isn't a real register.  cmd/internal/obj expects 0 in this case.
				objname = "0"
			case "SP":
				objname = pkg + ".REGSP"
			case "g":
				objname = pkg + ".REGG"
			default:
				objname = pkg + ".REG_" + r
			}
			// Assign a GC register map index to registers
			// that may contain pointers.
			gcRegIdx := -1
			if a.gpregmask&(1<<uint(i)) != 0 {
				gcRegIdx = gcRegN
				gcRegN++
			}
			fmt.Fprintf(w, "  {%d, %s, %d, \"%s\"},\n", i, objname, gcRegIdx, r)
		}
		if gcRegN > 32 {
			// Won't fit in a uint32 mask.
			log.Fatalf("too many GC registers (%d > 32) on %s", gcRegN, a.name)
		}
		fmt.Fprintln(w, "}")
		fmt.Fprintf(w, "var gpRegMask%s = regMask(%d)\n", a.name, a.gpregmask)
		fmt.Fprintf(w, "var fpRegMask%s = regMask(%d)\n", a.name, a.fpregmask)
		fmt.Fprintf(w, "var specialRegMask%s = regMask(%d)\n", a.name, a.specialregmask)
		fmt.Fprintf(w, "var framepointerReg%s = int8(%d)\n", a.name, a.framepointerreg)
		fmt.Fprintf(w, "var linkReg%s = int8(%d)\n", a.name, a.linkreg)
	}

	// gofmt result
	b := w.Bytes()
	var err error
	b, err = format.Source(b)
	if err != nil {
		fmt.Printf("%s\n", w.Bytes())
		panic(err)
	}

	err = ioutil.WriteFile("../opGen.go", b, 0666)
	if err != nil {
		log.Fatalf("can't write output: %v\n", err)
	}

	// Check that the arch genfile handles all the arch-specific opcodes.
	// This is very much a hack, but it is better than nothing.
	for _, a := range archs {
		if a.genfile == "" {
			continue
		}

		src, err := ioutil.ReadFile(a.genfile)
		if err != nil {
			log.Fatalf("can't read %s: %v", a.genfile, err)
		}

		for _, v := range a.ops {
			pattern := fmt.Sprintf("\\Wssa[.]Op%s%s\\W", a.name, v.name)
			match, err := regexp.Match(pattern, src)
			if err != nil {
				log.Fatalf("bad opcode regexp %s: %v", pattern, err)
			}
			if !match {
				log.Fatalf("Op%s%s has no code generation in %s", a.name, v.name, a.genfile)
			}
		}
	}
}

// Name returns the name of the architecture for use in Op* and Block* enumerations.
func (a arch) Name() string {
	s := a.name
	if s == "generic" {
		s = ""
	}
	return s
}

func genLower() {
	for _, a := range archs {
		genRules(a)
	}
}

// countRegs returns the number of set bits in the register mask.
func countRegs(r regMask) int {
	n := 0
	for r != 0 {
		n += int(r & 1)
		r >>= 1
	}
	return n
}

// for sorting a pair of integers by key
type intPair struct {
	key, val int
}
type byKey []intPair

func (a byKey) Len() int           { return len(a) }
func (a byKey) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byKey) Less(i, j int) bool { return a[i].key < a[j].key }

type ArchsByName []arch

func (x ArchsByName) Len() int           { return len(x) }
func (x ArchsByName) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
func (x ArchsByName) Less(i, j int) bool { return x[i].name < x[j].name }

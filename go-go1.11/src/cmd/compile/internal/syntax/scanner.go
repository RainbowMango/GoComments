// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements scanner, a lexical tokenizer for
// Go source. After initialization, consecutive calls of
// next advance the scanner one token at a time.
//
// This file, source.go, and tokens.go are self-contained
// (go tool compile scanner.go source.go tokens.go compiles)
// and thus could be made into its own package.

package syntax

import (
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"
)

// The mode flags below control which comments are reported
// by calling the error handler. If no flag is set, comments
// are ignored.
const (
	comments   uint = 1 << iota // call handler for all comments
	directives                  // call handler for directives only
)

type scanner struct {
	source
	mode   uint
	nlsemi bool // if set '\n' and EOF translate to ';'

	// current token, valid after calling next()
	line, col uint
	tok       token
	lit       string   // valid if tok is _Name, _Literal, or _Semi ("semicolon", "newline", or "EOF")
	kind      LitKind  // valid if tok is _Literal
	op        Operator // valid if tok is _Operator, _AssignOp, or _IncOp
	prec      int      // valid if tok is _Operator, _AssignOp, or _IncOp
}

func (s *scanner) init(src io.Reader, errh func(line, col uint, msg string), mode uint) {
	s.source.init(src, errh)
	s.mode = mode
	s.nlsemi = false
}

// next advances the scanner by reading the next token.
//
// If a read, source encoding, or lexical error occurs, next calls
// the installed error handler with the respective error position
// and message. The error message is guaranteed to be non-empty and
// never starts with a '/'. The error handler must exist.
//
// If the scanner mode includes the comments flag and a comment
// (including comments containing directives) is encountered, the
// error handler is also called with each comment position and text
// (including opening /* or // and closing */, but without a newline
// at the end of line comments). Comment text always starts with a /
// which can be used to distinguish these handler calls from errors.
//
// If the scanner mode includes the directives (but not the comments)
// flag, only comments containing a //line, /*line, or //go: directive
// are reported, in the same way as regular comments. Directives in
// //-style comments are only recognized if they are at the beginning
// of a line.
//
func (s *scanner) next() {
	nlsemi := s.nlsemi
	s.nlsemi = false

redo:
	// skip white space
	c := s.getr()
	for c == ' ' || c == '\t' || c == '\n' && !nlsemi || c == '\r' {
		c = s.getr()
	}

	// token start
	s.line, s.col = s.source.line0, s.source.col0

	if isLetter(c) || c >= utf8.RuneSelf && s.isIdentRune(c, true) {
		s.ident()
		return
	}

	switch c {
	case -1:
		if nlsemi {
			s.lit = "EOF"
			s.tok = _Semi
			break
		}
		s.tok = _EOF

	case '\n':
		s.lit = "newline"
		s.tok = _Semi

	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		s.number(c)

	case '"':
		s.stdString()

	case '`':
		s.rawString()

	case '\'':
		s.rune()

	case '(':
		s.tok = _Lparen

	case '[':
		s.tok = _Lbrack

	case '{':
		s.tok = _Lbrace

	case ',':
		s.tok = _Comma

	case ';':
		s.lit = "semicolon"
		s.tok = _Semi

	case ')':
		s.nlsemi = true
		s.tok = _Rparen

	case ']':
		s.nlsemi = true
		s.tok = _Rbrack

	case '}':
		s.nlsemi = true
		s.tok = _Rbrace

	case ':':
		if s.getr() == '=' {
			s.tok = _Define
			break
		}
		s.ungetr()
		s.tok = _Colon

	case '.':
		c = s.getr()
		if isDigit(c) {
			s.ungetr2()
			s.number('.')
			break
		}
		if c == '.' {
			c = s.getr()
			if c == '.' {
				s.tok = _DotDotDot
				break
			}
			s.ungetr2()
		}
		s.ungetr()
		s.tok = _Dot

	case '+':
		s.op, s.prec = Add, precAdd
		c = s.getr()
		if c != '+' {
			goto assignop
		}
		s.nlsemi = true
		s.tok = _IncOp

	case '-':
		s.op, s.prec = Sub, precAdd
		c = s.getr()
		if c != '-' {
			goto assignop
		}
		s.nlsemi = true
		s.tok = _IncOp

	case '*':
		s.op, s.prec = Mul, precMul
		// don't goto assignop - want _Star token
		if s.getr() == '=' {
			s.tok = _AssignOp
			break
		}
		s.ungetr()
		s.tok = _Star

	case '/':
		c = s.getr()
		if c == '/' {
			s.lineComment()
			goto redo
		}
		if c == '*' {
			s.fullComment()
			if s.source.line > s.line && nlsemi {
				// A multi-line comment acts like a newline;
				// it translates to a ';' if nlsemi is set.
				s.lit = "newline"
				s.tok = _Semi
				break
			}
			goto redo
		}
		s.op, s.prec = Div, precMul
		goto assignop

	case '%':
		s.op, s.prec = Rem, precMul
		c = s.getr()
		goto assignop

	case '&':
		c = s.getr()
		if c == '&' {
			s.op, s.prec = AndAnd, precAndAnd
			s.tok = _Operator
			break
		}
		s.op, s.prec = And, precMul
		if c == '^' {
			s.op = AndNot
			c = s.getr()
		}
		goto assignop

	case '|':
		c = s.getr()
		if c == '|' {
			s.op, s.prec = OrOr, precOrOr
			s.tok = _Operator
			break
		}
		s.op, s.prec = Or, precAdd
		goto assignop

	case '^':
		s.op, s.prec = Xor, precAdd
		c = s.getr()
		goto assignop

	case '<':
		c = s.getr()
		if c == '=' {
			s.op, s.prec = Leq, precCmp
			s.tok = _Operator
			break
		}
		if c == '<' {
			s.op, s.prec = Shl, precMul
			c = s.getr()
			goto assignop
		}
		if c == '-' {
			s.tok = _Arrow
			break
		}
		s.ungetr()
		s.op, s.prec = Lss, precCmp
		s.tok = _Operator

	case '>':
		c = s.getr()
		if c == '=' {
			s.op, s.prec = Geq, precCmp
			s.tok = _Operator
			break
		}
		if c == '>' {
			s.op, s.prec = Shr, precMul
			c = s.getr()
			goto assignop
		}
		s.ungetr()
		s.op, s.prec = Gtr, precCmp
		s.tok = _Operator

	case '=':
		if s.getr() == '=' {
			s.op, s.prec = Eql, precCmp
			s.tok = _Operator
			break
		}
		s.ungetr()
		s.tok = _Assign

	case '!':
		if s.getr() == '=' {
			s.op, s.prec = Neq, precCmp
			s.tok = _Operator
			break
		}
		s.ungetr()
		s.op, s.prec = Not, 0
		s.tok = _Operator

	default:
		s.tok = 0
		s.error(fmt.Sprintf("invalid character %#U", c))
		goto redo
	}

	return

assignop:
	if c == '=' {
		s.tok = _AssignOp
		return
	}
	s.ungetr()
	s.tok = _Operator
}

func isLetter(c rune) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_'
}

func isDigit(c rune) bool {
	return '0' <= c && c <= '9'
}

func (s *scanner) ident() {
	s.startLit()

	// accelerate common case (7bit ASCII)
	c := s.getr()
	for isLetter(c) || isDigit(c) {
		c = s.getr()
	}

	// general case
	if c >= utf8.RuneSelf {
		for s.isIdentRune(c, false) {
			c = s.getr()
		}
	}
	s.ungetr()

	lit := s.stopLit()

	// possibly a keyword
	if len(lit) >= 2 {
		if tok := keywordMap[hash(lit)]; tok != 0 && tokStrFast(tok) == string(lit) {
			s.nlsemi = contains(1<<_Break|1<<_Continue|1<<_Fallthrough|1<<_Return, tok)
			s.tok = tok
			return
		}
	}

	s.nlsemi = true
	s.lit = string(lit)
	s.tok = _Name
}

// tokStrFast is a faster version of token.String, which assumes that tok
// is one of the valid tokens - and can thus skip bounds checks.
func tokStrFast(tok token) string {
	return _token_name[_token_index[tok-1]:_token_index[tok]]
}

func (s *scanner) isIdentRune(c rune, first bool) bool {
	switch {
	case unicode.IsLetter(c) || c == '_':
		// ok
	case unicode.IsDigit(c):
		if first {
			s.error(fmt.Sprintf("identifier cannot begin with digit %#U", c))
		}
	case c >= utf8.RuneSelf:
		s.error(fmt.Sprintf("invalid identifier character %#U", c))
	default:
		return false
	}
	return true
}

// hash is a perfect hash function for keywords.
// It assumes that s has at least length 2.
func hash(s []byte) uint {
	return (uint(s[0])<<4 ^ uint(s[1]) + uint(len(s))) & uint(len(keywordMap)-1)
}

var keywordMap [1 << 6]token // size must be power of two

func init() {
	// populate keywordMap
	for tok := _Break; tok <= _Var; tok++ {
		h := hash([]byte(tok.String()))
		if keywordMap[h] != 0 {
			panic("imperfect hash")
		}
		keywordMap[h] = tok
	}
}

func (s *scanner) number(c rune) {
	s.startLit()

	if c != '.' {
		s.kind = IntLit // until proven otherwise
		if c == '0' {
			c = s.getr()
			if c == 'x' || c == 'X' {
				// hex
				c = s.getr()
				hasDigit := false
				for isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' {
					c = s.getr()
					hasDigit = true
				}
				if !hasDigit {
					s.error("malformed hex constant")
				}
				goto done
			}

			// decimal 0, octal, or float
			has8or9 := false
			for isDigit(c) {
				if c > '7' {
					has8or9 = true
				}
				c = s.getr()
			}
			if c != '.' && c != 'e' && c != 'E' && c != 'i' {
				// octal
				if has8or9 {
					s.error("malformed octal constant")
				}
				goto done
			}

		} else {
			// decimal or float
			for isDigit(c) {
				c = s.getr()
			}
		}
	}

	// float
	if c == '.' {
		s.kind = FloatLit
		c = s.getr()
		for isDigit(c) {
			c = s.getr()
		}
	}

	// exponent
	if c == 'e' || c == 'E' {
		s.kind = FloatLit
		c = s.getr()
		if c == '-' || c == '+' {
			c = s.getr()
		}
		if !isDigit(c) {
			s.error("malformed floating-point constant exponent")
		}
		for isDigit(c) {
			c = s.getr()
		}
	}

	// complex
	if c == 'i' {
		s.kind = ImagLit
		s.getr()
	}

done:
	s.ungetr()
	s.nlsemi = true
	s.lit = string(s.stopLit())
	s.tok = _Literal
}

func (s *scanner) rune() {
	s.startLit()

	ok := true // only report errors if we're ok so far
	n := 0
	for ; ; n++ {
		r := s.getr()
		if r == '\'' {
			break
		}
		if r == '\\' {
			if !s.escape('\'') {
				ok = false
			}
			continue
		}
		if r == '\n' {
			s.ungetr() // assume newline is not part of literal
			if ok {
				s.error("newline in character literal")
				ok = false
			}
			break
		}
		if r < 0 {
			if ok {
				s.errh(s.line, s.col, "invalid character literal (missing closing ')")
				ok = false
			}
			break
		}
	}

	if ok {
		if n == 0 {
			s.error("empty character literal or unescaped ' in character literal")
		} else if n != 1 {
			s.errh(s.line, s.col, "invalid character literal (more than one character)")
		}
	}

	s.nlsemi = true
	s.lit = string(s.stopLit())
	s.kind = RuneLit
	s.tok = _Literal
}

func (s *scanner) stdString() {
	s.startLit()

	for {
		r := s.getr()
		if r == '"' {
			break
		}
		if r == '\\' {
			s.escape('"')
			continue
		}
		if r == '\n' {
			s.ungetr() // assume newline is not part of literal
			s.error("newline in string")
			break
		}
		if r < 0 {
			s.errh(s.line, s.col, "string not terminated")
			break
		}
	}

	s.nlsemi = true
	s.lit = string(s.stopLit())
	s.kind = StringLit
	s.tok = _Literal
}

func (s *scanner) rawString() {
	s.startLit()

	for {
		r := s.getr()
		if r == '`' {
			break
		}
		if r < 0 {
			s.errh(s.line, s.col, "string not terminated")
			break
		}
	}
	// We leave CRs in the string since they are part of the
	// literal (even though they are not part of the literal
	// value).

	s.nlsemi = true
	s.lit = string(s.stopLit())
	s.kind = StringLit
	s.tok = _Literal
}

func (s *scanner) comment(text string) {
	s.errh(s.line, s.col, text)
}

func (s *scanner) skipLine(r rune) {
	for r >= 0 {
		if r == '\n' {
			s.ungetr() // don't consume '\n' - needed for nlsemi logic
			break
		}
		r = s.getr()
	}
}

func (s *scanner) lineComment() {
	r := s.getr()

	if s.mode&comments != 0 {
		s.startLit()
		s.skipLine(r)
		s.comment("//" + string(s.stopLit()))
		return
	}

	// directives must start at the beginning of the line (s.col == colbase)
	if s.mode&directives == 0 || s.col != colbase || (r != 'g' && r != 'l') {
		s.skipLine(r)
		return
	}

	// recognize go: or line directives
	prefix := "go:"
	if r == 'l' {
		prefix = "line "
	}
	for _, m := range prefix {
		if r != m {
			s.skipLine(r)
			return
		}
		r = s.getr()
	}

	// directive text
	s.startLit()
	s.skipLine(r)
	s.comment("//" + prefix + string(s.stopLit()))
}

func (s *scanner) skipComment(r rune) bool {
	for r >= 0 {
		for r == '*' {
			r = s.getr()
			if r == '/' {
				return true
			}
		}
		r = s.getr()
	}
	s.errh(s.line, s.col, "comment not terminated")
	return false
}

func (s *scanner) fullComment() {
	r := s.getr()

	if s.mode&comments != 0 {
		s.startLit()
		if s.skipComment(r) {
			s.comment("/*" + string(s.stopLit()))
		} else {
			s.killLit() // not a complete comment - ignore
		}
		return
	}

	if s.mode&directives == 0 || r != 'l' {
		s.skipComment(r)
		return
	}

	// recognize line directive
	const prefix = "line "
	for _, m := range prefix {
		if r != m {
			s.skipComment(r)
			return
		}
		r = s.getr()
	}

	// directive text
	s.startLit()
	if s.skipComment(r) {
		s.comment("/*" + prefix + string(s.stopLit()))
	} else {
		s.killLit() // not a complete comment - ignore
	}
}

func (s *scanner) escape(quote rune) bool {
	var n int
	var base, max uint32

	c := s.getr()
	switch c {
	case 'a', 'b', 'f', 'n', 'r', 't', 'v', '\\', quote:
		return true
	case '0', '1', '2', '3', '4', '5', '6', '7':
		n, base, max = 3, 8, 255
	case 'x':
		c = s.getr()
		n, base, max = 2, 16, 255
	case 'u':
		c = s.getr()
		n, base, max = 4, 16, unicode.MaxRune
	case 'U':
		c = s.getr()
		n, base, max = 8, 16, unicode.MaxRune
	default:
		if c < 0 {
			return true // complain in caller about EOF
		}
		s.error("unknown escape sequence")
		return false
	}

	var x uint32
	for i := n; i > 0; i-- {
		d := base
		switch {
		case isDigit(c):
			d = uint32(c) - '0'
		case 'a' <= c && c <= 'f':
			d = uint32(c) - ('a' - 10)
		case 'A' <= c && c <= 'F':
			d = uint32(c) - ('A' - 10)
		}
		if d >= base {
			if c < 0 {
				return true // complain in caller about EOF
			}
			kind := "hex"
			if base == 8 {
				kind = "octal"
			}
			s.error(fmt.Sprintf("non-%s character in escape sequence: %c", kind, c))
			s.ungetr()
			return false
		}
		// d < base
		x = x*base + d
		c = s.getr()
	}
	s.ungetr()

	if x > max && base == 8 {
		s.error(fmt.Sprintf("octal escape value > 255: %d", x))
		return false
	}

	if x > max || 0xD800 <= x && x < 0xE000 /* surrogate range */ {
		s.error("escape sequence is invalid Unicode code point")
		return false
	}

	return true
}

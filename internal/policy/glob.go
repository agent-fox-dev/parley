package policy

import (
	"math/bits"
	"unicode/utf8"
)

// This is the matcher of the paths and exclude globs of a policy, and of
// nothing else: the paths of a token are matched by the identity package,
// where ? is an ordinary character, and the targets of a rule by the
// branch-glob matcher of the workspace package.
//
// A caller can ask an evaluation to match thousands of long paths against
// a document of thousands of long globs, so the matcher is built to keep
// that work small: a glob is split into its elements once per evaluation,
// a path is read once per evaluation, and one element of a glob is matched
// against 64 positions of the path at a time, by word operations on the set
// of positions the glob can have reached, which stops as soon as that set
// is empty.

// tokenKind is the kind of one element of a path glob.
type tokenKind uint8

const (
	tokenLiteral tokenKind = iota // one given character
	tokenStar                     // * : any run of characters except /
	tokenAny                      // ** : any run of characters, / included
	tokenOne                      // ? : exactly one character except /
)

// pathToken is one element of a path glob; a literal names its character
// by its number in the alphabet of the globSet that compiled it.
type pathToken struct {
	kind tokenKind
	lit  int32
}

// pathGlob is a path glob split into its elements.
type pathGlob []pathToken

// globSet compiles path globs that are matched against the same paths, and
// numbers the characters their literals name, so that a path is prepared
// once for all of them.
type globSet struct {
	alphabet map[rune]int32
}

func newGlobSet() *globSet {
	return &globSet{alphabet: map[rune]int32{}}
}

// compile splits glob into its elements; ** is read before *, so a longer
// run of stars is ** followed by the rest.
func (gs *globSet) compile(glob string) pathGlob {
	runes := []rune(glob)
	out := make(pathGlob, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '*':
			out = append(out, pathToken{kind: tokenAny})
			i++
		case runes[i] == '*':
			out = append(out, pathToken{kind: tokenStar})
		case runes[i] == '?':
			out = append(out, pathToken{kind: tokenOne})
		default:
			n, ok := gs.alphabet[runes[i]]
			if !ok {
				n = int32(len(gs.alphabet))
				gs.alphabet[runes[i]] = n
			}
			out = append(out, pathToken{kind: tokenLiteral, lit: n})
		}
	}
	return out
}

// compileAll compiles every glob of globs.
func (gs *globSet) compileAll(globs []string) []pathGlob {
	out := make([]pathGlob, 0, len(globs))
	for _, g := range globs {
		out = append(out, gs.compile(g))
	}
	return out
}

// subject returns a path holder for the globs gs has compiled; gs must
// compile no further glob afterwards.
func (gs *globSet) subject() *subject {
	return &subject{alphabet: gs.alphabet, classes: make([]*class, len(gs.alphabet))}
}

// subject is one path at a time, prepared for the globs of one globSet.
// Position j of the path is the point after its first j characters, so a
// path of n characters has positions 0 to n, one bit each.
type subject struct {
	alphabet map[rune]int32
	n        int
	// open has bit j set when j > 0 and character j-1 is not /: the
	// positions a * or a ? can step into.
	open []uint64
	// classes holds, by alphabet number, where each character a literal
	// names occurs in the path; nil for a character that does not.
	classes []*class
	touched []int32
	// at and next are the positions reached before and after an element.
	at, next []uint64
}

// class is where one character occurs in the path: pos holds every
// position j whose character j-1 it is, ascending, and bits holds the same
// positions as a set when they are more than the set has words, so that a
// step costs at most the number of words either way.
type class struct {
	pos  []int32
	bits []uint64
}

// reset prepares s for path.
func (s *subject) reset(path string) {
	for _, n := range s.touched {
		s.classes[n] = nil
	}
	s.touched = s.touched[:0]
	s.n = utf8.RuneCountInString(path)
	words := s.n/64 + 1
	s.open = resize(s.open, words)
	s.at, s.next = resize(s.at, words), resize(s.next, words)
	j := 0
	for _, r := range path {
		j++
		if r != '/' {
			s.open[j/64] |= 1 << (j % 64)
		}
		n, ok := s.alphabet[r]
		if !ok {
			continue
		}
		c := s.classes[n]
		if c == nil {
			c = &class{}
			s.classes[n] = c
			s.touched = append(s.touched, n)
		}
		c.pos = append(c.pos, int32(j))
	}
	for _, n := range s.touched {
		if c := s.classes[n]; len(c.pos) > words {
			c.bits = make([]uint64, words)
			for _, p := range c.pos {
				c.bits[p/64] |= 1 << (p % 64)
			}
		}
	}
}

// resize returns b with n words, all zero.
func resize(b []uint64, n int) []uint64 {
	if cap(b) < n {
		return make([]uint64, n)
	}
	b = b[:n]
	clear(b)
	return b
}

// matchAny reports whether at least one glob of globs matches the path.
func (s *subject) matchAny(globs []pathGlob) bool {
	for _, g := range globs {
		if s.match(g) {
			return true
		}
	}
	return false
}

// match reports whether g matches the whole of the path s holds. It keeps
// the set of positions the elements read so far can end at, and stops as
// soon as that set is empty.
func (s *subject) match(g pathGlob) bool {
	at, next := s.at, s.next
	clear(at)
	at[0] = 1
	for _, tok := range g {
		var live bool
		switch tok.kind {
		case tokenAny:
			live = stepAny(at, next, s.n)
		case tokenStar:
			live = stepStar(at, next, s.open)
		case tokenOne:
			live = stepOne(at, next, s.open)
		default:
			live = stepLiteral(at, next, s.classes[tok.lit])
		}
		if !live {
			return false
		}
		at, next = next, at
	}
	return at[s.n/64]>>(s.n%64)&1 == 1
}

// stepAny sets next to every position from the first of at to the last of
// the path, n, which is what ** reaches; it reports whether next is
// non-empty.
func stepAny(at, next []uint64, n int) bool {
	first := -1
	for i, w := range at {
		if w != 0 {
			first = i*64 + bits.TrailingZeros64(w)
			break
		}
	}
	clear(next)
	if first < 0 {
		return false
	}
	for i := range next {
		next[i] = ^uint64(0)
	}
	next[first/64] &^= 1<<(first%64) - 1
	clear(next[:first/64])
	if r := (n + 1) % 64; r != 0 {
		next[len(next)-1] &= 1<<r - 1
	}
	return true
}

// stepStar sets next to the positions * reaches from at: each position of
// at, and every later one up to the next / . Adding to the open positions
// that are not in at a carry one past each position of at runs that carry
// through the open positions that follow it, and the bits the addition
// changed are the positions it reached.
func stepStar(at, next, open []uint64) bool {
	var carry, shifted, live uint64
	for i := range at {
		u := open[i] &^ at[i]
		add := at[i]<<1 | shifted
		shifted = at[i] >> 63
		var sum uint64
		sum, carry = bits.Add64(u, add, carry)
		next[i] = at[i] | (sum^u)&open[i]
		live |= next[i]
	}
	return live != 0
}

// stepOne sets next to the positions one character other than / past
// those of at.
func stepOne(at, next, open []uint64) bool {
	var shifted, live uint64
	for i := range at {
		next[i] = (at[i]<<1 | shifted) & open[i]
		shifted = at[i] >> 63
		live |= next[i]
	}
	return live != 0
}

// stepLiteral sets next to the positions one occurrence of c past those of
// at; a nil c is a character the path does not hold.
func stepLiteral(at, next []uint64, c *class) bool {
	if c == nil {
		return false
	}
	var live uint64
	if c.bits != nil {
		var shifted uint64
		for i := range at {
			next[i] = (at[i]<<1 | shifted) & c.bits[i]
			shifted = at[i] >> 63
			live |= next[i]
		}
		return live != 0
	}
	clear(next)
	for _, p := range c.pos {
		if at[(p-1)/64]>>((p-1)%64)&1 == 1 {
			next[p/64] |= 1 << (p % 64)
			live = 1
		}
	}
	return live != 0
}

// MatchPath reports whether glob matches the whole of path, a
// repository-relative path, case-sensitive: * matches any run of characters
// except /, possibly empty, ** any run of characters including /, possibly
// empty, ? exactly one character other than /, and every other character,
// [ and \ included, only itself. It takes time proportional to the length
// of the glob times the length of the path in words of 64 characters,
// whatever the glob, so a glob of many stars cannot make it slow.
func MatchPath(glob, path string) bool {
	gs := newGlobSet()
	g := gs.compile(glob)
	s := gs.subject()
	s.reset(path)
	return s.match(g)
}

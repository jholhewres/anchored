package memory

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file holds the parts of FastTokenizer that reproduce the HuggingFace
// `tokenizers` library exactly for SentencePiece-derived tokenizer.json files
// (the multilingual MiniLM ships one): the Precompiled normalizer, the
// Metaspace pre-tokenizer, the Unigram model's Viterbi search and the
// extraction of added tokens before normalization. TestTokenizerGolden holds
// them to the library's output. The legacy pipeline (legacy=true) keeps the
// behaviour vectors were built with before, see NewLegacyFastTokenizer.

// hfMaxTokenizeBytes bounds the input of the HF pipeline. The model sees 126
// tokens at most; Viterbi is linear in word length, so this cap only guards
// normalization cost on multi-megabyte inputs.
const hfMaxTokenizeBytes = 32 << 10

// hfWordCap bounds one word before subword encoding. For Unigram it is the
// most bytes the model's window can use (every piece is at most maxPieceBytes
// and the window holds maxLen tokens), so the cut never changes what the model
// sees; BPE and WordPiece are superlinear in word length and keep the legacy
// cap.
func (ft *FastTokenizer) hfWordCap() int {
	if ft.modelType == "Unigram" && ft.maxPieceBytes > 0 {
		return ft.maxLen * ft.maxPieceBytes
	}
	return maxWordBytes
}

// unigramUnkPenalty is subtracted from the lowest piece score to price a
// character no piece covers (sentencepiece's kUnkPenalty).
const unigramUnkPenalty = 10.0

// spmCharsMap is SentencePiece's precompiled normalization map as serialized
// in tokenizer.json: a little-endian uint32 byte count, a darts-clone
// double-array trie of that many bytes mapping UTF-8 sequences to offsets,
// and a pool of NUL-terminated replacement strings.
type spmCharsMap struct {
	trie []uint32
	pool string
}

func parseSPMCharsMap(encoded string) (*spmCharsMap, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("precompiled charsmap: %w", err)
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("precompiled charsmap: %d bytes", len(raw))
	}
	size := int(binary.LittleEndian.Uint32(raw[:4]))
	if size%4 != 0 || 4+size > len(raw) {
		return nil, fmt.Errorf("precompiled charsmap: trie size %d of %d bytes", size, len(raw))
	}
	trie := make([]uint32, size/4)
	for i := range trie {
		trie[i] = binary.LittleEndian.Uint32(raw[4+4*i:])
	}
	return &spmCharsMap{trie: trie, pool: string(raw[4+size:])}, nil
}

// Darts-clone unit accessors.
func dartsHasLeaf(u uint32) bool  { return (u>>8)&1 == 1 }
func dartsValue(u uint32) uint32  { return u & (1<<31 - 1) }
func dartsLabel(u uint32) uint32  { return u & (1<<31 | 0xFF) }
func dartsOffset(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }

// transform returns the replacement for the shortest trie key that prefixes
// chunk. Taking the shortest match, even when it leaves the rest of a grapheme
// behind, is what the reference does (spm_precompiled's transform).
func (m *spmCharsMap) transform(chunk string) (string, bool) {
	a := m.trie
	if len(a) == 0 {
		return "", false
	}
	pos := dartsOffset(a[0])
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		if c == 0 {
			break
		}
		pos ^= uint32(c)
		if int(pos) >= len(a) {
			return "", false
		}
		unit := a[pos]
		if dartsLabel(unit) != uint32(c) {
			return "", false
		}
		pos ^= dartsOffset(unit)
		if dartsHasLeaf(unit) {
			if int(pos) >= len(a) {
				return "", false
			}
			start := int(dartsValue(a[pos]))
			if start > len(m.pool) {
				return "", false
			}
			end := strings.IndexByte(m.pool[start:], 0)
			if end < 0 {
				return m.pool[start:], true
			}
			return m.pool[start : start+end], true
		}
	}
	return "", false
}

// normalize applies the map grapheme by grapheme: a grapheme shorter than 6
// bytes is looked up whole, anything else (or a whole-grapheme miss) one
// character at a time, as the HuggingFace Precompiled normalizer does.
func (m *spmCharsMap) normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		g := nextGrapheme(s)
		s = s[len(g):]
		if len(g) < 6 {
			if r, ok := m.transform(g); ok {
				b.WriteString(r)
				continue
			}
		}
		for i := 0; i < len(g); {
			_, w := utf8.DecodeRuneInString(g[i:])
			part := g[i : i+w]
			if r, ok := m.transform(part); ok {
				b.WriteString(r)
			} else {
				b.WriteString(part)
			}
			i += w
		}
	}
	return b.String()
}

// nextGrapheme returns the extended grapheme cluster s starts with, following
// the UAX #29 rules that can produce a cluster shorter than 6 bytes (the only
// ones the normalizer treats as a unit): CR LF, a base followed by extending
// and spacing marks or ZWJ, and controls standing alone. Longer clusters
// (Hangul jamo runs, flags, emoji sequences) go character by character in the
// normalizer either way, so splitting them differently changes nothing.
func nextGrapheme(s string) string {
	r, i := utf8.DecodeRuneInString(s)
	switch {
	case r == '\r':
		if i < len(s) && s[i] == '\n' {
			return s[:i+1]
		}
		return s[:i]
	case isGraphemeControl(r):
		return s[:i]
	}
	for i < len(s) {
		next, w := utf8.DecodeRuneInString(s[i:])
		if !isGraphemeExtender(next) {
			break
		}
		i += w
	}
	return s[:i]
}

func isGraphemeControl(r rune) bool {
	if r == 0x200C || r == 0x200D {
		return false
	}
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
}

func isGraphemeExtender(r rune) bool {
	switch {
	case r == 0x200C || r == 0x200D: // ZWNJ (Extend), ZWJ
		return true
	case r >= 0xFF9E && r <= 0xFF9F: // halfwidth voiced sound marks
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // emoji skin tone modifiers
		return true
	case r >= 0xE0020 && r <= 0xE007F: // tag characters
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc)
}

// metaspaceConfig mirrors the Metaspace pre-tokenizer: spaces become the
// replacement character, every piece is prefixed with it (add_prefix_space /
// prepend_scheme "always") and, with split, a piece is cut before each
// replacement character.
type metaspaceConfig struct {
	replacement string
	prepend     bool
	split       bool
}

func newMetaspaceConfig(cfg *preTokenizerConfig) metaspaceConfig {
	mc := metaspaceConfig{replacement: "▁", prepend: true, split: true}
	if cfg.Replacement != "" {
		mc.replacement = cfg.Replacement
	}
	if cfg.AddPrefixSpace != nil {
		mc.prepend = *cfg.AddPrefixSpace
	}
	switch cfg.PrependScheme {
	case "never":
		mc.prepend = false
	case "always", "first":
		// "first" prefixes only the text's first piece; the shipped tokenizer
		// uses "always", the only scheme the golden set verifies.
		mc.prepend = true
	}
	if cfg.Split != nil {
		mc.split = *cfg.Split
	}
	return mc
}

func (mc metaspaceConfig) apply(s string) []string {
	s = strings.ReplaceAll(s, " ", mc.replacement)
	if mc.prepend && !strings.HasPrefix(s, mc.replacement) {
		s = mc.replacement + s
	}
	if !mc.split {
		return []string{s}
	}
	var out []string
	start := 0
	for i := len(mc.replacement); i < len(s); {
		j := strings.Index(s[i:], mc.replacement)
		if j < 0 {
			break
		}
		cut := i + j
		out = append(out, s[start:cut])
		start = cut
		i = cut + len(mc.replacement)
	}
	return append(out, s[start:])
}

// encodeUnigramViterbi segments word into the pieces with the highest total
// score, the way the Unigram model does: pieces are tried in order of
// increasing length and a candidate replaces the best path only when strictly
// better; a character no piece covers costs the unknown-piece score, and runs
// of unknown characters fuse into one unknown token.
func (ft *FastTokenizer) encodeUnigramViterbi(word string) []int {
	n := len(word)
	if n == 0 {
		return nil
	}
	type node struct {
		score float64
		start int
		id    int
		set   bool
	}
	best := make([]node, n+1)
	best[0].set = true
	for start := 0; start < n; {
		_, charLen := utf8.DecodeRuneInString(word[start:])
		base := best[start].score
		single := false
		limit := start + ft.maxPieceBytes
		if limit > n {
			limit = n
		}
		for end := start + 1; end <= limit; end++ {
			if end < n && !utf8.RuneStart(word[end]) {
				continue
			}
			id, ok := ft.vocab[word[start:end]]
			if !ok || id < 0 || id >= len(ft.scores) {
				continue
			}
			if cand := base + ft.scores[id]; !best[end].set || cand > best[end].score {
				best[end] = node{score: cand, start: start, id: id, set: true}
			}
			if end-start == charLen {
				single = true
			}
		}
		if !single {
			end := start + charLen
			if cand := base + ft.unkScore; !best[end].set || cand > best[end].score {
				best[end] = node{score: cand, start: start, id: ft.unkID, set: true}
			}
		}
		start += charLen
	}
	var rev []int
	inUnk := false
	for end := n; end > 0; {
		nd := best[end]
		if nd.id == ft.unkID {
			if !inUnk {
				rev = append(rev, ft.unkID)
			}
			inUnk = true
		} else {
			rev = append(rev, nd.id)
			inUnk = false
		}
		end = nd.start
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// addedToken is a token matched in the raw text before normalization.
type addedToken struct {
	content string
	id      int
	lstrip  bool
	rstrip  bool
}

// textSegment is either a run of text to normalize and encode (id < 0) or an
// added token already resolved to its id.
type textSegment struct {
	text string
	id   int
}

// splitAddedTokens cuts s around the added tokens it contains, leftmost and
// longest first. lstrip/rstrip tokens absorb the whitespace beside them.
func (ft *FastTokenizer) splitAddedTokens(s string) []textSegment {
	if len(ft.specials) == 0 {
		return []textSegment{{text: s, id: -1}}
	}
	var out []textSegment
	emitted := 0 // s[:emitted] is already in out
	for i := 0; i < len(s); {
		tok, ok := ft.matchAddedToken(s[i:])
		if !ok {
			i++
			continue
		}
		start, end := i, i+len(tok.content)
		if tok.lstrip {
			for start > emitted {
				r, w := utf8.DecodeLastRuneInString(s[emitted:start])
				if !unicode.IsSpace(r) {
					break
				}
				start -= w
			}
		}
		if tok.rstrip {
			for end < len(s) {
				r, w := utf8.DecodeRuneInString(s[end:])
				if !unicode.IsSpace(r) {
					break
				}
				end += w
			}
		}
		if start > emitted {
			out = append(out, textSegment{text: s[emitted:start], id: -1})
		}
		out = append(out, textSegment{id: tok.id})
		emitted, i = end, end
	}
	if emitted < len(s) {
		out = append(out, textSegment{text: s[emitted:], id: -1})
	}
	return out
}

// matchAddedToken returns the longest added token s starts with.
func (ft *FastTokenizer) matchAddedToken(s string) (addedToken, bool) {
	for _, tok := range ft.specials { // longest first
		if strings.HasPrefix(s, tok.content) {
			return tok, true
		}
	}
	return addedToken{}, false
}

func sortAddedTokens(tokens []addedToken) {
	sort.SliceStable(tokens, func(i, j int) bool { return len(tokens[i].content) > len(tokens[j].content) })
}

// tokenizeHF is the HuggingFace pipeline: added tokens are split out of the
// raw text, each remaining segment is normalized, pre-tokenized and encoded,
// and the sequence is truncated to leave room for the special tokens.
func (ft *FastTokenizer) tokenizeHF(text string) []int {
	text = truncateUTF8(text, hfMaxTokenizeBytes)
	limit := ft.maxLen - 2
	var ids []int
segments:
	for _, seg := range ft.splitAddedTokens(text) {
		if seg.id >= 0 {
			ids = append(ids, seg.id)
		} else {
			for _, word := range ft.preTokenizer(ft.normalizer(seg.text)) {
				if word == "" {
					continue
				}
				ids = append(ids, ft.encodeWord(truncateUTF8(word, ft.hfWordCap()))...)
				if len(ids) >= limit {
					break segments
				}
			}
		}
		if len(ids) >= limit {
			break
		}
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

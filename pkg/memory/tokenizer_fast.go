package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// Tokenizer converts text into ONNX-compatible token sequences.
type Tokenizer interface {
	Tokenize(text string) (inputIDs, attentionMask, tokenTypeIDs []int64)
}

type tokenizerConfig struct {
	Version       string               `json:"version"`
	Model         modelConfig          `json:"model"`
	Normalizer    *normalizerConfig    `json:"normalizer"`
	PreTokenizer  *preTokenizerConfig  `json:"pre_tokenizer"`
	PostProcessor *postProcessorConfig `json:"post_processor"`
	Decoder       *decoderConfig       `json:"decoder"`
	AddedTokens   []addedTokenConfig   `json:"added_tokens"`
}

type modelConfig struct {
	Type     string         `json:"type"`
	Vocab    map[string]int `json:"vocab"`
	UnkToken string         `json:"unk_token"`
	Prefix   string         `json:"continuing_subword_prefix,omitempty"`
	MaxChars int            `json:"max_input_chars_per_word,omitempty"`
	Merges   []string       `json:"merges,omitempty"`
}

type normalizerConfig struct {
	Type            string             `json:"type"`
	Normalizers     []normalizerConfig `json:"normalizers,omitempty"`
	CleanText       *bool              `json:"clean_text,omitempty"`
	HandleChineseChars *bool           `json:"handle_chinese_chars,omitempty"`
	StripAccentsN   *bool              `json:"strip_accents,omitempty"`
	LowercaseN      *bool              `json:"lowercase,omitempty"`
}

type preTokenizerConfig struct {
	Type          string              `json:"type"`
	PreTokenizers []preTokenizerConfig `json:"pre_tokenizers,omitempty"`
}

type postProcessorConfig struct {
	Type          string                    `json:"type"`
	Sep           []interface{}             `json:"sep,omitempty"`
	Cls           []interface{}             `json:"cls,omitempty"`
	Single        interface{}               `json:"single,omitempty"`
	Pair          interface{}               `json:"pair,omitempty"`
	SpecialTokens map[string]spTokenConfig  `json:"special_tokens,omitempty"`
}

type spTokenConfig struct {
	ID int `json:"id"`
}

type decoderConfig struct {
	Type   string `json:"type"`
	Prefix string `json:"prefix,omitempty"`
}

type addedTokenConfig struct {
	ID      int    `json:"id"`
	Content string `json:"content"`
	Special bool   `json:"special"`
}

type normalizerFn func(string) string
type preTokenizerFn func(string) []string

// FastTokenizer parses HuggingFace tokenizer.json and produces ONNX-compatible
// input_ids matching Python transformers output.
type FastTokenizer struct {
	vocab           map[string]int
	addedTokens     map[string]int
	modelType       string
	unkToken        string
	unkID           int
	clsID           int
	sepID           int
	padID           int
	maxLen          int
	doLowerCase     bool
	normalizer      normalizerFn
	preTokenizer    preTokenizerFn
	postProcessor   func([]int) []int
	wordPiecePrefix string
	maxInputChars   int
	// BPE merge table: pair → rank
	bpeMerges map[[2]string]int
}

// NewFastTokenizer loads a HuggingFace tokenizer.json and builds a FastTokenizer.
func NewFastTokenizer(tokenizerPath string, maxLen int) (*FastTokenizer, error) {
	data, err := os.ReadFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("read tokenizer.json: %w", err)
	}

	var cfg tokenizerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse tokenizer.json: %w", err)
	}

	if maxLen <= 0 {
		maxLen = 128
	}

	ft := &FastTokenizer{
		vocab:           cfg.Model.Vocab,
		addedTokens:     make(map[string]int),
		modelType:       cfg.Model.Type,
		unkToken:        cfg.Model.UnkToken,
		maxLen:          maxLen,
		wordPiecePrefix: cfg.Model.Prefix,
		maxInputChars:   cfg.Model.MaxChars,
	}

	if ft.wordPiecePrefix == "" && ft.modelType == "WordPiece" {
		ft.wordPiecePrefix = "##"
	}
	if ft.maxInputChars == 0 {
		ft.maxInputChars = 200
	}

	for _, at := range cfg.AddedTokens {
		ft.addedTokens[at.Content] = at.ID
		ft.vocab[at.Content] = at.ID
	}

	ft.unkID = ft.tokenID(ft.unkToken, -1)
	ft.clsID = ft.tokenID("[CLS]", -1)
	ft.sepID = ft.tokenID("[SEP]", -1)
	ft.padID = ft.tokenID("[PAD]", 0)

	ft.normalizer = ft.buildNormalizer(cfg.Normalizer)
	ft.preTokenizer = ft.buildPreTokenizer(cfg.PreTokenizer)
	ft.postProcessor = ft.buildPostProcessor(cfg.PostProcessor)

	if len(cfg.Model.Merges) > 0 {
		ft.bpeMerges = make(map[[2]string]int, len(cfg.Model.Merges))
		for i, m := range cfg.Model.Merges {
			parts := strings.SplitN(m, " ", 2)
			if len(parts) == 2 {
				ft.bpeMerges[[2]string{parts[0], parts[1]}] = i
			}
		}
	}

	return ft, nil
}

func (ft *FastTokenizer) tokenID(token string, defaultID int) int {
	if id, ok := ft.vocab[token]; ok {
		return id
	}
	return defaultID
}

func (ft *FastTokenizer) Tokenize(text string) (inputIDs, attentionMask, tokenTypeIDs []int64) {
	text = strings.TrimSpace(text)
	text = ft.normalizer(text)
	words := ft.preTokenizer(text)

	var tokenIDs []int
	for _, word := range words {
		if word == "" {
			continue
		}
		if id, ok := ft.addedTokens[word]; ok {
			tokenIDs = append(tokenIDs, id)
			continue
		}
		pieces := ft.encodeWord(word)
		tokenIDs = append(tokenIDs, pieces...)
		if len(tokenIDs) >= ft.maxLen-2 {
			tokenIDs = tokenIDs[:ft.maxLen-2]
			break
		}
	}

	if ft.postProcessor != nil {
		tokenIDs = ft.postProcessor(tokenIDs)
	} else {
		tokenIDs = append([]int{ft.clsID}, tokenIDs...)
		tokenIDs = append(tokenIDs, ft.sepID)
	}

	if len(tokenIDs) > ft.maxLen {
		tokenIDs = tokenIDs[:ft.maxLen]
	}

	seqLen := len(tokenIDs)
	inputIDs = make([]int64, ft.maxLen)
	attentionMask = make([]int64, ft.maxLen)
	tokenTypeIDs = make([]int64, ft.maxLen)

	for i := 0; i < seqLen; i++ {
		inputIDs[i] = int64(tokenIDs[i])
		attentionMask[i] = 1
	}
	for i := seqLen; i < ft.maxLen; i++ {
		inputIDs[i] = int64(ft.padID)
	}

	return inputIDs, attentionMask, tokenTypeIDs
}

func (ft *FastTokenizer) encodeWord(word string) []int {
	switch ft.modelType {
	case "WordPiece":
		return ft.encodeWordPiece(word)
	case "BPE":
		return ft.encodeBPE(word)
	case "WordLevel":
		return ft.encodeWordLevel(word)
	default:
		return ft.encodeWordPiece(word)
	}
}

func (ft *FastTokenizer) buildNormalizer(cfg *normalizerConfig) normalizerFn {
	if cfg == nil {
		return identityNormalizer
	}
	switch cfg.Type {
	case "Sequence":
		fns := make([]normalizerFn, len(cfg.Normalizers))
		for i, sub := range cfg.Normalizers {
			fns[i] = ft.buildNormalizer(&sub)
		}
		return chainNormalizers(fns)
	case "NFD":
		return normalizeNFD
	case "NFKD":
		return normalizeNFD
	case "NFC":
		return identityNormalizer
	case "Lowercase":
		ft.doLowerCase = true
		return strings.ToLower
	case "StripAccents":
		return stripAccents
	case "Strip":
		return identityNormalizer
	case "BertNormalizer":
		return ft.buildBertNormalizer(cfg)
	default:
		return identityNormalizer
	}
}

func (ft *FastTokenizer) buildBertNormalizer(cfg *normalizerConfig) normalizerFn {
	var fns []normalizerFn

	lowercase := true
	strip := true
	if cfg.LowercaseN != nil {
		lowercase = *cfg.LowercaseN
	}
	if cfg.StripAccentsN != nil {
		strip = *cfg.StripAccentsN
	}

	if lowercase {
		ft.doLowerCase = true
		fns = append(fns, strings.ToLower)
	}
	if strip {
		fns = append(fns, normalizeNFD, stripAccents)
	}

	if len(fns) == 0 {
		return identityNormalizer
	}
	return chainNormalizers(fns)
}

func identityNormalizer(s string) string { return s }

func chainNormalizers(fns []normalizerFn) normalizerFn {
	return func(s string) string {
		for _, fn := range fns {
			s = fn(s)
		}
		return s
	}
}

func normalizeNFD(s string) string {
	var buf strings.Builder
	buf.Grow(len(s) + len(s)/4)
	for _, r := range s {
		if decomposed := nfdDecompose(r); decomposed != nil {
			for _, dr := range decomposed {
				buf.WriteRune(dr)
			}
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func stripAccents(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	for _, r := range s {
		if !unicode.Is(unicode.Mn, r) {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func nfdDecompose(r rune) []rune {
	if d, ok := nfdLatin1[r]; ok {
		return d
	}
	if d, ok := nfdLatinExtA[r]; ok {
		return d
	}
	return nil
}

// Latin-1 Supplement decompositions (U+00C0–U+00FF).
// Only characters with canonical decomposition mappings.
var nfdLatin1 = map[rune][]rune{
	// A with diacritics
	'\u00C0': {'A', '\u0300'}, // À
	'\u00C1': {'A', '\u0301'}, // Á
	'\u00C2': {'A', '\u0302'}, // Â
	'\u00C3': {'A', '\u0303'}, // Ã
	'\u00C4': {'A', '\u0308'}, // Ä
	'\u00C5': {'A', '\u030A'}, // Å
	// C with cedilla
	'\u00C7': {'C', '\u0327'}, // Ç
	// E with diacritics
	'\u00C8': {'E', '\u0300'}, // È
	'\u00C9': {'E', '\u0301'}, // É
	'\u00CA': {'E', '\u0302'}, // Ê
	'\u00CB': {'E', '\u0308'}, // Ë
	// I with diacritics
	'\u00CC': {'I', '\u0300'}, // Ì
	'\u00CD': {'I', '\u0301'}, // Í
	'\u00CE': {'I', '\u0302'}, // Î
	'\u00CF': {'I', '\u0308'}, // Ï
	// N with tilde
	'\u00D1': {'N', '\u0303'}, // Ñ
	// O with diacritics
	'\u00D2': {'O', '\u0300'}, // Ò
	'\u00D3': {'O', '\u0301'}, // Ó
	'\u00D4': {'O', '\u0302'}, // Ô
	'\u00D5': {'O', '\u0303'}, // Õ
	'\u00D6': {'O', '\u0308'}, // Ö
	// U with diacritics
	'\u00D9': {'U', '\u0300'}, // Ù
	'\u00DA': {'U', '\u0301'}, // Ú
	'\u00DB': {'U', '\u0302'}, // Û
	'\u00DC': {'U', '\u0308'}, // Ü
	// Y with acute
	'\u00DD': {'Y', '\u0301'}, // Ý
	// a with diacritics
	'\u00E0': {'a', '\u0300'}, // à
	'\u00E1': {'a', '\u0301'}, // á
	'\u00E2': {'a', '\u0302'}, // â
	'\u00E3': {'a', '\u0303'}, // ã
	'\u00E4': {'a', '\u0308'}, // ä
	'\u00E5': {'a', '\u030A'}, // å
	// c with cedilla
	'\u00E7': {'c', '\u0327'}, // ç
	// e with diacritics
	'\u00E8': {'e', '\u0300'}, // è
	'\u00E9': {'e', '\u0301'}, // é
	'\u00EA': {'e', '\u0302'}, // ê
	'\u00EB': {'e', '\u0308'}, // ë
	// i with diacritics
	'\u00EC': {'i', '\u0300'}, // ì
	'\u00ED': {'i', '\u0301'}, // í
	'\u00EE': {'i', '\u0302'}, // î
	'\u00EF': {'i', '\u0308'}, // ï
	// n with tilde
	'\u00F1': {'n', '\u0303'}, // ñ
	// o with diacritics
	'\u00F2': {'o', '\u0300'}, // ò
	'\u00F3': {'o', '\u0301'}, // ó
	'\u00F4': {'o', '\u0302'}, // ô
	'\u00F5': {'o', '\u0303'}, // õ
	'\u00F6': {'o', '\u0308'}, // ö
	// u with diacritics
	'\u00F9': {'u', '\u0300'}, // ù
	'\u00FA': {'u', '\u0301'}, // ú
	'\u00FB': {'u', '\u0302'}, // û
	'\u00FC': {'u', '\u0308'}, // ü
	// y with diacritics
	'\u00FD': {'y', '\u0301'}, // ý
	'\u00FF': {'y', '\u0308'}, // ÿ
}

// Common Latin Extended-A decompositions (U+0100–U+017F).
var nfdLatinExtA = map[rune][]rune{
	'\u0100': {'A', '\u0304'}, // Ā
	'\u0101': {'a', '\u0304'}, // ā
	'\u0102': {'A', '\u0306'}, // Ă
	'\u0103': {'a', '\u0306'}, // ă
	'\u0104': {'A', '\u0328'}, // Ą
	'\u0105': {'a', '\u0328'}, // ą
	'\u0106': {'C', '\u0301'}, // Ć
	'\u0107': {'c', '\u0301'}, // ć
	'\u0108': {'C', '\u0302'}, // Ĉ
	'\u0109': {'c', '\u0302'}, // ĉ
	'\u010A': {'C', '\u0307'}, // Ċ
	'\u010B': {'c', '\u0307'}, // ċ
	'\u010C': {'C', '\u030C'}, // Č
	'\u010D': {'c', '\u030C'}, // č
	'\u010E': {'D', '\u030C'}, // Ď
	'\u010F': {'d', '\u030C'}, // ď
	'\u0110': {'D', '\u0327'}, // Đ (not canonical, but practical)
	'\u0112': {'E', '\u0304'}, // Ē
	'\u0113': {'e', '\u0304'}, // ē
	'\u0114': {'E', '\u0306'}, // Ĕ
	'\u0115': {'e', '\u0306'}, // ĕ
	'\u0116': {'E', '\u0307'}, // Ė
	'\u0117': {'e', '\u0307'}, // ė
	'\u0118': {'E', '\u0328'}, // Ę
	'\u0119': {'e', '\u0328'}, // ę
	'\u011A': {'E', '\u030C'}, // Ě
	'\u011B': {'e', '\u030C'}, // ě
	'\u011C': {'G', '\u0302'}, // Ĝ
	'\u011D': {'g', '\u0302'}, // ĝ
	'\u011E': {'G', '\u0306'}, // Ğ
	'\u011F': {'g', '\u0306'}, // ğ
	'\u0120': {'G', '\u0307'}, // Ġ
	'\u0121': {'g', '\u0307'}, // ġ
	'\u0122': {'G', '\u0327'}, // Ģ
	'\u0123': {'g', '\u0327'}, // ģ
	'\u0124': {'H', '\u0302'}, // Ĥ
	'\u0125': {'h', '\u0302'}, // ĥ
	'\u0128': {'I', '\u0303'}, // Ĩ
	'\u0129': {'i', '\u0303'}, // ĩ
	'\u012A': {'I', '\u0304'}, // Ī
	'\u012B': {'i', '\u0304'}, // ī
	'\u012C': {'I', '\u0306'}, // Ĭ
	'\u012D': {'i', '\u0306'}, // ĭ
	'\u012E': {'I', '\u0328'}, // Į
	'\u012F': {'i', '\u0328'}, // į
	'\u0130': {'I', '\u0307'}, // İ
	'\u0134': {'J', '\u0302'}, // Ĵ
	'\u0135': {'j', '\u0302'}, // ĵ
	'\u0136': {'K', '\u0327'}, // Ķ
	'\u0137': {'k', '\u0327'}, // ķ
	'\u0139': {'L', '\u0301'}, // Ĺ
	'\u013A': {'l', '\u0301'}, // ĺ
	'\u013B': {'L', '\u0327'}, // Ļ
	'\u013C': {'l', '\u0327'}, // ļ
	'\u013D': {'L', '\u030C'}, // Ľ
	'\u013E': {'l', '\u030C'}, // ľ
	'\u0141': {'L', '\u0331'}, // Ł (stroke, mapped to macron below)
	'\u0142': {'l', '\u0331'}, // ł
	'\u0143': {'N', '\u0301'}, // Ń
	'\u0144': {'n', '\u0301'}, // ń
	'\u0145': {'N', '\u0327'}, // Ņ
	'\u0146': {'n', '\u0327'}, // ņ
	'\u0147': {'N', '\u030C'}, // Ň
	'\u0148': {'n', '\u030C'}, // ň
	'\u014C': {'O', '\u0304'}, // Ō
	'\u014D': {'o', '\u0304'}, // ō
	'\u014E': {'O', '\u0306'}, // Ŏ
	'\u014F': {'o', '\u0306'}, // ŏ
	'\u0150': {'O', '\u030B'}, // Ő
	'\u0151': {'o', '\u030B'}, // ő
	'\u0152': {'O', '\u0308'}, // Œ (simplified)
	'\u0153': {'o', '\u0308'}, // œ (simplified)
	'\u0154': {'R', '\u0301'}, // Ŕ
	'\u0155': {'r', '\u0301'}, // ŕ
	'\u0156': {'R', '\u0327'}, // Ŗ
	'\u0157': {'r', '\u0327'}, // ŗ
	'\u0158': {'R', '\u030C'}, // Ř
	'\u0159': {'r', '\u030C'}, // ř
	'\u015A': {'S', '\u0301'}, // Ś
	'\u015B': {'s', '\u0301'}, // ś
	'\u015C': {'S', '\u0302'}, // Ŝ
	'\u015D': {'s', '\u0302'}, // ŝ
	'\u015E': {'S', '\u0327'}, // Ş
	'\u015F': {'s', '\u0327'}, // ş
	'\u0160': {'S', '\u030C'}, // Š
	'\u0161': {'s', '\u030C'}, // š
	'\u0162': {'T', '\u0327'}, // Ţ
	'\u0163': {'t', '\u0327'}, // ţ
	'\u0164': {'T', '\u030C'}, // Ť
	'\u0165': {'t', '\u030C'}, // ť
	'\u0168': {'U', '\u0303'}, // Ũ
	'\u0169': {'u', '\u0303'}, // ũ
	'\u016A': {'U', '\u0304'}, // Ū
	'\u016B': {'u', '\u0304'}, // ū
	'\u016C': {'U', '\u0306'}, // Ŭ
	'\u016D': {'u', '\u0306'}, // ŭ
	'\u016E': {'U', '\u030A'}, // Ů
	'\u016F': {'u', '\u030A'}, // ů
	'\u0170': {'U', '\u030B'}, // Ű
	'\u0171': {'u', '\u030B'}, // ű
	'\u0174': {'W', '\u0302'}, // Ŵ
	'\u0175': {'w', '\u0302'}, // ŵ
	'\u0176': {'Y', '\u0302'}, // Ŷ
	'\u0177': {'y', '\u0302'}, // ŷ
	'\u0178': {'Y', '\u0308'}, // Ÿ
	'\u0179': {'Z', '\u0301'}, // Ź
	'\u017A': {'z', '\u0301'}, // ź
	'\u017B': {'Z', '\u0307'}, // Ż
	'\u017C': {'z', '\u0307'}, // ż
	'\u017D': {'Z', '\u030C'}, // Ž
	'\u017E': {'z', '\u030C'}, // ž
}

func (ft *FastTokenizer) buildPreTokenizer(cfg *preTokenizerConfig) preTokenizerFn {
	if cfg == nil {
		return defaultPreTokenizer
	}
	switch cfg.Type {
	case "Sequence":
		fns := make([]preTokenizerFn, len(cfg.PreTokenizers))
		for i, sub := range cfg.PreTokenizers {
			fns[i] = ft.buildPreTokenizer(&sub)
		}
		return chainPreTokenizers(fns)
	case "Whitespace":
		return whitespacePreTokenizer
	case "WhitespaceSplit":
		return whitespaceSplitPreTokenizer
	case "Punctuation":
		return punctuationPreTokenizer
	case "BertPreTokenizer":
		return bertPreTokenizer
	case "Split":
		return whitespacePreTokenizer
	case "ByteLevel":
		return whitespacePreTokenizer
	case "Metaspace":
		return whitespacePreTokenizer
	default:
		return defaultPreTokenizer
	}
}

func defaultPreTokenizer(s string) []string {
	return SplitOnPunctuation(s)
}

func chainPreTokenizers(fns []preTokenizerFn) preTokenizerFn {
	return func(s string) []string {
		result := []string{s}
		for _, fn := range fns {
			var next []string
			for _, part := range result {
				next = append(next, fn(part)...)
			}
			result = next
		}
		return result
	}
}

func whitespacePreTokenizer(s string) []string {
	return strings.Fields(s)
}

func whitespaceSplitPreTokenizer(s string) []string {
	return strings.Fields(s)
}

func punctuationPreTokenizer(s string) []string {
	return SplitOnPunctuation(s)
}

func bertPreTokenizer(s string) []string {
	return SplitOnPunctuation(s)
}

func (ft *FastTokenizer) buildPostProcessor(cfg *postProcessorConfig) func([]int) []int {
	if cfg == nil {
		return nil
	}
	switch cfg.Type {
	case "BertProcessing":
		return ft.bertPostProcessor(cfg)
	case "TemplateProcessing":
		return ft.templatePostProcessor(cfg)
	default:
		return nil
	}
}

func (ft *FastTokenizer) bertPostProcessor(cfg *postProcessorConfig) func([]int) []int {
	clsID := ft.clsID
	sepID := ft.sepID
	if len(cfg.Cls) >= 2 {
		if id, ok := toInt(cfg.Cls[1]); ok {
			clsID = id
		}
	}
	if len(cfg.Sep) >= 2 {
		if id, ok := toInt(cfg.Sep[1]); ok {
			sepID = id
		}
	}
	return func(ids []int) []int {
		out := make([]int, 0, len(ids)+2)
		out = append(out, clsID)
		out = append(out, ids...)
		out = append(out, sepID)
		return out
	}
}

func (ft *FastTokenizer) templatePostProcessor(cfg *postProcessorConfig) func([]int) []int {
	clsID := ft.clsID
	sepID := ft.sepID
	if st, ok := cfg.SpecialTokens["[CLS]"]; ok {
		clsID = st.ID
	}
	if st, ok := cfg.SpecialTokens["[SEP]"]; ok {
		sepID = st.ID
	}
	return func(ids []int) []int {
		out := make([]int, 0, len(ids)+2)
		out = append(out, clsID)
		out = append(out, ids...)
		out = append(out, sepID)
		return out
	}
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func (ft *FastTokenizer) encodeWordPiece(word string) []int {
	if len(word) > ft.maxInputChars {
		return []int{ft.unkID}
	}

	if id, ok := ft.vocab[word]; ok {
		return []int{id}
	}

	var tokens []int
	runes := []rune(word)
	start := 0
	for start < len(runes) {
		end := len(runes)
		found := false
		for end > start {
			sub := string(runes[start:end])
			if start > 0 {
				sub = ft.wordPiecePrefix + sub
			}
			if id, ok := ft.vocab[sub]; ok {
				tokens = append(tokens, id)
				found = true
				start = end
				break
			}
			end--
		}
		if !found {
			tokens = append(tokens, ft.unkID)
			start++
		}
	}
	return tokens
}

func (ft *FastTokenizer) encodeBPE(word string) []int {
	symbols := make([]string, 0, len(word))
	for _, r := range word {
		symbols = append(symbols, string(r))
	}

	if len(symbols) == 0 {
		return nil
	}

	if ft.bpeMerges != nil {
		for {
			bestIdx := -1
			bestRank := len(ft.bpeMerges) + 1
			for i := 0; i < len(symbols)-1; i++ {
				pair := [2]string{symbols[i], symbols[i+1]}
				if rank, ok := ft.bpeMerges[pair]; ok && rank < bestRank {
					bestRank = rank
					bestIdx = i
				}
			}
			if bestIdx == -1 {
				break
			}
			merged := symbols[bestIdx] + symbols[bestIdx+1]
			newSymbols := make([]string, 0, len(symbols)-1)
			newSymbols = append(newSymbols, symbols[:bestIdx]...)
			newSymbols = append(newSymbols, merged)
			newSymbols = append(newSymbols, symbols[bestIdx+2:]...)
			symbols = newSymbols
		}
	}

	var ids []int
	for _, s := range symbols {
		if id, ok := ft.vocab[s]; ok {
			ids = append(ids, id)
		} else {
			ids = append(ids, ft.unkID)
		}
	}
	return ids
}

func (ft *FastTokenizer) encodeWordLevel(word string) []int {
	if id, ok := ft.vocab[word]; ok {
		return []int{id}
	}
	return []int{ft.unkID}
}

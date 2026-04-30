package memory

import (
	"bufio"
	"os"
	"strings"
)

type WordPieceTokenizer struct {
	vocab  map[string]int32
	unkID  int32
	clsID  int32
	sepID  int32
	padID  int32
	maxLen int
}

func NewWordPieceTokenizer(vocabPath string, maxLen int) (*WordPieceTokenizer, error) {
	f, err := os.Open(vocabPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vocab := make(map[string]int32)
	scanner := bufio.NewScanner(f)
	var id int32
	for scanner.Scan() {
		token := scanner.Text()
		vocab[token] = id
		id++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if maxLen <= 0 {
		maxLen = 128
	}

	return &WordPieceTokenizer{
		vocab:  vocab,
		unkID:  vocab["[UNK]"],
		clsID:  vocab["[CLS]"],
		sepID:  vocab["[SEP]"],
		padID:  vocab["[PAD]"],
		maxLen: maxLen,
	}, nil
}

func (t *WordPieceTokenizer) Tokenize(text string) (inputIDs, attentionMask, tokenTypeIDs []int64) {
	text = strings.ToLower(strings.TrimSpace(text))

	words := SplitOnPunctuation(text)

	var tokens []int32
	tokens = append(tokens, t.clsID)
	for _, word := range words {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		pieces := t.wordPieceEncode(word)
		tokens = append(tokens, pieces...)
		if len(tokens) >= t.maxLen-1 {
			tokens = tokens[:t.maxLen-1]
			break
		}
	}
	tokens = append(tokens, t.sepID)

	seqLen := len(tokens)

	inputIDs = make([]int64, t.maxLen)
	attentionMask = make([]int64, t.maxLen)
	tokenTypeIDs = make([]int64, t.maxLen)

	for i := 0; i < seqLen; i++ {
		inputIDs[i] = int64(tokens[i])
		attentionMask[i] = 1
	}
	for i := seqLen; i < t.maxLen; i++ {
		inputIDs[i] = int64(t.padID)
	}

	return inputIDs, attentionMask, tokenTypeIDs
}

func (t *WordPieceTokenizer) wordPieceEncode(word string) []int32 {
	if _, ok := t.vocab[word]; ok {
		return []int32{t.vocab[word]}
	}

	var tokens []int32
	start := 0
	for start < len(word) {
		end := len(word)
		found := false
		for end > start {
			sub := word[start:end]
			if start > 0 {
				sub = "##" + sub
			}
			if id, ok := t.vocab[sub]; ok {
				tokens = append(tokens, id)
				found = true
				start = end
				break
			}
			end--
		}
		if !found {
			tokens = append(tokens, t.unkID)
			start++
		}
	}
	return tokens
}

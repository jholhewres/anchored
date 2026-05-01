package dream

import (
	"strings"
)

// AntonymPair represents a word and its opposite.
type AntonymPair struct {
	Word     string
	Opposite string
}

var antonymPairs = []AntonymPair{
	{"include", "exclude"}, {"enable", "disable"}, {"accept", "reject"},
	{"increase", "decrease"}, {"add", "remove"}, {"start", "stop"},
	{"active", "inactive"}, {"true", "false"}, {"use", "avoid"},
	{"incluir", "excluir"}, {"ativar", "desativar"},
	{"adicionar", "remover"}, {"aceitar", "rejeitar"},
}

var negationWords = []string{
	"not", "don't", "doesn't", "won't", "can't", "isn't", "aren't",
	"wasn't", "never", "no", "none",
	"não", "nunca", "nenhum",
}

// detectNegation returns true when one text contains a negation word
// that the other does not.
func detectNegation(a, b string) bool {
	aLower := strings.ToLower(a)
	bLower := strings.ToLower(b)

	for _, neg := range negationWords {
		if strings.Contains(aLower, neg) != strings.Contains(bLower, neg) {
			return true
		}
	}
	return false
}

// detectAntonyms returns true when the two texts contain opposing words
// from the antonym pairs list.
func detectAntonyms(a, b string) bool {
	aLower := strings.ToLower(a)
	bLower := strings.ToLower(b)

	for _, pair := range antonymPairs {
		aHasWord := strings.Contains(aLower, pair.Word)
		bHasWord := strings.Contains(bLower, pair.Word)
		aHasOpposite := strings.Contains(aLower, pair.Opposite)
		bHasOpposite := strings.Contains(bLower, pair.Opposite)

		if (aHasWord && bHasOpposite) || (aHasOpposite && bHasWord) {
			return true
		}
	}
	return false
}

package memory

import (
	"strings"
	"unicode"
)

func ExtractKeywords(query string) []string {
	words := strings.Fields(strings.ToLower(query))
	var keywords []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'()[]{}*`~@#$%&_-+=<>/\\|")
		if !isValidKeyword(w) {
			continue
		}
		keywords = append(keywords, w)
	}
	return keywords
}

func isValidKeyword(w string) bool {
	if len(w) < 2 {
		return false
	}
	if stopWords[w] {
		return false
	}
	allDigits := true
	for _, r := range w {
		if !unicode.IsDigit(r) {
			allDigits = false
			break
		}
	}
	if allDigits {
		return false
	}
	allPunct := true
	for _, r := range w {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			allPunct = false
			break
		}
	}
	if allPunct {
		return false
	}
	return true
}

func ExpandQueryForFTS(keywords []string) string {
	if len(keywords) == 0 {
		return ""
	}
	var parts []string
	for _, kw := range keywords {
		s := sanitizeFTS5Query(kw)
		if s != "" {
			parts = append(parts, s)
		}
		if len(kw) >= 3 {
			clean := sanitizeFTS5Keyword(kw)
			if clean != "" {
				parts = append(parts, clean+"*")
			}
		}
	}
	seen := make(map[string]bool, len(parts))
	var unique []string
	for _, p := range parts {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	return strings.Join(unique, " OR ")
}

func sanitizeFTS5Query(kw string) string {
	return `"` + sanitizeFTS5Keyword(kw) + `"`
}

func sanitizeFTS5Keyword(kw string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '"', '(', ')', '*', '^', ':', '{', '}':
			return -1
		default:
			return r
		}
	}, kw)
	return strings.TrimSpace(cleaned)
}

var stopWords = map[string]bool{
	"to": true, "of": true, "in": true, "is": true, "it": true,
	"an": true, "as": true, "at": true, "be": true, "by": true,
	"do": true, "go": true, "he": true, "if": true, "me": true,
	"my": true, "no": true, "on": true, "or": true, "so": true,
	"up": true, "we": true, "am": true,
	"de": true, "se": true, "eu": true, "em": true, "ou": true,
	"la": true, "le": true, "un": true, "en": true, "ya": true,
	"du": true, "et": true, "il": true, "je": true, "ce": true,
	"el": true, "lo": true, "mi": true, "si": true, "tu": true,
	"the": true, "and": true, "for": true, "are": true, "but": true,
	"not": true, "you": true, "all": true, "can": true, "had": true,
	"her": true, "was": true, "one": true, "our": true, "out": true,
	"has": true, "its": true, "let": true, "may": true, "who": true,
	"did": true, "get": true, "got": true, "him": true, "his": true,
	"how": true, "man": true, "new": true, "now": true, "old": true,
	"see": true, "way": true, "day": true, "too": true, "use": true,
	"that": true, "with": true, "have": true, "this": true, "will": true,
	"your": true, "from": true, "they": true, "been": true, "said": true,
	"each": true, "which": true, "their": true, "what": true, "about": true,
	"would": true, "there": true, "when": true, "make": true, "like": true,
	"time": true, "just": true, "know": true, "take": true, "come": true,
	"could": true, "than": true, "look": true, "only": true, "into": true,
	"over": true, "such": true, "also": true, "back": true, "some": true,
	"them": true, "then": true, "these": true, "thing": true, "where": true,
	"much": true, "should": true, "well": true, "after": true,
	"very": true, "does": true, "here": true, "were": true,
	"more": true, "most": true, "many": true, "other": true, "those": true,
	"still": true, "even": true, "both": true, "same": true, "every": true,
	"que": true, "não": true, "nao": true, "com": true, "uma": true, "para": true,
	"por": true, "mais": true, "como": true, "mas": true, "dos": true,
	"das": true, "nos": true, "nas": true, "foi": true, "ser": true,
	"tem": true, "são": true, "sao": true, "seu": true, "sua": true, "isso": true,
	"este": true, "esta": true, "esse": true, "essa": true, "aqui": true,
	"ele": true, "ela": true, "eles": true, "elas": true, "nós": true,
	"vocé": true, "voce": true, "você": true, "também": true, "tambem": true,
	"onde": true, "quando": true, "quem": true, "qual": true, "quais": true,
	"tudo": true, "todos": true, "toda": true, "todas": true,
	"muito": true, "muita": true, "muitos": true, "muitas": true,
	"outro": true, "outra": true, "outros": true, "outras": true,
	"sobre": true, "entre": true, "depois": true, "ainda": true,
	"desde": true, "até": true, "ate": true, "seus": true, "suas": true,
	"meu": true, "minha": true, "meus": true, "minhas": true,
	"los": true, "las": true, "del": true, "uno": true,
	"con": true, "más": true, "pero": true,
	"sin": true, "sus": true, "les": true, "fue": true, "son": true,
	"han": true, "hay": true, "está": true,
	"todo": true, "ese": true, "eso": true, "así": true, "asi": true,
	"cada": true, "bien": true, "puede": true, "tiene": true,
	"donde": true, "cuando": true, "quien": true, "cual": true,
	"porque": true, "aunque": true, "después": true, "despues": true,
	"antes": true, "hasta": true, "aquí": true,
	"algo": true, "mismo": true, "misma": true,
	"des": true, "une": true, "dans": true, "pour": true,
	"avec": true, "sur": true, "pas": true, "qui": true, "est": true,
	"par": true, "plus": true, "sont": true, "ont": true,
	"aux": true, "été": true, "ete": true, "ces": true, "ses": true,
	"fait": true, "tout": true, "même": true, "meme": true,
	"être": true, "etre": true, "avoir": true, "comme": true,
	"aussi": true, "après": true, "apres": true, "encore": true,
	"donc": true, "quand": true, "chez": true,
	"leur": true, "leurs": true, "autre": true, "autres": true,
}

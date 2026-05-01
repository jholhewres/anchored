package memory

import (
	"regexp"
	"strings"
	"unicode"
)

var accentMap = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a',
	'Á': 'a', 'À': 'a', 'Â': 'a', 'Ã': 'a', 'Ä': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'É': 'e', 'È': 'e', 'Ê': 'e', 'Ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'Í': 'i', 'Ì': 'i', 'Î': 'i', 'Ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'Ó': 'o', 'Ò': 'o', 'Ô': 'o', 'Õ': 'o', 'Ö': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'Ú': 'u', 'Ù': 'u', 'Û': 'u', 'Ü': 'u',
	'ç': 'c', 'Ç': 'c',
	'ñ': 'n', 'Ñ': 'n',
	'ý': 'y', 'ÿ': 'y', 'Ý': 'y',
}

func NormalizeAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if repl, ok := accentMap[r]; ok {
			b.WriteRune(repl)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var synonyms = map[string][]string{
	"auth":         {"authentication", "authenticate", "autorizacao"},
	"api":          {"endpoint", "rest"},
	"deploy":       {"deployment", "release", "implantacao", "publicar"},
	"test":         {"testing", "spec", "testar", "verificacion"},
	"config":       {"configuration", "setting", "configuracion", "parametre", "ajuste"},
	"database":     {"db", "sqlite", "postgres", "datos", "donnees"},
	"error":        {"exception", "failure", "bug", "falha", "excepcion", "fallo", "defaut"},
	"function":     {"method", "procedure", "handler", "funcao"},
	"performance":  {"speed", "latency", "throughput"},
	"refactor":     {"rewrite", "restructure"},
	"component":    {"widget", "module", "part"},
	"debug":        {"troubleshoot", "diagnose"},
	"server":       {"backend", "service", "servidor", "servicio", "serveur"},
	"client":       {"frontend", "app", "cliente", "aplicacion", "application"},
	"security":     {"auth", "authorization", "seguranca"},
	"cache":        {"memoize", "store"},
	"dependency":   {"import", "library", "package"},
	"framework":    {"library", "toolkit", "sdk"},
	"migration":    {"upgrade", "migracao"},
	"repository":   {"repo", "store", "repositorio", "armazenamento"},
	"autenticar":   {"autenticacao", "login", "auth"},
	"implantar":    {"implantacao", "publicar", "deploy"},
	"teste":        {"testar", "spec", "verificacao"},
	"configurar":   {"configuracao", "ajuste", "config"},
	"banco":        {"database", "db", "sqlite"},
	"erro":         {"exception", "falha", "bug"},
	"funcao":       {"metodo", "procedimento", "handler"},
	"seguranca":    {"auth", "autorizacao"},
	"prueba":       {"test", "verificacion"},
	"serveur":      {"backend", "service"},
	"erreur":       {"exception", "defaut"},
	"base":         {"database", "db", "datos", "donnees"},
}

// nearRe matches FTS5 NEAR operator patterns like "word1 NEAR/5 word2".
var nearRe = regexp.MustCompile(`(?i)(\S+)\s+NEAR/\d+\s+(\S+)`)
var nearDistRe = regexp.MustCompile(`NEAR/\d+`)

// ExpandQueryAdvanced takes a raw user query and produces an FTS5-compatible query string.
// It handles:
//   - Quoted phrases: "exact phrase" → preserved as FTS5 phrase
//   - NEAR operator: word1 NEAR/5 word2 → preserved
//   - Standalone words: prefix expansion (word*) + synonym expansion
//   - Accent normalization before FTS5 generation
func ExpandQueryAdvanced(query string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}

	var parts []string
	remaining := query

	quoteRe := regexp.MustCompile(`"([^"]+)"`)
	phraseMatches := quoteRe.FindAllStringSubmatchIndex(remaining, -1)

	var nonPhraseSegs []string
	lastEnd := 0
	for _, loc := range phraseMatches {
		if loc[0] > lastEnd {
			nonPhraseSegs = append(nonPhraseSegs, remaining[lastEnd:loc[0]])
		}
		phrase := remaining[loc[2]:loc[3]]
		normalized := NormalizeAccents(strings.ToLower(phrase))
		if normalized != "" {
			parts = append(parts, `"`+normalized+`"`)
		}
		lastEnd = loc[1]
	}
	if lastEnd < len(remaining) {
		nonPhraseSegs = append(nonPhraseSegs, remaining[lastEnd:])
	}

	for _, seg := range nonPhraseSegs {
		nearMatches := nearRe.FindAllStringSubmatch(seg, -1)
		nearLastEnd := 0

		for _, nm := range nearMatches {
			fullMatch := nm[0]
			idx := strings.Index(seg[nearLastEnd:], fullMatch)
			if idx < 0 {
				continue
			}
			globalIdx := nearLastEnd + idx

			before := seg[nearLastEnd:globalIdx]
			if strings.TrimSpace(before) != "" {
				parts = append(parts, expandStandaloneWords(before)...)
			}

			word1 := NormalizeAccents(strings.ToLower(nm[1]))
			word2 := NormalizeAccents(strings.ToLower(nm[2]))
			distance := nearDistRe.FindString(fullMatch)
			if distance == "" {
				distance = "NEAR/10"
			}
			parts = append(parts, word1+" "+distance+" "+word2)

			nearLastEnd = globalIdx + len(fullMatch)
		}

		after := seg[nearLastEnd:]
		if strings.TrimSpace(after) != "" {
			parts = append(parts, expandStandaloneWords(after)...)
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

func expandStandaloneWords(text string) []string {
	words := strings.Fields(strings.ToLower(text))
	var parts []string

	for _, w := range words {
		w = strings.Trim(w, ".,;:!?()[]{}*`~@#$%&_-+=<>/\\|")
		w = NormalizeAccents(w)

		if w == "" || !isValidKeyword(w) {
			continue
		}

		clean := sanitizeFTS5Keyword(w)
		if clean != "" {
			parts = append(parts, `"`+clean+`"`)
		}

		if len(w) >= 3 && clean != "" {
			parts = append(parts, clean+"*")
		}

		if syns, ok := synonyms[w]; ok {
			for _, s := range syns {
				sc := sanitizeFTS5Keyword(s)
				if sc != "" {
					parts = append(parts, `"`+sc+`"`)
				}
			}
		}
	}

	return parts
}

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
		case '"', '(', ')', '*', '^', ':', '{', '}', '/', '\\':
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
	"any": true, "few": true, "own": true, "she": true, "two": true,
	"before": true, "because": true, "between": true, "during": true,
	"without": true, "under": true, "while": true, "must": true,
	"need": true, "shall": true, "might": true, "never": true,
	"always": true, "often": true, "however": true, "through": true,
	"being": true, "having": true, "doing": true, "used": true,
	"using": true, "made": true, "found": true, "first": true,
	"last": true, "long": true, "great": true, "little": true,
	"right": true, "big": true, "high": true, "different": true,
	"small": true, "large": true, "next": true, "early": true,
	"young": true, "important": true, "public": true, "good": true,
	"able": true, "work": true, "part": true, "case": true,
	"number": true, "point": true, "group": true, "general": true,
	"upon": true, "per": true,
	"de": true, "se": true, "eu": true, "em": true, "ou": true,
	"la": true, "le": true, "un": true, "en": true, "ya": true,
	"du": true, "et": true, "il": true, "je": true, "ce": true,
	"el": true, "lo": true, "mi": true, "si": true, "tu": true,
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
	"pela": true, "pelo": true, "pelos": true, "pelas": true,
	"num": true, "numa": true, "nesse": true, "nessa": true,
	"neste": true, "nesta": true, "naquele": true, "naquela": true,
	"da": true,
	"tenho": true, "temos": true, "ter": true,
	"fazer": true, "feito": true, "poder": true,
	"dizer": true, "estar": true, "houve": true, "pôr": true,
	"mesmo": true, "local": true, "contra": true, "contudo": true,
	"portanto": true, "pois": true, "enquanto": true, "apenas": true,
	"algum": true, "alguma": true, "nenhum": true, "nenhuma": true,
	"pouco": true, "bem": true, "mal": true,
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
	"estos": true, "estas": true, "aquel": true, "aquella": true,
	"su": true, "yo": true, "nosotros": true,
	"ellos": true, "ellas": true, "esos": true, "esas": true,
	"tener": true, "hacer": true, "decir": true,
	"ir": true, "ver": true, "dar": true, "saber": true,
	"muy": true, "tan": true, "ni": true,
	"sino": true, "solo": true, "siempre": true, "nunca": true,
	"también": true, "además": true, "tampoco": true, "quizá": true,
	"otro": true, "otra": true, "otros": true, "otras": true,
	"des": true, "une": true, "dans": true, "pour": true,
	"avec": true, "sur": true, "pas": true, "qui": true, "est": true,
	"par": true, "plus": true, "sont": true, "ont": true,
	"aux": true, "été": true, "ete": true, "ses": true,
	"fait": true, "même": true, "meme": true,
	"être": true, "etre": true, "avoir": true, "comme": true,
	"après": true, "apres": true, "encore": true,
	"quand": true, "chez": true,
	"leur": true, "leurs": true, "autre": true, "autres": true,
	"mon": true, "ma": true, "mes": true, "ton": true, "ta": true, "tes": true,
	"notre": true, "votre": true, "vos": true,
	"cette": true, "cet": true,
	"elle": true, "vous": true,
	"ne": true,
	"car": true,
	"très": true, "tres": true, "peu": true,
	"tous": true, "toute": true, "toutes": true,
	"rien": true, "jamais": true, "toujours": true, "déjà": true,
	"pendant": true, "depuis": true,
}

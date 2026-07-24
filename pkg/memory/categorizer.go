package memory

import "regexp"

type categoryPattern struct {
	re       *regexp.Regexp
	category string
}

var compiledCategoryPatterns []categoryPattern

// Pattern order matters: more specific categories run before broader ones.
// "fact" is the implicit default and has no pattern.
func init() {
	patterns := []struct {
		pattern  string
		category string
	}{
		// summary
		{`(?i)(daily|weekly|monthly).*(log|summary|report|relatório)`, "summary"},
		{`(?i)\b(resumo|summary|compacted|consolidado|consolidated)\b`, "summary"},
		{`(?i)\b(overview|balanço|relatório|recap|tldr|tl;dr)\b`, "summary"},

		// learning — was previously broken (only 6/23K), now broader
		{`(?i)\b(til|today\s+i\s+learned)\b`, "learning"},
		{`(?i)\b(learned|aprend[ie]|discovered|lesson|takeaway)\b`, "learning"},
		{`(?i)\b(vimos\s+que|descobri(mos)?\s+que|notamos\s+que|percebi(mos)?\s+que)\b`, "learning"},
		{`(?i)\b(now\s+i\s+know|noticed\s+that|found\s+out|got\s+bit\s+by|gotcha)\b`, "learning"},
		{`(?i)\b(lição\s+aprendida|experiência\s+foi|ensinou\s+que|aprendizado)\b`, "learning"},
		{`(?i)\b(post[-\s]?mortem|root\s+cause|causa\s+raiz|análise\s+do\s+incidente)\b`, "learning"},

		// plan — must run before decision so "Next up: refactor" wins over "refactor"
		{`(?i)\b(todo|fixme|próxim[oa]\s+passo|next\s+(up|step)|need\s+to|going\s+to)\b`, "plan"},
		{`(?i)\b(vou\s+\w+\b|tenho\s+que\b|preciso\s+\w+\b)`, "plan"},
		{`(?i)\b(roadmap|backlog|sprint\s+goal|milestone|deliverable)\b`, "plan"},
		{`(?i)\b(planejad[oa]|planned|scheduled\s+for|programad[oa])\b`, "plan"},

		// decision — bare "pattern" / "design" removed (false positives on "design patterns in Python");
		// require explicit decision-verb anchors instead
		{`(?i)\b(decisão|decided|escolh[aei]\s+usar|chose|picked)\b`, "decision"},
		{`(?i)\b(arquitetura|architecture)\s+(decision|choice|direction|escolha)\b`, "decision"},
		{`(?i)\b(chose|picked|decided\s+on|optamos\s+por|fechamos\s+em)\s+(o\s+|a\s+)?\S+\s+(pattern|design)\b`, "decision"},
		{`(?i)\brefactor(ing|ed)?\s+(plan|decision|approach)\b`, "decision"},
		{`(?i)\bdecisão\s+técnica\b`, "decision"},
		{`(?i)\b(vamos\s+(com|usar|adotar|seguir\s+com)|fechamos\s+em|optamos\s+por|consenso\s+foi)\b`, "decision"},
		{`(?i)\b(settled\s+on|agreed\s+on|going\s+with|gonna\s+use|we'?ll\s+(use|go\s+with))\b`, "decision"},
		{`(?i)\b(adr|architecture\s+decision|decisão\s+técnica)\b`, "decision"},
		{`(?i)\b(de\s+agora\s+em\s+diante|going\s+forward|from\s+now\s+on)\b`, "decision"},

		// preference
		{`(?i)\b(prefere|prefer[es]?|gosta\s+de|likes?|sempre\s+usa|always\s+use)\b`, "preference"},
		// nunca/never are anchored to a usage verb (mirroring sempre+usa above):
		// bare "nunca"/"never" matched any negated sentence ("senha nunca
		// exposta") and dumped it into the preference bucket.
		{`(?i)\b(não\s+gosta|dislikes?|evita|avoids?|nunca\s+usa|never\s+use)\b`, "preference"},
		{`(?i)\b(modo|mode|theme|layout)\b.*(escuro|dark|claro|light)`, "preference"},
		{`(?i)\b(favorit[oa]|favorite|preferid[oa]|preferred)\b`, "preference"},
		// "padrão é"/"regra é" require a personal possessive: bare forms matched
		// config/domain facts ("o padrão é 8080", "a regra é X") rather than a
		// personal convention.
		{`(?i)\b(habito\s+de|costum[oa]|minha\s+preferência|meu\s+padrão\s+é|minha\s+regra\s+é)\b`, "preference"},
		{`(?i)\b(i\s+(always|never|tend\s+to)|rule\s+of\s+thumb|by\s+convention)\b`, "preference"},
		{`(?i)\b(estilo\s+de\s+código|coding\s+style|convention[ao]?\b)`, "preference"},

		// event
		{`(?i)\b(reunião|meeting|standup|retro|retrospectiva)\b`, "event"},
		{`(?i)\b(lembrete|reminder|alerta|alert|aviso)\b`, "event"},
		{`(?i)\b\d{1,2}[/:h]\d{2}\b`, "event"},
		{`(?i)\b(hoje|amanhã|ontem|tomorrow|yesterday|today)\b`, "event"},
		{`(?i)\b(segunda|terça|quarta|quinta|sexta|sábado|domingo)\b`, "event"},
		{`(?i)\b(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`, "event"},
		{`(?i)\b(deploy|deployed|rollback|hotfix|incident|outage|downtime|release(d)?)\b`, "event"},
		{`(?i)\b(comprou|pagou|transferiu|depositou|sacou)\b`, "event"},
		{`(?i)\b(saldo|fatura|invoice|bill)\b.*\b(R\$|BRL|\d+[.,]\d{2})\b`, "event"},
		{`(?i)\b(merged?|cherry[-\s]?pick|tag(ged)?\s+v?\d|launched|shipped)\b`, "event"},
	}

	for _, p := range patterns {
		compiledCategoryPatterns = append(compiledCategoryPatterns, categoryPattern{
			re:       regexp.MustCompile(p.pattern),
			category: p.category,
		})
	}
}

func Categorize(content string) string {
	for _, cp := range compiledCategoryPatterns {
		if cp.re.MatchString(content) {
			return cp.category
		}
	}
	return "fact"
}

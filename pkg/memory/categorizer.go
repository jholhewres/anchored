package memory

import "regexp"

type categoryPattern struct {
	re       *regexp.Regexp
	category string
}

var compiledCategoryPatterns []categoryPattern

func init() {
	patterns := []struct {
		pattern  string
		category string
	}{
		{`(?i)(daily|weekly|monthly).*(log|summary|report|relatório)`, "summary"},
		{`(?i)\b(resumo|summary|compacted|consolidado|consolidated)\b`, "summary"},
		{`(?i)\b(overview|balanço|relatório|recap)\b`, "summary"},
		{`(?i)\b(reunião|meeting|standup)\b`, "event"},
		{`(?i)\b(lembrete|reminder|alerta|alert|aviso)\b`, "event"},
		{`(?i)\b\d{1,2}[/:h]\d{2}\b`, "event"},
		{`(?i)\b(hoje|amanhã|ontem|tomorrow|yesterday|today)\b`, "event"},
		{`(?i)\b(segunda|terça|quarta|quinta|sexta|sábado|domingo)\b`, "event"},
		{`(?i)\b(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`, "event"},
		{`(?i)\b(deploy|rollback|hotfix|incident|outage)\b`, "event"},
		{`(?i)\b(comprou|pagou|transferiu|depositou|sacou)\b`, "event"},
		{`(?i)\b(saldo|fatura|invoice|bill)\b.*\b(R\$|BRL|\d+[.,]\d{2})\b`, "event"},
		{`(?i)\b(prefere|prefer[es]?|gosta\s+de|likes?|sempre\s+usa|always\s+use)\b`, "preference"},
		{`(?i)\b(não\s+gosta|dislikes?|evita|avoids?|nunca|never)\b`, "preference"},
		{`(?i)\b(modo|mode|theme|layout)\b.*(escuro|dark|claro|light)`, "preference"},
		{`(?i)\b(favorit[oa]|favorite|preferid[oa]|preferred)\b`, "preference"},
		{`(?i)\b(decisão|decided|escolh[aei]\s+usar|chose|picked)\b`, "decision"},
		{`(?i)\b(arquitetura|architecture|refator|refactor|pattern|design)\b`, "decision"},
		{`(?i)\b(learned|aprend[ie]|discovered|lesson)\b`, "learning"},
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

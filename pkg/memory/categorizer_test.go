package memory

import "testing"

func TestCategorize(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		expected string
	}{
		// learning — was previously broken, must now hit
		{"learning_til_en", "TIL that postgres uses MVCC for isolation", "learning"},
		{"learning_pt_vimos", "Vimos que o cache invalida quando o prefixo muda", "learning"},
		{"learning_pt_descobrimos", "Descobrimos que o lambda timeout era 3s, não 30s", "learning"},
		{"learning_pt_licao", "Lição aprendida: nunca confiar em mocks de DB em integração", "learning"},
		{"learning_pt_post_mortem", "Post-mortem do incidente: causa raiz foi a chave duplicada", "learning"},
		{"learning_en_got_bit", "got bit by JS hoisting again", "learning"},
		{"learning_en_takeaway", "Key takeaway: always set request timeouts", "learning"},

		// decision
		{"decision_pt_vamos_com", "Vamos com Postgres em vez de Mongo pra esse serviço", "decision"},
		{"decision_pt_fechamos", "Fechamos em UUID v7 pros IDs", "decision"},
		{"decision_pt_optamos", "Optamos por server-side rendering", "decision"},
		{"decision_en_settled", "Settled on Tailwind for the design system", "decision"},
		{"decision_en_going_forward", "Going forward, all services run on ARM", "decision"},
		{"decision_pt_de_agora", "De agora em diante, commits sem co-author trailer", "decision"},

		// preference
		{"pref_pt_costumo", "Costumo escrever testes antes da implementação", "preference"},
		{"pref_pt_minha_pref", "Minha preferência é tabs em vez de espaços", "preference"},
		{"pref_en_i_always", "I always pin dependency versions", "preference"},
		{"pref_en_i_never", "I never use git push --force on main", "preference"},
		{"pref_en_rule_of_thumb", "Rule of thumb: keep PRs under 400 lines", "preference"},

		// preference false-positives fixed: bare nunca/never and bare "padrão é"/
		// "regra é" must not hijack ordinary sentences into the preference bucket.
		{"not_pref_senha_nunca", "A senha nunca é exposta nos logs", "fact"},
		{"not_pref_padrao_porta", "O padrão é a porta 8080", "fact"},
		{"not_pref_regra_negocio", "A regra é validar entradas antes de processar", "fact"},

		// plan
		{"plan_todo", "TODO: migrar pro novo SDK", "plan"},
		{"plan_pt_vou_fazer", "Vou implementar o webhook na próxima sprint", "plan"},
		{"plan_pt_preciso", "Preciso adicionar testes de integração", "plan"},
		{"plan_en_next_up", "Next up: refactor the auth middleware", "plan"},
		{"plan_roadmap", "Roadmap Q3: migration to GraphQL", "plan"},

		// event
		{"event_deploy", "deployed v2.4.0 to staging", "event"},
		{"event_merged", "merged PR #421", "event"},
		{"event_meeting", "Reunião com o time de plataforma 14h", "event"},
		{"event_release", "shipped the new dashboard today", "event"},

		// summary
		{"summary_recap", "Daily recap: 3 PRs reviewed, 1 incident closed", "summary"},
		{"summary_tldr", "TL;DR: rollback foi limpo, sem perda de dados", "summary"},

		// fact (default)
		{"fact_default", "The user is a backend engineer at HostGator", "fact"},
		{"fact_stack", "Service runs Go 1.22 on Linux ARM", "fact"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Categorize(tc.content)
			if got != tc.expected {
				t.Errorf("Categorize(%q) = %q, want %q", tc.content, got, tc.expected)
			}
		})
	}
}

func TestCategorizeEmpty(t *testing.T) {
	if got := Categorize(""); got != "fact" {
		t.Errorf("empty content: got %q, want fact", got)
	}
}

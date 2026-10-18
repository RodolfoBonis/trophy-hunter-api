package ai

import (
	"fmt"
	"strings"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// systemPrompt vale para todas as execuções (--append-system-prompt).
const systemPrompt = `Você é o assistente de caça a troféus do Trophy Hunter (app pessoal de um único jogador brasileiro).

Pesquisa: use WebSearch/WebFetch para confirmar detalhes em guias conhecidos (PowerPyx, PSNProfiles, TrueAchievements, Steam Community). Não invente localizações, números ou requisitos. Os nomes de troféus que recebo estão em português (como aparecem no console); os guias costumam usar o nome em inglês — descubra a correspondência pela descrição/raridade.

Escrita (obrigatório):
- Português do Brasil correto e natural, com concordância certa (ex.: "a seleção de capítulos"). Nada de frases em inglês.
- Nomes de capítulos, fases, lugares, itens e personagens: use o nome oficial em português do jogo, se existir; se não souber, traduza de forma natural e coloque o original em inglês entre parênteses SÓ na primeira menção. Comandos de controle como no PlayStation/Steam (R2, X, LT…).
- Nomes de troféus: exatamente como fornecidos.
- Seja direto: frases curtas, voz ativa, imperativo nos passos ("Vá até…", "Equipe…"). Um passo = uma ação.
- Escreva para o jogador, não sobre a pesquisa: nunca "as fontes divergem", "não consegui abrir", "segundo o guia X" nos passos/resumo/dicas. Se houver dúvida real, escolha a versão mais provável nos passos e diga em UMA frase curta no campo de incertezas o que o jogador deve conferir no jogo.
- Antes de responder, revise o texto: português, clareza, nada repetido entre resumo, passos e dicas.

Spoilers: não revele história, chefes finais ou o conteúdo de troféus ocultos além do estritamente necessário para obtê-los; quando precisar, avise antes.
Quando não tiver certeza (jogo de nicho, versão diferente, informação conflitante), baixe a confiança.`

func platformName(p string) string {
	if p == "psn" {
		return "PlayStation (troféus; objetivo: platina)"
	}
	return "Steam (conquistas; objetivo: 100%)"
}

func typeName(t string) string {
	switch t {
	case "platinum":
		return "platina"
	case "gold":
		return "ouro"
	case "silver":
		return "prata"
	case "bronze":
		return "bronze"
	default:
		return "conquista"
	}
}

func rarity(p *float64) string {
	if p == nil {
		return "raridade desconhecida"
	}
	return fmt.Sprintf("%.1f%% dos jogadores", *p)
}

// trophyGuidePrompt monta o pedido do guia de um troféu.
func trophyGuidePrompt(t dbgen.GetTrophyWithGameRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Escreva o guia para obter este troféu/conquista.\n\nJogo: %s\nPlataforma: %s\n", t.GameTitle, platformName(t.GamePlatform))
	fmt.Fprintf(&b, "Troféu: %s (%s, %s)\n", t.Name, typeName(t.Type), rarity(t.RarityPct))
	switch {
	case t.Description != nil && *t.Description != "":
		fmt.Fprintf(&b, "Descrição oficial: %s\n", *t.Description)
	case t.Hidden:
		b.WriteString("Descrição oficial: (troféu oculto — a plataforma não mostra a descrição)\n")
	}
	if !t.IsBase {
		fmt.Fprintf(&b, "Grupo: DLC \"%s\"\n", t.GroupName)
	}
	// O guia de troféu é COMPARTILHADO entre os usuários: sem progresso, marcações ou notas de
	// ninguém no prompt (o roadmap e o chat, que são de cada pessoa, é que levam o progresso).
	b.WriteString("\nPreencha o schema seguindo as regras de escrita: em `where`, onde o troféu sai (capítulo/fase/área); " +
		"`summary` em 1–2 frases dizendo o que fazer; `steps` curtos e em ordem (até 8); perdível + ponto de não retorno " +
		"se for o caso; dificuldade 1–10; tempo estimado; dicas práticas (até 4); confiança 0–1; incerteza em uma frase; " +
		"URLs em sources.")
	return b.String()
}

// progressContext resume o progresso real do jogador no jogo (roadmap e chat).
func progressContext(g dbgen.Game, trophies []dbgen.GameTrophiesForAIRow) string {
	var b strings.Builder
	earned := 0
	for _, t := range trophies {
		if t.Earned {
			earned++
		}
	}
	fmt.Fprintf(&b, "Jogo: %s\nPlataforma: %s\nProgresso: %d de %d obtidos.\n", g.Title, platformName(g.Platform), earned, len(trophies))
	if g.Abandoned {
		b.WriteString("O jogador marcou o jogo como abandonado.\n")
	}
	if len(g.Tags) > 0 {
		fmt.Fprintf(&b, "Marcações do jogo: %s\n", strings.Join(g.Tags, ", "))
	}
	if g.Notes != "" {
		fmt.Fprintf(&b, "Notas do jogador sobre o jogo: %s\n", g.Notes)
	}
	b.WriteString("\nFaltam:\n")
	missing := 0
	for _, t := range trophies {
		if t.Earned {
			continue
		}
		missing++
		fmt.Fprintf(&b, "- %s (%s, %s", t.Name, typeName(t.Type), rarity(t.RarityPct))
		if !t.IsBase {
			fmt.Fprintf(&b, ", DLC %s", t.GroupName)
		}
		if t.Hidden {
			b.WriteString(", oculto")
		}
		if t.Progress != nil && t.Target != nil {
			fmt.Fprintf(&b, ", progresso %d/%d", *t.Progress, *t.Target)
		}
		if len(t.Tags) > 0 {
			fmt.Fprintf(&b, ", marcado: %s", strings.Join(t.Tags, "/"))
		}
		if t.ServerShutdownRisk {
			b.WriteString(", servidor pode fechar")
		}
		b.WriteString(")")
		if t.Description != nil && *t.Description != "" && !t.Hidden {
			fmt.Fprintf(&b, ": %s", *t.Description)
		}
		b.WriteString("\n")
	}
	if missing == 0 {
		b.WriteString("(nada — jogo completo)\n")
	}
	return b.String()
}

// roadmapPrompt monta o pedido do roadmap de platina/100%.
func roadmapPrompt(g dbgen.Game, trophies []dbgen.GameTrophiesForAIRow) string {
	return "Monte o roadmap para eu completar este jogo a partir do meu progresso atual " +
		"(platina na PSN / 100% na Steam). Considere só o que FALTA. Ordene as etapas (jogadas, " +
		"limpeza pós-jogo, DLCs), destaque os perdíveis a garantir antes de cada ponto de não retorno, " +
		"e estime o tempo total restante. Cada troféu que falta deve aparecer em alguma etapa, pelo nome " +
		"exato da lista. Descrições de etapa objetivas (2–4 frases), seguindo as regras de escrita.\n\n" +
		progressContext(g, trophies)
}

// chatTurn é uma mensagem anterior da conversa (para reconstruir o contexto sem --resume).
type chatTurn struct {
	Role    string
	Content string
}

// maxTranscript limita quantas mensagens anteriores entram ao recomeçar sem sessão.
const maxTranscript = 20

// chatPrompt monta o turno do chat. Com sessão (--resume), só a pergunta + um lembrete do
// progresso; sem sessão, o contexto completo + o histórico recente.
func chatPrompt(g dbgen.Game, trophies []dbgen.GameTrophiesForAIRow, history []chatTurn, question string, resumed bool) string {
	var b strings.Builder
	if resumed {
		earned := 0
		for _, t := range trophies {
			if t.Earned {
				earned++
			}
		}
		fmt.Fprintf(&b, "(Progresso atualizado: %d de %d obtidos.)\n\n%s", earned, len(trophies), question)
		return b.String()
	}
	b.WriteString("Conversa sobre o jogo abaixo, com o meu progresso real. Responda de forma curta e prática " +
		"(markdown simples), usando a lista do que falta.\n\n")
	b.WriteString(progressContext(g, trophies))
	if len(history) > maxTranscript {
		history = history[len(history)-maxTranscript:]
	}
	if len(history) > 0 {
		b.WriteString("\nConversa até agora:\n")
		for _, m := range history {
			who := "Eu"
			if m.Role == "assistant" {
				who = "Você"
			}
			fmt.Fprintf(&b, "%s: %s\n", who, m.Content)
		}
	}
	fmt.Fprintf(&b, "\nMinha pergunta: %s", question)
	return b.String()
}

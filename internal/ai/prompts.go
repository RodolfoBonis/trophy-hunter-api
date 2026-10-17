package ai

import (
	"fmt"
	"strings"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// systemPrompt vale para todas as execuções (--append-system-prompt).
const systemPrompt = `Você é o assistente de caça a troféus do Trophy Hunter (app pessoal de um único jogador).
Responda SEMPRE em português do Brasil, direto ao ponto, com nomes de troféus/conquistas exatamente como fornecidos.
Use WebSearch/WebFetch para confirmar detalhes em guias conhecidos (PSNProfiles, PowerPyx, TrueAchievements, Steam Community) — não invente localizações, números ou requisitos.
Spoilers: não revele história, chefes finais ou o conteúdo de troféus ocultos além do estritamente necessário para obtê-los; quando precisar citar algo assim, avise antes.
Quando não tiver certeza (jogo de nicho, versão/plataforma diferente, informação conflitante), diga isso claramente e baixe a confiança.`

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
	if t.Progress != nil && t.Target != nil {
		fmt.Fprintf(&b, "Progresso atual do jogador: %d/%d\n", *t.Progress, *t.Target)
	}
	if t.Earned {
		b.WriteString("O jogador JÁ tem este troféu (guia para referência).\n")
	}
	if len(t.Tags) > 0 {
		fmt.Fprintf(&b, "Marcações do jogador: %s\n", strings.Join(t.Tags, ", "))
	}
	if t.Notes != "" {
		fmt.Fprintf(&b, "Notas do jogador: %s\n", t.Notes)
	}
	b.WriteString("\nPreencha o schema: passos concretos e em ordem; se for perdível, diga o ponto de não retorno; dificuldade 1–10; tempo estimado; confiança 0–1 com as incertezas; URLs consultadas em sources.")
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
		"e estime o tempo total restante. Nas etapas, cite os troféus pelos nomes exatos da lista.\n\n" +
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

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ranking"
)

// KindRecap é o job do resumo semanal da IA (ai-worker). Não conta no teto diário (é 1 por semana).
const KindRecap = "ai_recap"

type recapPayload struct {
	UserID string `json:"user_id"`
	Week   string `json:"week"` // AAAA-MM-DD (segunda-feira)
}

// WeeklyRecap é o conteúdo do resumo semanal.
type WeeklyRecap struct {
	Headline      string            `json:"headline"`
	Highlights    []string          `json:"highlights"`
	Suggestions   []RecapSuggestion `json:"suggestions"`
	Encouragement string            `json:"encouragement"`
}

// RecapSuggestion é um jogo para caçar na próxima semana.
type RecapSuggestion struct {
	Game     string   `json:"game"`
	Why      string   `json:"why"`
	Trophies []string `json:"trophies"`
}

// RecapSchema é o JSON Schema do resumo semanal.
var RecapSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["headline","highlights","suggestions","encouragement"],
  "properties":{
    "headline":{"type":"string","description":"Uma frase que resume a semana."},
    "highlights":{"type":"array","maxItems":4,"items":{"type":"string"}},
    "suggestions":{"type":"array","maxItems":3,"items":{"type":"object","additionalProperties":false,
      "required":["game","why","trophies"],
      "properties":{"game":{"type":"string"},"why":{"type":"string"},"trophies":{"type":"array","maxItems":4,"items":{"type":"string"}}}}},
    "encouragement":{"type":"string"}
  }}`)

// ValidateRecap valida e normaliza o resumo.
func ValidateRecap(raw []byte) ([]byte, error) {
	var r WeeklyRecap
	if err := strictDecode(raw, &r, RecapSchema); err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Headline) == "" {
		return nil, fmt.Errorf("%w: headline vazio", ErrInvalidGuide)
	}
	r.Highlights = clean(r.Highlights)
	for i := range r.Suggestions {
		r.Suggestions[i].Trophies = clean(r.Suggestions[i].Trophies)
	}
	return json.Marshal(r)
}

// EnqueueWeeklyRecap cria (ou reabre, se deu erro) o resumo da semana e enfileira a geração.
// Devolve false quando já existia pronto ou pendente (nada a fazer).
func EnqueueWeeklyRecap(ctx context.Context, q *dbgen.Queries, jobs queue.Queue, userID pgtype.UUID, week time.Time) (bool, error) {
	day := pgtype.Date{Time: time.Date(week.Year(), week.Month(), week.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
	if _, err := q.UpsertRecapPending(ctx, dbgen.UpsertRecapPendingParams{UserID: userID, Week: day}); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	p := recapPayload{UserID: uuidString(userID), Week: week.Format("2006-01-02")}
	_, _, err := jobs.Enqueue(ctx, queue.EnqueueParams{Kind: KindRecap, Payload: p, DedupeKey: KindRecap + ":" + p.UserID + ":" + p.Week, MaxAttempts: 4})
	return err == nil, err
}

const recapSystemPrompt = `Você escreve o resumo semanal do Trophy Hunter para um caçador de troféus brasileiro.
Português do Brasil natural, frases curtas, tom de amigo animado mas sem exagero, sem emojis.
Use SÓ os dados recebidos (não pesquise, não invente números, jogos ou troféus). Nomes de jogos e troféus exatamente como vieram.
Não revele conteúdo de troféus marcados como ocultos.`

func (s *Service) handleRecap(ctx context.Context, p recapPayload) error {
	uid, err := parseUUID(p.UserID)
	if err != nil {
		return queue.Permanent(err)
	}
	week, err := time.Parse("2006-01-02", p.Week)
	if err != nil {
		return queue.Permanent(err)
	}
	day := pgtype.Date{Time: week, Valid: true}
	until := time.Now()
	since := until.Add(-7 * 24 * time.Hour)
	prompt, err := s.recapPrompt(ctx, uid, since, until)
	if err != nil {
		return err
	}
	res, err := s.runner.Run(ctx, claude.Request{SystemPrompt: recapSystemPrompt, Model: s.model, Prompt: prompt, JSONSchema: RecapSchema, Workdir: s.dir("recaps")}, nil)
	if err != nil {
		return err
	}
	if res.RateLimited {
		metrics.AIJob("recap", "rate_limited")
		return errors.New("ai: limite de uso")
	}
	content, verr := ValidateRecap(res.Structured)
	if res.Err != "" || verr != nil {
		msg := res.Err
		if msg == "" {
			msg = verr.Error()
		}
		metrics.AIJob("recap", "error")
		_ = s.q.FailRecap(ctx, dbgen.FailRecapParams{UserID: uid, Week: day, Error: ptr(userMessage(msg))})
		return queue.Permanent(errors.New(msg))
	}
	if err := s.q.CompleteRecap(ctx, dbgen.CompleteRecapParams{UserID: uid, Week: day, Content: content, Model: ptr(modelName(s.model))}); err != nil {
		return err
	}
	metrics.AIJob("recap", "ok")
	var r WeeklyRecap
	_ = json.Unmarshal(content, &r)
	msg := push.Payload{UserID: p.UserID, Kind: push.KindWeeklyRecap, Title: "Seu resumo da semana", Body: r.Headline, URL: "/recap", Tag: "recap-" + p.Week}
	if err := push.Enqueue(ctx, s.queue, msg, ""); err != nil {
		s.logger.Warn("ai: enfileirar aviso do resumo falhou", slog.String("err", err.Error()))
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func platformLabel(p string) string {
	if p == "psn" {
		return "PlayStation"
	}
	return "Steam"
}

// recapPrompt junta os números da semana e a fila Próxima Platina (com horas do HLTB e perdíveis).
func (s *Service) recapPrompt(ctx context.Context, uid pgtype.UUID, since, until time.Time) (string, error) {
	sinceTS, untilTS := pgtype.Timestamptz{Time: since, Valid: true}, pgtype.Timestamptz{Time: until, Valid: true}
	tot, err := s.q.RecapTotals(ctx, dbgen.RecapTotalsParams{UserID: uid, Since: sinceTS, Until: untilTS})
	if err != nil {
		return "", err
	}
	top, err := s.q.RecapTopGames(ctx, dbgen.RecapTopGamesParams{UserID: uid, Since: sinceTS, Until: untilTS, Lim: 5})
	if err != nil {
		return "", err
	}
	rare, err := s.q.RecapRarest(ctx, dbgen.RecapRarestParams{UserID: uid, Since: sinceTS, Until: untilTS, Lim: 3})
	if err != nil {
		return "", err
	}
	comps, err := s.q.RecapCompletions(ctx, dbgen.RecapCompletionsParams{UserID: uid, Since: sinceTS, Until: untilTS})
	if err != nil {
		return "", err
	}
	cands, err := s.q.NextPlatinumCandidates(ctx, uid)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SEMANA (%s a %s)\n", since.Format("02/01"), until.Format("02/01"))
	fmt.Fprintf(&b, "- %d troféus/conquistas (platinas %d, ouro %d, prata %d, bronze %d, conquistas Steam %d); %d raros (< 5%%); %d jogos.\n",
		tot.Earned, tot.Platinum, tot.Gold, tot.Silver, tot.Bronze, tot.Achievements, tot.Rare, tot.Games)
	for _, g := range top {
		fmt.Fprintf(&b, "- Jogou: %s [%s] — %d desbloqueios, agora %d/%d.\n", g.Title, platformLabel(g.Platform), g.Unlocked, g.Earned, g.Total)
	}
	for _, c := range comps {
		fmt.Fprintf(&b, "- FECHOU: %s [%s].\n", c.Title, platformLabel(c.Platform))
	}
	for _, r := range rare {
		name := r.Name
		if r.Hidden {
			name = "(troféu oculto)"
		}
		label := "Mais raro da semana"
		if r.RarityPct < 5 {
			label = "Raro (< 5%)"
		}
		fmt.Fprintf(&b, "- %s: %s em %s (%.1f%% dos jogadores).\n", label, name, r.GameTitle, r.RarityPct)
	}
	// Fila para as sugestões (mesmo ranking da tela Próxima Platina).
	rc := make([]ranking.Candidate, 0, len(cands))
	byID := map[string]dbgen.NextPlatinumCandidatesRow{}
	for _, c := range cands {
		id := uuidString(c.ID)
		byID[id] = c
		left := 0.0
		if c.CompletionistH > 0 {
			left = c.CompletionistH * (1 - c.CompletionPct)
		}
		rc = append(rc, ranking.Candidate{ID: id, Completion: c.CompletionPct, Remaining: int(c.Remaining), AvgRemainingRarity: c.AvgRemainingRarity,
			Priority: int(c.Priority), RemainingMissable: int(c.RemainingMissable), RemainingOnline: int(c.RemainingOnline), HoursLeft: left})
	}
	ranked := ranking.Rank(rc)
	if len(ranked) > 5 {
		ranked = ranked[:5]
	}
	b.WriteString("\nFILA PRÓXIMA PLATINA (melhores candidatos)\n")
	for _, rk := range ranked {
		c := byID[rk.ID]
		hours := "sem estimativa de horas"
		if rk.HoursLeft > 0 {
			hours = fmt.Sprintf("~%.0f h restantes", rk.HoursLeft)
		}
		fmt.Fprintf(&b, "- %s [%s]: %d/%d, faltam %d, %s, %d perdíveis, %d online.\n",
			c.Title, platformLabel(c.Platform), c.Earned, c.Total, c.Remaining, hours, c.RemainingMissable, c.RemainingOnline)
	}
	b.WriteString("\nEscreva o resumo: headline (1 frase), até 4 destaques curtos da semana (se a semana foi parada, diga com leveza), " +
		"até 3 sugestões de jogo da fila para a próxima semana com o porquê (horas, perdíveis, quanto falta) — em game só o nome do jogo, " +
		"sem a plataforma entre colchetes; só chame de raro o que estiver marcado como raro (< 5%); em trophies deixe vazio " +
		"se não souber nomes de troféus pelos dados — e uma frase final de incentivo.")
	return b.String(), nil
}

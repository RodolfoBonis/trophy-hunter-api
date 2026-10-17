package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/stream"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

// Escopos de guia e tipos de job.
const (
	ScopeTrophy  = "trophy"
	ScopeRoadmap = "roadmap"

	KindGuide = "ai_guide"
	KindChat  = "ai_chat"
)

// AllowedTools liberadas para o modelo (só leitura da web; nada de shell/arquivos).
const AllowedTools = "WebSearch,WebFetch"

// Erros de domínio expostos à API.
var (
	ErrChatBusy  = errors.New("já há uma resposta em andamento nesta conversa")
	ErrEmptyChat = errors.New("mensagem vazia")
)

// MaxChatMessage limita o tamanho de uma pergunta.
const MaxChatMessage = 4000

// Runner executa o modelo (claude.Runner em produção; fake nos testes).
type Runner interface {
	Run(ctx context.Context, req claude.Request, onEvent func(claude.Event)) (claude.Result, error)
}

// Deps injeta as dependências do serviço. Runner/Workdir só são necessários no ai-worker.
type Deps struct {
	Queries *dbgen.Queries
	Queue   queue.Queue
	Redis   *redis.Client
	Runner  Runner
	Model   string
	Workdir string
	Logger  *slog.Logger
}

// Service coordena pedidos (API) e execuções (ai-worker).
type Service struct {
	q       *dbgen.Queries
	queue   queue.Queue
	rdb     *redis.Client
	runner  Runner
	model   string
	workdir string
	logger  *slog.Logger
}

// New constrói o serviço.
func New(d Deps) *Service {
	return &Service{q: d.Queries, queue: d.Queue, rdb: d.Redis, runner: d.Runner, model: d.Model, workdir: d.Workdir, logger: d.Logger}
}

type guidePayload struct {
	GuideID  string `json:"guide_id"`
	GameID   string `json:"game_id"`
	TrophyID string `json:"trophy_id,omitempty"`
}

type chatPayload struct {
	MessageID string `json:"message_id"`
	GameID    string `json:"game_id"`
}

/* ── Pedidos (API) ─────────────────────────────────────────────────────────────────────── */

// RequestTrophyGuide cria (ou regenera) o guia do troféu e enfileira a geração.
func (s *Service) RequestTrophyGuide(ctx context.Context, t dbgen.GetTrophyWithGameRow) (dbgen.AiGuide, error) {
	g, err := s.q.UpsertGuidePending(ctx, dbgen.UpsertGuidePendingParams{
		Scope: ScopeTrophy, Platform: t.GamePlatform, GameExternalID: t.GameExternalID, TrophyExternalID: t.ExternalID,
	})
	if err != nil {
		return g, err
	}
	return g, s.enqueueGuide(ctx, guidePayload{GuideID: uuidString(g.ID), GameID: uuidString(t.GameID), TrophyID: uuidString(t.ID)})
}

// RequestRoadmap cria (ou regenera) o roadmap do jogo e enfileira a geração.
func (s *Service) RequestRoadmap(ctx context.Context, game dbgen.Game) (dbgen.AiGuide, error) {
	g, err := s.q.UpsertGuidePending(ctx, dbgen.UpsertGuidePendingParams{
		Scope: ScopeRoadmap, Platform: game.Platform, GameExternalID: game.ExternalID,
	})
	if err != nil {
		return g, err
	}
	return g, s.enqueueGuide(ctx, guidePayload{GuideID: uuidString(g.ID), GameID: uuidString(game.ID)})
}

func (s *Service) enqueueGuide(ctx context.Context, p guidePayload) error {
	if _, _, err := s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind: KindGuide, Payload: p, DedupeKey: KindGuide + ":" + p.GuideID, MaxAttempts: 6,
	}); err != nil {
		return err
	}
	s.status(ctx, p.GuideID, "Na fila")
	return nil
}

// SendChat grava a pergunta e a resposta pendente do assistente e enfileira a geração.
func (s *Service) SendChat(ctx context.Context, game dbgen.Game, text string) (user, assistant dbgen.ChatMessage, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return user, assistant, ErrEmptyChat
	}
	if len([]rune(text)) > MaxChatMessage {
		text = string([]rune(text)[:MaxChatMessage])
	}
	th, err := s.q.CreateThread(ctx, dbgen.CreateThreadParams{Platform: game.Platform, GameExternalID: game.ExternalID})
	if err != nil {
		return user, assistant, err
	}
	busy, err := s.q.ThreadBusy(ctx, th.ID)
	if err != nil {
		return user, assistant, err
	}
	if busy {
		return user, assistant, ErrChatBusy
	}
	if user, err = s.q.InsertChatMessage(ctx, dbgen.InsertChatMessageParams{ThreadID: th.ID, Role: "user", Content: text, Status: "done"}); err != nil {
		return user, assistant, err
	}
	if assistant, err = s.q.InsertChatMessage(ctx, dbgen.InsertChatMessageParams{ThreadID: th.ID, Role: "assistant", Status: "pending"}); err != nil {
		return user, assistant, err
	}
	id := uuidString(assistant.ID)
	if _, _, err = s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind: KindChat, Payload: chatPayload{MessageID: id, GameID: uuidString(game.ID)}, DedupeKey: KindChat + ":" + id, MaxAttempts: 4,
	}); err != nil {
		return user, assistant, err
	}
	s.status(ctx, id, "Na fila")
	return user, assistant, nil
}

// status publica um aviso avulso na stream (ex.: "Na fila"), sem encerrá-la.
func (s *Service) status(ctx context.Context, id, msg string) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream.Key(id), Values: map[string]any{"t": stream.TypeStatus, "d": msg}}).Err(); err != nil {
		s.logger.Warn("ai: falha ao publicar status", slog.String("err", err.Error()))
	}
}

/* ── Execução (ai-worker) ──────────────────────────────────────────────────────────────── */

// Handle executa um job de IA. Erro não-permanente = reagendar (ex.: limite de uso).
func (s *Service) Handle(ctx context.Context, job queue.Job) error {
	switch job.Kind {
	case KindGuide:
		var p guidePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return queue.Permanent(err)
		}
		return s.handleGuide(ctx, p)
	case KindChat:
		var p chatPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return queue.Permanent(err)
		}
		return s.handleChat(ctx, p)
	default:
		return queue.Permanent(fmt.Errorf("ai: job desconhecido %q", job.Kind))
	}
}

func (s *Service) handleGuide(ctx context.Context, p guidePayload) error {
	gid, err := parseUUID(p.GuideID)
	if err != nil {
		return queue.Permanent(err)
	}
	guide, err := s.q.GetGuideByID(ctx, gid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // apagado nesse meio tempo
	}
	if err != nil {
		return err
	}
	req := claude.Request{SystemPrompt: systemPrompt, Model: s.model, AllowedTools: AllowedTools, Workdir: s.dir("guides")}
	switch guide.Scope {
	case ScopeTrophy:
		tid, err := parseUUID(p.TrophyID)
		if err != nil {
			return queue.Permanent(err)
		}
		t, err := s.q.GetTrophyWithGame(ctx, tid)
		if err != nil {
			return s.failGuide(ctx, guide, "troféu não encontrado (sincronize o jogo e tente de novo)")
		}
		req.Prompt, req.JSONSchema = trophyGuidePrompt(t), TrophyGuideSchema
	default:
		game, trophies, err := s.gameContext(ctx, p.GameID)
		if err != nil {
			return s.failGuide(ctx, guide, "jogo não encontrado (sincronize e tente de novo)")
		}
		req.Prompt, req.JSONSchema = roadmapPrompt(game, trophies), RoadmapSchema
	}

	if err := s.q.MarkGuideRunning(ctx, guide.ID); err != nil {
		return err
	}
	pub := stream.NewPublisher(ctx, s.rdb, p.GuideID)
	res, err := s.runner.Run(ctx, req, forward(ctx, pub))
	if err != nil {
		_ = pub.Close(ctx, stream.TypeStatus, "Falha ao iniciar a IA — tentando de novo")
		return err
	}
	if res.RateLimited {
		metrics.AIJob(guide.Scope, "rate_limited")
		_ = pub.Close(ctx, stream.TypeStatus, "Limite de uso da IA atingido — nova tentativa em breve")
		return errors.New("ai: limite de uso")
	}
	if res.Err != "" {
		return s.finishGuideError(ctx, guide, pub, res.Err)
	}
	content, verr := ValidateGuide(guide.Scope, res.Structured)
	if verr != nil {
		return s.finishGuideError(ctx, guide, pub, verr.Error())
	}
	if err := s.q.CompleteGuide(ctx, dbgen.CompleteGuideParams{ID: guide.ID, Content: content, Model: s.modelName(), SchemaVersion: SchemaVersion}); err != nil {
		return err
	}
	metrics.AIJob(guide.Scope, "ok")
	_ = pub.Close(ctx, stream.TypeDone, "")
	return nil
}

func (s *Service) finishGuideError(ctx context.Context, guide dbgen.AiGuide, pub *stream.Publisher, msg string) error {
	_ = pub.Close(ctx, stream.TypeError, userMessage(msg))
	return s.failGuide(ctx, guide, msg)
}

func (s *Service) failGuide(ctx context.Context, guide dbgen.AiGuide, msg string) error {
	metrics.AIJob(guide.Scope, "error")
	s.logger.Warn("ai: guia falhou", slog.String("guide_id", uuidString(guide.ID)), slog.String("err", truncate(msg, 300)))
	if err := s.q.FailGuide(ctx, dbgen.FailGuideParams{ID: guide.ID, Error: userMessage(msg)}); err != nil {
		return err
	}
	return queue.Permanent(errors.New(msg))
}

func (s *Service) handleChat(ctx context.Context, p chatPayload) error {
	mid, err := parseUUID(p.MessageID)
	if err != nil {
		return queue.Permanent(err)
	}
	msg, err := s.q.GetChatMessage(ctx, mid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	th, err := s.q.GetThreadByID(ctx, msg.ThreadID)
	if err != nil {
		return err
	}
	all, err := s.q.ListThreadMessages(ctx, th.ID)
	if err != nil {
		return err
	}
	question, history := splitConversation(all, msg.ID)
	game, trophies, err := s.gameContext(ctx, p.GameID)
	if err != nil {
		return s.failChat(ctx, msg, nil, "jogo não encontrado (sincronize e tente de novo)")
	}
	if err := s.q.SetChatMessageRunning(ctx, msg.ID); err != nil {
		return err
	}

	pub := stream.NewPublisher(ctx, s.rdb, p.MessageID)
	req := claude.Request{SystemPrompt: systemPrompt, Model: s.model, AllowedTools: AllowedTools, Workdir: s.dir("chat", uuidString(th.ID))}
	run := func(resume string) (claude.Result, error) {
		req.ResumeID = resume
		req.Prompt = chatPrompt(game, trophies, history, question, resume != "")
		return s.runner.Run(ctx, req, forward(ctx, pub))
	}
	res, err := run(th.ClaudeSessionID)
	if err == nil && res.NoConversation {
		// Sessão perdida (pod recriado): recomeça com o histórico no prompt — o fio não se perde.
		res, err = run("")
	}
	if err != nil {
		_ = pub.Close(ctx, stream.TypeStatus, "Falha ao iniciar a IA — tentando de novo")
		return err
	}
	if res.RateLimited {
		metrics.AIJob("chat", "rate_limited")
		_ = pub.Close(ctx, stream.TypeStatus, "Limite de uso da IA atingido — nova tentativa em breve")
		return errors.New("ai: limite de uso")
	}
	if res.Err != "" {
		return s.failChat(ctx, msg, pub, res.Err)
	}
	text := strings.TrimSpace(res.Text)
	if err := s.q.CompleteChatMessage(ctx, dbgen.CompleteChatMessageParams{ID: msg.ID, Content: text}); err != nil {
		return err
	}
	if res.SessionID != "" && res.SessionID != th.ClaudeSessionID {
		if err := s.q.SetThreadSession(ctx, dbgen.SetThreadSessionParams{ID: th.ID, ClaudeSessionID: res.SessionID}); err != nil {
			s.logger.Warn("ai: falha ao gravar sessão", slog.String("err", err.Error()))
		}
	}
	metrics.AIJob("chat", "ok")
	_ = pub.Close(ctx, stream.TypeDone, "")
	return nil
}

func (s *Service) failChat(ctx context.Context, msg dbgen.ChatMessage, pub *stream.Publisher, cause string) error {
	metrics.AIJob("chat", "error")
	s.logger.Warn("ai: chat falhou", slog.String("message_id", uuidString(msg.ID)), slog.String("err", truncate(cause, 300)))
	if pub != nil {
		_ = pub.Close(ctx, stream.TypeError, userMessage(cause))
	}
	if err := s.q.FailChatMessage(ctx, dbgen.FailChatMessageParams{ID: msg.ID, Error: userMessage(cause)}); err != nil {
		return err
	}
	return queue.Permanent(errors.New(cause))
}

// splitConversation separa a pergunta que gerou a resposta msgID e o histórico anterior a ela
// (só mensagens concluídas).
func splitConversation(all []dbgen.ChatMessage, msgID pgtype.UUID) (string, []chatTurn) {
	idx := len(all)
	for i, m := range all {
		if m.ID == msgID {
			idx = i
			break
		}
	}
	question := ""
	qIdx := -1
	for i := idx - 1; i >= 0; i-- {
		if all[i].Role == "user" {
			question, qIdx = all[i].Content, i
			break
		}
	}
	var history []chatTurn
	for i := 0; i < qIdx; i++ {
		if all[i].Status == "done" && all[i].Content != "" {
			history = append(history, chatTurn{Role: all[i].Role, Content: all[i].Content})
		}
	}
	return question, history
}

// gameContext carrega o jogo (linha da conta) e os troféus para o prompt.
func (s *Service) gameContext(ctx context.Context, gameID string) (dbgen.Game, []dbgen.GameTrophiesForAIRow, error) {
	id, err := parseUUID(gameID)
	if err != nil {
		return dbgen.Game{}, nil, err
	}
	g, err := s.q.GetGameByID(ctx, id)
	if err != nil {
		return g, nil, err
	}
	trophies, err := s.q.GameTrophiesForAI(ctx, g.ID)
	return g, trophies, err
}

// forward repassa os eventos da CLI para a stream.
func forward(ctx context.Context, pub *stream.Publisher) func(claude.Event) {
	return func(e claude.Event) {
		switch e.Type {
		case claude.EventText:
			pub.Emit(ctx, stream.TypeText, e.Text)
		case claude.EventJSON:
			pub.Emit(ctx, stream.TypeJSON, e.Text)
		case claude.EventTool:
			pub.Emit(ctx, stream.TypeTool, e.Text)
		}
	}
}

func (s *Service) dir(parts ...string) string {
	d := filepath.Join(append([]string{s.workdir}, parts...)...)
	if err := os.MkdirAll(d, 0o750); err != nil {
		s.logger.Warn("ai: workdir", slog.String("err", err.Error()))
	}
	return d
}

func (s *Service) modelName() string {
	if s.model == "" {
		return "claude"
	}
	return s.model
}

// userMessage traduz o erro técnico em algo exibível (sem detalhes internos).
func userMessage(msg string) string {
	switch {
	case strings.Contains(msg, "tempo limite"):
		return "A IA demorou demais para responder. Tente de novo."
	case strings.Contains(msg, ErrInvalidGuide.Error()) || strings.Contains(msg, "saída estruturada"):
		return "A IA devolveu um guia incompleto. Tente regenerar."
	case strings.Contains(msg, "não encontrado"):
		return msg
	default:
		return "A IA não conseguiu responder. Tente de novo."
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, fmt.Errorf("uuid inválido %q: %w", s, err)
	}
	return u, nil
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func dbFailGuide(id pgtype.UUID, msg string) dbgen.FailGuideParams {
	return dbgen.FailGuideParams{ID: id, Error: msg}
}

func dbFailChat(id pgtype.UUID, msg string) dbgen.FailChatMessageParams {
	return dbgen.FailChatMessageParams{ID: id, Error: msg}
}

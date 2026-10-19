// Package push envia notificações Web Push (VAPID) para os navegadores inscritos e decide o que
// notificar ao USUÁRIO: desbloqueios (agregados), platina/100%, conta que precisa de ação dele
// (reautenticar a PSN, perfil Steam privado) e guias prontos — respeitando as preferências (settings
// "notifications"). Problemas de operação (sync falhando, IA sem cota, fila travada) NÃO viram push:
// são alertas do Prometheus → Alertmanager → Telegram (apps/trophy-hunter/monitoring no k3s-apps).
//
// O envio é sempre assíncrono: quem detecta o evento enfileira um job `notify` (Enqueue) e o worker
// de sync, único processo com a chave privada VAPID, entrega (Service.HandleJob). A API só envia o
// "teste" direto, para dar retorno imediato.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

// JobKind é o tipo do job de notificação na fila.
const JobKind = "notify"

// Kind classifica a notificação (cada uma tem uma preferência, exceto o teste).
type Kind string

// Tipos de notificação.
const (
	KindUnlocks     Kind = "unlocks"      // troféus novos (agregados) — calculado na hora do envio
	KindCompletions Kind = "completions"  // platina / 100% — sai junto com KindUnlocks
	KindReconnect   Kind = "reconnect"    // conta precisa de ação do usuário (NPSSO novo, perfil público)
	KindAIReady     Kind = "ai_ready"     // guia/roadmap pronto
	KindChallenges  Kind = "challenges"   // desafios entre amigos: convite, avanço e resultado
	KindWeeklyRecap Kind = "weekly_recap" // resumo semanal da IA (opt-in)
	KindBadges      Kind = "badges"       // medalha/temporada nova do app
	KindTest        Kind = "test"         // botão "Enviar teste" (ignora preferências)
)

// Payload é o conteúdo do job `notify`. Para KindUnlocks título/corpo são calculados no envio.
type Payload struct {
	// UserID: de quem é a notificação (vai só para os aparelhos dessa pessoa). Vazio em
	// KindUnlocks = todos os desbloqueios pendentes, cada um para o seu dono.
	UserID string `json:"user_id,omitempty"`
	Kind   Kind   `json:"kind"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	URL    string `json:"url,omitempty"`
	Tag    string `json:"tag,omitempty"`
}

// Message é o JSON entregue ao service worker (ver src/sw/service-worker.ts no web).
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
	Kind  Kind   `json:"kind"`
}

// Enqueue agenda uma notificação. dedupe (opcional) evita jobs iguais enfileirados ao mesmo tempo.
func Enqueue(ctx context.Context, q queue.Queue, p Payload, dedupe string) error {
	_, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: JobKind, Payload: p, DedupeKey: dedupe, MaxAttempts: 3})
	return err
}

// Config são as chaves VAPID. Sem chaves, o push fica desligado (Enabled() = false).
type Config struct {
	PublicKey  string
	PrivateKey string
	// Subject identifica o remetente para os push services (URL https ou mailto:).
	Subject string
}

// Sender entrega uma mensagem a uma inscrição; devolve o status HTTP do push service.
type Sender interface {
	Send(ctx context.Context, sub dbgen.PushSubscription, body []byte) (int, error)
}

// webpushSender é o Sender real (VAPID + criptografia aes128gcm via webpush-go).
type webpushSender struct {
	cfg    Config
	client *http.Client
}

func (w webpushSender) Send(ctx context.Context, sub dbgen.PushSubscription, body []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient: w.client, Subscriber: w.cfg.Subject, VAPIDPublicKey: w.cfg.PublicKey,
		VAPIDPrivateKey: w.cfg.PrivateKey, TTL: int((24 * time.Hour).Seconds()), Urgency: webpush.UrgencyNormal,
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// Service decide e entrega as notificações.
type Service struct {
	q      *dbgen.Queries
	sender Sender
	cfg    Config
	logger *slog.Logger
}

// New constrói o serviço. sender nil usa o envio real (webpush-go).
func New(q *dbgen.Queries, cfg Config, sender Sender, logger *slog.Logger) *Service {
	if sender == nil {
		sender = webpushSender{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}
	}
	return &Service{q: q, sender: sender, cfg: cfg, logger: logger}
}

// Enabled indica se há chaves VAPID configuradas.
func (s *Service) Enabled() bool { return s != nil && s.cfg.PublicKey != "" && s.cfg.PrivateKey != "" }

// PublicKey é a chave pública VAPID (applicationServerKey do navegador).
func (s *Service) PublicKey() string {
	if s == nil {
		return ""
	}
	return s.cfg.PublicKey
}

// maxFailures: inscrição que falha isso tudo seguido é removida.
const maxFailures = 10

// Result resume um envio.
type Result struct {
	Sent, Failed, Removed int
}

// Send entrega msg a todos os aparelhos do usuário (sem checar preferências — use Notify).
func (s *Service) Send(ctx context.Context, userID pgtype.UUID, msg Message) (Result, error) {
	var r Result
	if !s.Enabled() {
		return r, nil
	}
	subs, err := s.q.ListPushSubscriptions(ctx, userID)
	if err != nil {
		return r, err
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return r, err
	}
	for _, sub := range subs {
		status, serr := s.sender.Send(ctx, sub, body)
		switch {
		case serr == nil && status >= 200 && status < 300:
			r.Sent++
			_ = s.q.MarkPushSuccess(ctx, sub.ID)
			metrics.PushSent(string(msg.Kind), "ok")
		case status == http.StatusNotFound || status == http.StatusGone:
			// Inscrição expirada/revogada no navegador: some.
			r.Removed++
			_ = s.q.DeletePushSubscription(ctx, sub.ID)
			metrics.PushSent(string(msg.Kind), "gone")
		default:
			r.Failed++
			metrics.PushSent(string(msg.Kind), "error")
			n, _ := s.q.MarkPushFailure(ctx, sub.ID)
			if n >= maxFailures {
				_ = s.q.DeletePushSubscription(ctx, sub.ID)
			}
			reason := fmt.Sprintf("status %d", status)
			if serr != nil {
				reason = serr.Error()
			}
			s.logger.Warn("push: envio falhou", slog.String("kind", string(msg.Kind)), slog.String("reason", reason))
		}
	}
	return r, nil
}

// Notify entrega se o usuário quer este tipo de notificação.
func (s *Service) Notify(ctx context.Context, userID pgtype.UUID, msg Message) (Result, error) {
	if msg.Kind != KindTest {
		prefs, err := LoadPreferences(ctx, s.q, userID)
		if err != nil {
			return Result{}, err
		}
		if !prefs.Allows(msg.Kind) {
			return Result{}, nil
		}
	}
	return s.Send(ctx, userID, msg)
}

// HandleJob processa um job `notify` (chamado pelo worker de sync).
func (s *Service) HandleJob(ctx context.Context, job queue.Job) error {
	var p Payload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return queue.Permanent(fmt.Errorf("payload notify inválido: %w", err))
	}
	if !s.Enabled() {
		return nil // push não configurado: descarta
	}
	if p.Kind == KindUnlocks {
		return s.sendUnlocks(ctx)
	}
	var uid pgtype.UUID
	if err := uid.Scan(p.UserID); err != nil {
		return queue.Permanent(fmt.Errorf("notify sem user_id válido: %w", err))
	}
	_, err := s.Notify(ctx, uid, Message{Title: p.Title, Body: p.Body, URL: p.URL, Tag: p.Tag, Kind: p.Kind})
	return err
}

// sendUnlocks agrega os desbloqueios ainda não notificados: platina/100% em destaque (uma por
// jogo) e o resto num aviso só. Marca todos como notificados, mesmo os filtrados por preferência
// (senão um "liga a preferência" depois despejaria o histórico).
func (s *Service) sendUnlocks(ctx context.Context) error {
	rows, err := s.q.ListUnpushedUnlocks(ctx)
	if err != nil || len(rows) == 0 {
		return err
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	type batch struct {
		completions, others []dbgen.ListUnpushedUnlocksRow
	}
	byUser := map[pgtype.UUID]*batch{}
	var order []pgtype.UUID
	seen := map[pgtype.UUID]bool{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		b := byUser[r.UserID]
		if b == nil {
			b = &batch{}
			byUser[r.UserID] = b
			order = append(order, r.UserID)
		}
		if r.CompletesGame && !seen[r.GameID] {
			seen[r.GameID] = true
			b.completions = append(b.completions, r)
			continue
		}
		b.others = append(b.others, r)
	}
	for _, uid := range order {
		b := byUser[uid]
		for _, m := range unlockMessages(b.completions, b.others) {
			if _, err := s.Notify(ctx, uid, m); err != nil {
				return err
			}
		}
	}
	return s.q.MarkUnlocksPushed(ctx, ids)
}

// unlockMessages monta as notificações de desbloqueio. Separada para teste.
func unlockMessages(completions, others []dbgen.ListUnpushedUnlocksRow) []Message {
	var out []Message
	for _, c := range completions {
		title, body := "Platina! "+c.GameTitle, c.TrophyName
		if c.Platform == "steam" {
			title, body = "100%! "+c.GameTitle, "Última conquista: "+c.TrophyName
		}
		out = append(out, Message{Kind: KindCompletions, Title: title, Body: body, URL: gameURL(c.GameID), Tag: "completion-" + uuidString(c.GameID)})
	}
	switch n := len(others); {
	case n == 1:
		o := others[0]
		label := "Troféu desbloqueado"
		if o.Platform == "steam" {
			label = "Conquista desbloqueada"
		}
		out = append(out, Message{Kind: KindUnlocks, Title: label + ": " + o.TrophyName, Body: o.GameTitle, URL: gameURL(o.GameID), Tag: "unlocks"})
	case n > 1:
		games := []string{}
		gameSet := map[pgtype.UUID]bool{}
		for _, o := range others {
			if !gameSet[o.GameID] {
				gameSet[o.GameID] = true
				games = append(games, o.GameTitle)
			}
		}
		body := joinMax(games, 3)
		url := "/"
		if len(games) == 1 {
			url = gameURL(others[0].GameID)
		}
		out = append(out, Message{Kind: KindUnlocks, Title: fmt.Sprintf("%d troféus novos", n), Body: body, URL: url, Tag: "unlocks"})
	}
	return out
}

func joinMax(items []string, limit int) string {
	if len(items) <= limit {
		return joinComma(items)
	}
	return fmt.Sprintf("%s e mais %d", joinComma(items[:limit]), len(items)-limit)
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func gameURL(id pgtype.UUID) string { return "/games/" + uuidString(id) }

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

/* ── Preferências ──────────────────────────────────────────────────────────────────────── */

// settingsKey é a chave das preferências do usuário em `settings`.
func settingsKey(userID pgtype.UUID) string { return "notifications:" + uuidString(userID) }

// Preferences liga/desliga cada tipo de notificação.
type Preferences struct {
	Unlocks     bool `json:"unlocks"`
	Completions bool `json:"completions"`
	Reconnect   bool `json:"reconnect"`
	AIReady     bool `json:"ai_ready"`
	Challenges  bool `json:"challenges"`
	// WeeklyRecap liga o resumo semanal da IA (gera E avisa) — opt-in, começa desligado.
	WeeklyRecap bool `json:"weekly_recap"`
	Badges      bool `json:"badges"`
}

// DefaultPreferences devolve tudo ligado.
func DefaultPreferences() Preferences {
	return Preferences{Unlocks: true, Completions: true, Reconnect: true, AIReady: true, Challenges: true, Badges: true}
}

// Allows indica se o tipo está ligado.
func (p Preferences) Allows(k Kind) bool {
	switch k {
	case KindUnlocks:
		return p.Unlocks
	case KindCompletions:
		return p.Completions
	case KindReconnect:
		return p.Reconnect
	case KindAIReady:
		return p.AIReady
	case KindChallenges:
		return p.Challenges
	case KindBadges:
		return p.Badges
	case KindWeeklyRecap:
		return p.WeeklyRecap
	default:
		return true
	}
}

// LoadPreferences lê as preferências (padrão quando ainda não salvas).
func LoadPreferences(ctx context.Context, q *dbgen.Queries, userID pgtype.UUID) (Preferences, error) {
	p := DefaultPreferences()
	raw, err := q.GetSetting(ctx, settingsKey(userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return DefaultPreferences(), nil
	}
	return p, nil
}

// SavePreferences grava as preferências.
func SavePreferences(ctx context.Context, q *dbgen.Queries, userID pgtype.UUID, p Preferences) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return q.UpsertSetting(ctx, dbgen.UpsertSettingParams{Key: settingsKey(userID), Value: raw})
}

// UserIDString formata o uuid do usuário para o Payload.
func UserIDString(u pgtype.UUID) string { return uuidString(u) }

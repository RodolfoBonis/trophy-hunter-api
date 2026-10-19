// Package challenges implementa os desafios entre amigos: placar ao vivo, decisão do vencedor e os
// avisos (push) de convite, avanço e resultado. A decisão é pura (Decide) para ser testada isolada;
// o Service lê o banco e é chamado pela API (placar) e pelo worker (avaliação periódica).
package challenges

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
)

// Tipos de desafio.
const (
	KindFirstToComplete = "first_to_complete"
	KindMostTrophies    = "most_trophies"
	KindMostRare        = "most_rare"
)

// Milestones de progresso avisados aos outros participantes (first_to_complete).
var Milestones = []int{50, 75, 90}

// Standing é a situação de um participante.
type Standing struct {
	UserID pgtype.UUID
	Name   string
	State  string // invited | accepted | declined
	// first_to_complete
	HasGame       bool
	Earned, Total int
	CompletionPct float64
	LastUnlockAt  time.Time
	CompletedAt   *time.Time
	Milestone     int
	// most_trophies / most_rare
	Score int
}

// Done indica que fechou o jogo DENTRO do desafio (depois de começar).
func (s Standing) Done(startsAt time.Time) bool {
	return s.HasGame && s.Total > 0 && s.Earned == s.Total && !s.LastUnlockAt.Before(startsAt)
}

// Decision é o que fazer com um desafio aberto.
type Decision struct {
	Finish bool
	Winner *pgtype.UUID // nil = sem vencedor (prazo acabou / empate)
}

// Decide aplica as regras (só participantes que aceitaram contam):
//   - first_to_complete: vence quem fechou primeiro (data do último desbloqueio); sem ninguém e com
//     prazo vencido, termina sem vencedor;
//   - most_*: só termina no prazo; vence a maior contagem — empate no topo = sem vencedor.
func Decide(kind string, startsAt time.Time, endsAt *time.Time, now time.Time, st []Standing) Decision {
	accepted := make([]Standing, 0, len(st))
	for _, s := range st {
		if s.State == "accepted" {
			accepted = append(accepted, s)
		}
	}
	expired := endsAt != nil && !now.Before(*endsAt)
	switch kind {
	case KindFirstToComplete:
		var best *Standing
		for i := range accepted {
			s := accepted[i]
			if !s.Done(startsAt) {
				continue
			}
			if best == nil || s.LastUnlockAt.Before(best.LastUnlockAt) {
				best = &accepted[i]
			}
		}
		if best != nil {
			w := best.UserID
			return Decision{Finish: true, Winner: &w}
		}
		return Decision{Finish: expired}
	default:
		if !expired {
			return Decision{}
		}
		sort.SliceStable(accepted, func(i, j int) bool { return accepted[i].Score > accepted[j].Score })
		if len(accepted) == 0 || accepted[0].Score == 0 || (len(accepted) > 1 && accepted[1].Score == accepted[0].Score) {
			return Decision{Finish: true}
		}
		w := accepted[0].UserID
		return Decision{Finish: true, Winner: &w}
	}
}

// NextMilestone devolve o maior marco atingido acima do último avisado (0 = nada novo).
func NextMilestone(pct float64, last int) int {
	got := 0
	for _, m := range Milestones {
		if pct*100 >= float64(m) && m > last {
			got = m
		}
	}
	return got
}

// Service lê o banco e avisa os participantes.
type Service struct {
	q      *dbgen.Queries
	queue  queue.Queue // nil = sem push
	logger *slog.Logger
	now    func() time.Time
}

// New constrói o serviço.
func New(q *dbgen.Queries, jobs queue.Queue, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{q: q, queue: jobs, logger: logger, now: time.Now}
}

// Standings calcula a situação de todos os participantes de um desafio.
func (s *Service) Standings(ctx context.Context, c dbgen.Challenge) ([]Standing, error) {
	parts, err := s.q.ChallengeParticipants(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	until := s.now()
	if c.EndsAt.Valid && c.EndsAt.Time.Before(until) {
		until = c.EndsAt.Time
	}
	out := make([]Standing, 0, len(parts))
	for _, p := range parts {
		st := Standing{UserID: p.UserID, Name: p.UserName, State: p.State, Milestone: int(p.LastMilestone)}
		if p.CompletedAt.Valid {
			t := p.CompletedAt.Time
			st.CompletedAt = &t
		}
		if p.State == "accepted" {
			if c.Kind == KindFirstToComplete {
				g, err := s.q.ChallengeGameProgress(ctx, dbgen.ChallengeGameProgressParams{
					UserID: p.UserID, Platform: deref(c.GamePlatform), ExternalID: deref(c.GameExternalID), NormalizedTitle: deref(c.NormalizedTitle),
				})
				switch {
				case errors.Is(err, pgx.ErrNoRows):
				case err != nil:
					return nil, err
				default:
					st.HasGame, st.Earned, st.Total, st.CompletionPct = true, int(g.Earned), int(g.Total), g.CompletionPct
					st.LastUnlockAt = g.LastUnlockAt.Time
				}
			} else {
				n, err := s.q.ChallengeTrophyCount(ctx, dbgen.ChallengeTrophyCountParams{
					UserID: p.UserID, Since: c.StartsAt, Until: pgtype.Timestamptz{Time: until, Valid: true}, RareOnly: c.Kind == KindMostRare,
				})
				if err != nil {
					return nil, err
				}
				st.Score = int(n)
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// EvaluateAll avalia os desafios abertos (worker: após syncs e no ticker de manutenção).
func (s *Service) EvaluateAll(ctx context.Context) {
	open, err := s.q.ListOpenChallenges(ctx)
	if err != nil {
		s.logger.Warn("desafios: listar abertos falhou", slog.String("err", err.Error()))
		return
	}
	for _, c := range open {
		if err := s.evaluate(ctx, c); err != nil {
			s.logger.Warn("desafios: avaliação falhou", slog.String("challenge_id", uuidStr(c.ID)), slog.String("err", err.Error()))
		}
	}
}

func (s *Service) evaluate(ctx context.Context, c dbgen.Challenge) error {
	st, err := s.Standings(ctx, c)
	if err != nil {
		return err
	}
	var ends *time.Time
	if c.EndsAt.Valid {
		ends = &c.EndsAt.Time
	}
	if c.Kind == KindFirstToComplete {
		for _, p := range st {
			if p.State != "accepted" || !p.HasGame {
				continue
			}
			if p.Done(c.StartsAt.Time) && p.CompletedAt == nil {
				if err := s.q.MarkParticipantCompleted(ctx, dbgen.MarkParticipantCompletedParams{
					ChallengeID: c.ID, UserID: p.UserID, CompletedAt: pgtype.Timestamptz{Time: p.LastUnlockAt, Valid: true},
				}); err != nil {
					return err
				}
			}
			if m := NextMilestone(p.CompletionPct, p.Milestone); m > 0 && m < 100 && !p.Done(c.StartsAt.Time) {
				if err := s.q.SetParticipantMilestone(ctx, dbgen.SetParticipantMilestoneParams{ChallengeID: c.ID, UserID: p.UserID, LastMilestone: int16(m)}); err != nil { //nolint:gosec // 50..90
					return err
				}
				s.notifyOthers(ctx, c, st, p.UserID, fmt.Sprintf("%s chegou a %d%% em %s", p.Name, m, deref(c.GameTitle)), "Corre que ainda dá tempo.", fmt.Sprintf("m%d", m))
			}
		}
	}
	d := Decide(c.Kind, c.StartsAt.Time, ends, s.now(), st)
	if !d.Finish {
		return nil
	}
	params := dbgen.FinishChallengeParams{ID: c.ID}
	if d.Winner != nil {
		params.WinnerID = *d.Winner
	}
	n, err := s.q.FinishChallenge(ctx, params)
	if err != nil || n == 0 {
		return err
	}
	title, body := "Desafio encerrado: "+c.Title, "Acabou o prazo e ninguém venceu."
	if d.Winner != nil {
		for _, p := range st {
			if p.UserID == *d.Winner {
				title, body = p.Name+" venceu: "+c.Title, "Veja o placar final."
			}
		}
	}
	s.notifyOthers(ctx, c, st, pgtype.UUID{}, title, body, "end")
	s.logger.Info("desafio encerrado", slog.String("challenge_id", uuidStr(c.ID)), slog.Bool("has_winner", d.Winner != nil))
	return nil
}

// NotifyInvite avisa os convidados de um desafio novo.
func (s *Service) NotifyInvite(ctx context.Context, c dbgen.Challenge, creatorName string, invited []pgtype.UUID) {
	for _, u := range invited {
		s.notify(ctx, c, u, creatorName+" te desafiou", c.Title, "invite")
	}
}

// notifyOthers avisa todos os participantes que aceitaram, menos `except`.
func (s *Service) notifyOthers(ctx context.Context, c dbgen.Challenge, st []Standing, except pgtype.UUID, title, body, tag string) {
	for _, p := range st {
		if p.State == "accepted" && p.UserID != except {
			s.notify(ctx, c, p.UserID, title, body, tag)
		}
	}
}

func (s *Service) notify(ctx context.Context, c dbgen.Challenge, user pgtype.UUID, title, body, tag string) {
	if s.queue == nil {
		return
	}
	id := uuidStr(c.ID)
	p := push.Payload{
		UserID: push.UserIDString(user), Kind: push.KindChallenges, Title: title, Body: body,
		URL: "/friends/challenges/" + id, Tag: "challenge-" + id,
	}
	if err := push.Enqueue(ctx, s.queue, p, "challenge:"+id+":"+tag+":"+uuidStr(user)); err != nil {
		s.logger.Warn("desafios: enfileirar push falhou", slog.String("err", err.Error()))
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

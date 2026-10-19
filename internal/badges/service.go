package badges

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/recap"
)

// Service lê os agregados, grava as medalhas ganhas e avisa (push `badges`).
type Service struct {
	q      *dbgen.Queries
	queue  queue.Queue
	logger *slog.Logger
	now    func() time.Time
}

// New monta o serviço. queue nil = sem aviso por push.
func New(q *dbgen.Queries, jobs queue.Queue, logger *slog.Logger) *Service {
	return &Service{q: q, queue: jobs, logger: logger, now: time.Now}
}

// Snapshot é o estado da pessoa: andamento do catálogo e da temporada do mês.
type Snapshot struct {
	Progress []Progress
	Season   SeasonStats
}

// Award é uma medalha recém-ganha.
type Award struct {
	Key    string
	Season string
	Name   string
}

// Snapshot calcula sem gravar nada.
func (s *Service) Snapshot(ctx context.Context, userID pgtype.UUID) (Snapshot, error) {
	st, err := s.stats(ctx, userID)
	if err != nil {
		return Snapshot{}, err
	}
	season, err := s.season(ctx, userID, SeasonKey(s.now()))
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Progress: Evaluate(st), Season: season}, nil
}

// Sync grava o que a pessoa já alcançou e não tinha. Com notify, avisa por push só as novas — e
// nunca na primeira vez (o histórico inteiro de uma vez não é notícia; é o backfill). Leituras da
// API chamam sem notify (GET não dispara push); o worker chama com notify depois de cada sync.
func (s *Service) Sync(ctx context.Context, userID pgtype.UUID, notify bool) ([]Award, error) {
	had, err := s.q.CountUserBadges(ctx, userID)
	if err != nil {
		return nil, err
	}
	snap, err := s.Snapshot(ctx, userID)
	if err != nil {
		return nil, err
	}
	var awards []Award
	award := func(key, season, name string) error {
		inserted, err := s.q.AwardBadge(ctx, dbgen.AwardBadgeParams{UserID: userID, BadgeKey: key, Season: season})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) { // ON CONFLICT DO NOTHING não devolve linha
			return err
		}
		if inserted {
			awards = append(awards, Award{Key: key, Season: season, Name: name})
		}
		return nil
	}
	for _, p := range snap.Progress {
		if p.Earned {
			if err := award(p.Key, "", p.Name); err != nil {
				return nil, err
			}
		}
	}
	// Temporada do mês corrente e a do mês passado (sync atrasado na virada do mês).
	for _, key := range []string{snap.Season.Key, SeasonKey(s.now().AddDate(0, -1, 0))} {
		st := snap.Season
		if key != snap.Season.Key {
			if st, err = s.season(ctx, userID, key); err != nil {
				return nil, err
			}
		}
		if SeasonDone(st) {
			if err := award("season", key, SeasonLabel(key)); err != nil {
				return nil, err
			}
		}
	}
	if notify && had > 0 && len(awards) > 0 {
		s.notify(ctx, userID, awards)
	}
	return awards, nil
}

// SyncAll avalia todo mundo ativo (worker, depois de cada sync).
func (s *Service) SyncAll(ctx context.Context) {
	ids, err := s.q.ActiveUserIDs(ctx)
	if err != nil {
		s.logger.Warn("medalhas: listar usuários falhou", slog.String("err", err.Error()))
		return
	}
	for _, id := range ids {
		if _, err := s.Sync(ctx, id, true); err != nil {
			s.logger.Warn("medalhas: avaliar falhou", slog.String("err", err.Error()))
		}
	}
}

func (s *Service) notify(ctx context.Context, userID pgtype.UUID, awards []Award) {
	if s.queue == nil {
		return
	}
	title := "Medalha nova: " + awards[0].Name
	body := "Veja na sua vitrine de medalhas."
	if len(awards) > 1 {
		title = fmt.Sprintf("%d medalhas novas", len(awards))
		body = awards[0].Name + " e mais " + fmt.Sprint(len(awards)-1)
	}
	uid := uuidStr(userID)
	p := push.Payload{UserID: uid, Kind: push.KindBadges, Title: title, Body: body, URL: "/badges", Tag: "badges"}
	if err := push.Enqueue(ctx, s.queue, p, "badges:"+uid+":"+awards[0].Key+awards[0].Season); err != nil {
		s.logger.Warn("medalhas: enfileirar push falhou", slog.String("err", err.Error()))
	}
}

func (s *Service) stats(ctx context.Context, userID pgtype.UUID) (Stats, error) {
	r, err := s.q.BadgeStats(ctx, userID)
	if err != nil {
		return Stats{}, err
	}
	days, err := s.q.RecapDays(ctx, dbgen.RecapDaysParams{
		UserID: userID,
		Since:  pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
		Until:  pgtype.Timestamptz{Time: s.now().Add(24 * time.Hour), Valid: true},
	})
	if err != nil {
		return Stats{}, err
	}
	ds := make([]time.Time, 0, len(days))
	for _, d := range days {
		ds = append(ds, d.Day.Time)
	}
	return Stats{
		Earned: int(r.Earned), Platinums: int(r.PsnPlatinum + r.SteamCompleted), Rare: int(r.Rare), UltraRare: int(r.UltraRare),
		Completed: int(r.Completed), SteamComplete: int(r.SteamCompleted), PSNPlatinum: int(r.PsnPlatinum),
		LongestStreak: recap.LongestStreak(ds), NightUnlocks: int(r.Night), ChallengesWon: int(r.ChallengesWon),
	}, nil
}

func (s *Service) season(ctx context.Context, userID pgtype.UUID, key string) (SeasonStats, error) {
	from, to, err := SeasonBounds(key)
	if err != nil {
		return SeasonStats{}, err
	}
	r, err := s.q.SeasonStats(ctx, dbgen.SeasonStatsParams{
		UserID: userID, Since: pgtype.Timestamptz{Time: from, Valid: true}, Until: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return SeasonStats{}, err
	}
	return SeasonStats{Key: key, Platinums: int(r.Platinums), Rare: int(r.Rare)}, nil
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Package backlog cuida dos jogos que a pessoa ainda não tem ("quero platinar"), com preço no Brasil e aviso
// de promoção. Fontes: loja da Steam (sem chave; preço da própria Steam), ITAD (opcional, chave:
// melhor preço entre lojas de PC + menor histórico) e PS Store (não oficial, ver psstore).
package backlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/itad"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psstore"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steamstore"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// Lojas que o serviço usa (interfaces para os testes).
type (
	// SteamStore é a loja da Steam (busca e preço).
	SteamStore interface {
		Search(ctx context.Context, term string) ([]steamstore.App, error)
		Price(ctx context.Context, appID int) (steamstore.Price, bool, error)
	}
	// PSStore é a busca da PlayStation Store.
	PSStore interface {
		Search(ctx context.Context, term string) ([]psstore.Product, error)
	}
	// ITAD é o IsThereAnyDeal (melhor preço em lojas de PC e menor histórico).
	ITAD interface {
		Enabled() bool
		LookupSteam(ctx context.Context, appID int) (string, bool, error)
		Prices(ctx context.Context, ids []string) (map[string]itad.Prices, error)
	}
)

// Service junta as lojas.
type Service struct {
	q      *dbgen.Queries
	steam  SteamStore
	ps     PSStore
	itad   ITAD
	queue  queue.Queue
	logger *slog.Logger
}

// New monta o serviço. queue nil = sem aviso de promoção.
func New(q *dbgen.Queries, steam SteamStore, ps PSStore, it ITAD, jobs queue.Queue, logger *slog.Logger) *Service {
	return &Service{q: q, steam: steam, ps: ps, itad: it, queue: jobs, logger: logger}
}

// Item é um resultado da busca no catálogo das lojas.
type Item struct {
	Platform     string
	StoreID      string
	Title        string
	Cover        string
	PriceCents   *int
	RegularCents *int
	Cut          int
	Owned        bool
}

// psKinds: o que conta como "jogo" na PS Store (fora DLC, moedas, temas…).
var psKinds = map[string]bool{"FULL_GAME": true, "GAME_BUNDLE": true, "PREMIUM_EDITION": true}

// Search busca nas duas lojas ao mesmo tempo. Uma loja fora do ar não derruba a outra: ela vai em
// unavailable e a UI avisa.
func (s *Service) Search(ctx context.Context, userID pgtype.UUID, term string) (items []Item, unavailable []string, err error) {
	var (
		wg              sync.WaitGroup
		steamApps       []steamstore.App
		psProds         []psstore.Product
		steamErr, psErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); steamApps, steamErr = s.steam.Search(ctx, term) }()
	go func() { defer wg.Done(); psProds, psErr = s.ps.Search(ctx, term) }()
	wg.Wait()
	if steamErr != nil {
		unavailable = append(unavailable, "steam")
		s.logger.Warn("backlog: busca steam falhou", slog.String("err", steamErr.Error()))
	}
	if psErr != nil {
		unavailable = append(unavailable, "psn")
		s.logger.Warn("backlog: busca ps store falhou", slog.String("err", psErr.Error()))
	}
	for _, a := range steamApps {
		it := Item{Platform: "steam", StoreID: strconv.Itoa(a.ID), Title: a.Name, Cover: a.Image}
		if a.Price != nil {
			it.PriceCents, it.RegularCents, it.Cut = ptr(a.Price.FinalCents), ptr(a.Price.InitialCents), a.Price.Discount
		}
		items = append(items, it)
	}
	for _, p := range psProds {
		if !psKinds[p.Classification] {
			continue
		}
		it := Item{Platform: "psn", StoreID: p.ID, Title: p.Name, Cover: p.Image}
		if p.FinalCents > 0 || p.Free {
			it.PriceCents, it.RegularCents, it.Cut = ptr(p.FinalCents), ptr(p.BaseCents), cut(p.BaseCents, p.FinalCents)
		}
		items = append(items, it)
	}
	titles := make([]string, 0, len(items))
	for _, it := range items {
		titles = append(titles, syncpkg.NormalizeTitle(it.Title))
	}
	owned, err := s.q.OwnedNormalizedTitles(ctx, dbgen.OwnedNormalizedTitlesParams{UserID: userID, Titles: titles})
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, t := range owned {
		have[t] = true
	}
	for i := range items {
		items[i].Owned = have[titles[i]]
	}
	return items, unavailable, nil
}

// Add põe um jogo na lista e já tenta buscar o preço (sem travar se a loja falhar).
func (s *Service) Add(ctx context.Context, userID pgtype.UUID, platform, storeID, title, cover string, target *int32) (pgtype.UUID, error) {
	if platform != "steam" && platform != "psn" {
		return pgtype.UUID{}, ErrInvalid
	}
	if platform == "steam" {
		if _, err := strconv.Atoi(storeID); err != nil {
			return pgtype.UUID{}, ErrInvalid
		}
	}
	title = strings.TrimSpace(title)
	if storeID == "" || title == "" || len(title) > 300 {
		return pgtype.UUID{}, ErrInvalid
	}
	id, err := s.q.AddWishlist(ctx, dbgen.AddWishlistParams{
		UserID: userID, Platform: platform, StoreID: storeID, Title: title,
		NormalizedTitle: syncpkg.NormalizeTitle(title), Cover: cover, TargetPriceCents: target,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	if err := s.refreshOne(ctx, platform, storeID, title); err != nil {
		s.logger.Warn("backlog: preço inicial falhou", slog.String("platform", platform), slog.String("err", err.Error()))
	}
	return id, nil
}

// ErrInvalid indica item malformado.
var ErrInvalid = errors.New("backlog: item inválido")

// RefreshPrices atualiza um lote de preços velhos e avisa as promoções. Para no primeiro 429.
func (s *Service) RefreshPrices(ctx context.Context, batch int) error {
	rows, err := s.q.StaleWishlistPrices(ctx, int32(batch)) //nolint:gosec // lote pequeno
	if err != nil {
		return err
	}
	for _, r := range rows {
		err := s.refreshOne(ctx, r.Platform, r.StoreID, r.Title)
		if errors.Is(err, steamstore.ErrRateLimited) || errors.Is(err, psstore.ErrRateLimited) || errors.Is(err, itad.ErrRateLimited) {
			s.logger.Info("backlog: loja pediu para esperar; continua na próxima rodada")
			break
		}
		if err != nil {
			s.logger.Warn("backlog: preço falhou", slog.String("platform", r.Platform), slog.String("err", err.Error()))
		}
	}
	return s.notifyDeals(ctx)
}

func (s *Service) refreshOne(ctx context.Context, platform, storeID, title string) error {
	var (
		p   dbgen.UpsertGamePriceParams
		err error
	)
	switch platform {
	case "steam":
		p, err = s.steamPrice(ctx, storeID)
	case "psn":
		p, err = s.psPrice(ctx, storeID, title)
	default:
		return ErrInvalid
	}
	if err != nil {
		if terr := s.q.TouchGamePriceError(ctx, dbgen.TouchGamePriceErrorParams{Platform: platform, StoreID: storeID}); terr != nil {
			return terr
		}
		return err
	}
	return s.q.UpsertGamePrice(ctx, p)
}

func (s *Service) steamPrice(ctx context.Context, storeID string) (dbgen.UpsertGamePriceParams, error) {
	appID, _ := strconv.Atoi(storeID)
	p := dbgen.UpsertGamePriceParams{Platform: "steam", StoreID: storeID, Currency: "BRL", Status: "not_found", Shop: "Steam", Url: steamstore.StoreURL(appID)}
	sp, ok, err := s.steam.Price(ctx, appID)
	if err != nil {
		return p, err
	}
	if ok {
		p.Status, p.PriceCents, p.RegularCents, p.Cut = "ok", ptr32(sp.FinalCents), ptr32(sp.InitialCents), int32(sp.Discount) //nolint:gosec // 0..100
	}
	if s.itad == nil || !s.itad.Enabled() {
		return p, nil
	}
	// ITAD: melhor preço entre lojas de PC (pode ser mais barato que a Steam) e o menor histórico.
	gid, found, err := s.itad.LookupSteam(ctx, appID)
	if err != nil || !found {
		return p, err
	}
	prices, err := s.itad.Prices(ctx, []string{gid})
	if err != nil {
		return p, err
	}
	ip := prices[gid]
	if ip.LowCents != nil {
		p.LowCents = ptr32(*ip.LowCents)
	}
	if b := ip.Best; b != nil && b.Currency == "BRL" && (p.PriceCents == nil || int32(b.Cents) < *p.PriceCents) { //nolint:gosec // centavos
		p.Status, p.PriceCents, p.RegularCents, p.Cut, p.Shop, p.Url = "ok", ptr32(b.Cents), ptr32(b.RegularCents), int32(b.Cut), b.Shop, b.URL //nolint:gosec // 0..100
	}
	return p, nil
}

func (s *Service) psPrice(ctx context.Context, storeID, title string) (dbgen.UpsertGamePriceParams, error) {
	p := dbgen.UpsertGamePriceParams{Platform: "psn", StoreID: storeID, Currency: "BRL", Status: "not_found", Shop: "PlayStation Store", Url: psstore.StoreURL(storeID)}
	prods, err := s.ps.Search(ctx, title)
	if err != nil {
		return p, err
	}
	for _, pr := range prods {
		if pr.ID != storeID {
			continue
		}
		if pr.FinalCents > 0 || pr.Free {
			p.Status, p.PriceCents, p.RegularCents, p.Cut = "ok", ptr32(pr.FinalCents), ptr32(pr.BaseCents), int32(cut(pr.BaseCents, pr.FinalCents)) //nolint:gosec // 0..100
		}
		break
	}
	return p, nil
}

// notifyDeals avisa (push `deals`) quem tem item no alvo ou no menor preço histórico.
func (s *Service) notifyDeals(ctx context.Context) error {
	deals, err := s.q.DealsToNotify(ctx)
	if err != nil {
		return err
	}
	for _, d := range deals {
		if s.queue != nil && d.PriceCents != nil {
			why := "chegou no seu preço"
			if d.HistoricLow {
				why = "no menor preço que já teve"
			}
			body := fmt.Sprintf("%s: %s%s (%s)", d.Shop, BRL(int(*d.PriceCents)), cutText(int(d.Cut)), why)
			uid := uuidStr(d.UserID)
			p := push.Payload{UserID: uid, Kind: push.KindDeals, Title: "Promoção: " + d.Title, Body: body, URL: "/next?tab=comprar", Tag: "deal-" + uuidStr(d.ID)}
			if err := push.Enqueue(ctx, s.queue, p, "deal:"+uuidStr(d.ID)+":"+strconv.Itoa(int(*d.PriceCents))); err != nil {
				s.logger.Warn("backlog: enfileirar push falhou", slog.String("err", err.Error()))
				continue
			}
		}
		if err := s.q.MarkWishlistNotified(ctx, dbgen.MarkWishlistNotifiedParams{ID: d.ID, NotifiedPriceCents: d.PriceCents}); err != nil {
			return err
		}
	}
	return nil
}

// BRL formata centavos: 14995 → "R$ 149,95".
func BRL(cents int) string {
	reais := cents / 100
	s := strconv.Itoa(reais)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	return fmt.Sprintf("R$ %s,%02d", s, cents%100)
}

func cutText(c int) string {
	if c <= 0 {
		return ""
	}
	return fmt.Sprintf(" −%d%%", c)
}

func cut(base, final int) int {
	if base <= 0 || final >= base {
		return 0
	}
	return (base - final) * 100 / base
}

func ptr(v int) *int { return &v }

func ptr32(v int) *int32 {
	x := int32(v) //nolint:gosec // centavos cabem em int32
	return &x
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

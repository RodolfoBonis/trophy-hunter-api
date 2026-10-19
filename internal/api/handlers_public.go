package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/publiccard"
)

// Perfil público (opt-in): vitrine enxuta sem login. Nunca sai e-mail, contas Steam/PSN, notas,
// marcações nem lista de desejos — só o que está no publicProfileDTO.
//   PUT /me/public-profile                      liga/desliga, escolhe link e apelido
//   GET /public/profiles/{slug}                 JSON da página pública (sem auth, limite por IP)
//   GET /public/profiles/{slug}/card.png        cartão 1200×630 (og:image)
//   GET /public/profiles/{slug}/share           HTML com og:* para robôs + redirect para a página

var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,28}[a-z0-9])$`)

// Links que não podem virar perfil (rotas do app/infra ou que confundem).
var reservedSlugs = []string{"admin", "api", "app", "settings", "perfil", "u", "convite", "login", "logout", "legal", "static", "assets", "trophy", "trophy-hunter", "dono", "owner", "suporte"}

type publicProfileRequest struct {
	Enabled bool   `json:"enabled"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
}

func (h *Handlers) putPublicProfile(w http.ResponseWriter, r *http.Request) {
	var req publicProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo inválido"))
		return
	}
	me := currentUser(r)
	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = firstWord(me.Name)
	}
	var slugArg *string
	if slug != "" {
		if !slugRe.MatchString(slug) || slices.Contains(reservedSlugs, slug) {
			httperr.Write(w, r, httperr.New(httperr.KindValidation, "Validation", "link: 3 a 30 letras minúsculas, números ou hífen (e não pode ser uma palavra reservada)").WithCode("slug_invalid"))
			return
		}
		slugArg = &slug
	}
	if req.Enabled && slugArg == nil {
		httperr.Write(w, r, httperr.New(httperr.KindValidation, "Validation", "escolha um link para o perfil público").WithCode("slug_invalid"))
		return
	}
	if len([]rune(name)) > 40 {
		httperr.Write(w, r, httperr.New(httperr.KindValidation, "Validation", "apelido com até 40 caracteres"))
		return
	}
	u, err := h.q.SetPublicProfile(r.Context(), dbgen.SetPublicProfileParams{ID: me.ID, Enabled: req.Enabled, Slug: slugArg, PublicName: name})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", "esse link já é de outra pessoa").WithCode("slug_taken"))
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMeDTO(u))
}

type publicBadgeDTO struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Tier   string `json:"tier"`
	Season string `json:"season,omitempty"`
}

type publicRareDTO struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Icon      string  `json:"icon"`
	RarityPct float64 `json:"rarity_pct"`
	GameTitle string  `json:"game_title"`
	Platform  string  `json:"platform"`
}

type publicCompletionDTO struct {
	Title       string     `json:"title"`
	Platform    string     `json:"platform"`
	ExternalID  string     `json:"external_id"`
	Cover       string     `json:"cover"`
	Hero        string     `json:"hero"`
	CompletedAt *time.Time `json:"completed_at"`
}

type publicProfileDTO struct {
	Slug        string                `json:"slug"`
	Name        string                `json:"name"`
	Platinums   int                   `json:"platinums"`
	Trophies    int                   `json:"trophies"`
	Rare        int                   `json:"rare"`
	Completed   int                   `json:"completed"`
	Badges      []publicBadgeDTO      `json:"badges"`
	Rarest      []publicRareDTO       `json:"rarest"`
	Completions []publicCompletionDTO `json:"completions"`
}

// publicFor monta a vitrine. found=false para link inexistente, desligado ou pessoa inativa.
func (h *Handlers) publicFor(ctx context.Context, slug string) (publicProfileDTO, bool, error) {
	slug = strings.ToLower(slug)
	if !slugRe.MatchString(slug) {
		return publicProfileDTO{}, false, nil
	}
	u, err := h.q.GetPublicUser(ctx, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return publicProfileDTO{}, false, nil
	}
	if err != nil {
		return publicProfileDTO{}, false, err
	}
	st, err := h.statsFor(ctx, u.ID)
	if err != nil {
		return publicProfileDTO{}, false, err
	}
	// Totais pela mesma regra das medalhas (melhor conta por jogo; "platina" = PSN ou 100% Steam).
	agg, err := h.q.BadgeStats(ctx, u.ID)
	if err != nil {
		return publicProfileDTO{}, false, err
	}
	if _, err := h.badges.Sync(ctx, u.ID, false); err != nil {
		return publicProfileDTO{}, false, err
	}
	rows, err := h.q.ListUserBadges(ctx, u.ID)
	if err != nil {
		return publicProfileDTO{}, false, err
	}
	out := publicProfileDTO{
		Slug: slug, Name: u.PublicName, Trophies: int(agg.Earned), Rare: int(agg.Rare),
		Platinums: int(agg.PsnPlatinum + agg.SteamCompleted), Completed: int(agg.Completed),
		Badges: []publicBadgeDTO{}, Rarest: []publicRareDTO{}, Completions: []publicCompletionDTO{},
	}
	if out.Name == "" {
		out.Name = firstWord(u.Name)
	}
	for _, b := range rows {
		d := publicBadgeDTO{Key: b.BadgeKey, Season: b.Season}
		if def, ok := catalogByKey[b.BadgeKey]; ok {
			d.Name, d.Tier = def.Name, string(def.Tier)
		} else if b.BadgeKey == "season" {
			d.Name, d.Tier = "Temporada "+b.Season, "gold"
		} else {
			continue
		}
		out.Badges = append(out.Badges, d)
	}
	for i, x := range st.Rarest {
		if i == 5 {
			break
		}
		out.Rarest = append(out.Rarest, publicRareDTO{Name: x.Name, Type: x.Type, Icon: x.Icon, RarityPct: x.RarityPct, GameTitle: x.GameTitle, Platform: x.Platform})
	}
	for i, c := range st.Completions {
		if i == 6 {
			break
		}
		out.Completions = append(out.Completions, publicCompletionDTO{Title: c.Title, Platform: c.Platform, ExternalID: c.ExternalID, Cover: c.Cover, Hero: c.Hero, CompletedAt: c.CompletedAt})
	}
	return out, true, nil
}

func (h *Handlers) publicAllowed(w http.ResponseWriter, r *http.Request) bool {
	if !h.clientErrors.allow("public:" + clientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		return false
	}
	return true
}

func (h *Handlers) getPublicProfile(w http.ResponseWriter, r *http.Request) {
	if !h.publicAllowed(w, r) {
		return
	}
	p, ok, err := h.publicFor(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if !ok {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "perfil não encontrado"))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, p)
}

func (h *Handlers) getPublicCard(w http.ResponseWriter, r *http.Request) {
	if !h.publicAllowed(w, r) {
		return
	}
	p, ok, err := h.publicFor(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	card := publiccard.Card{Name: p.Name, Platinums: p.Platinums, Trophies: p.Trophies, Rare: p.Rare, Badges: len(p.Badges)}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", card)))
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	png, err := publiccard.Render(card, strings.TrimPrefix(strings.TrimPrefix(h.appURL, "https://"), "http://")+"/u/"+p.Slug)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(png)
}

var shareTmpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8">
<title>{{.Title}}</title>
<meta name="description" content="{{.Desc}}">
<meta property="og:type" content="profile">
<meta property="og:site_name" content="Trophy Hunter">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Desc}}">
<meta property="og:image" content="{{.Image}}">
<meta property="og:image:width" content="1200"><meta property="og:image:height" content="630">
<meta property="og:url" content="{{.URL}}">
<meta name="twitter:card" content="summary_large_image">
<meta http-equiv="refresh" content="0; url={{.Page}}">
</head><body><a href="{{.Page}}">Ver o perfil de {{.Name}} no Trophy Hunter</a></body></html>`))

// sharePage é o que robôs (WhatsApp, Discord, Telegram) leem ao colar o link /u/<slug>; gente é
// redirecionada na hora para a página do app.
func (h *Handlers) sharePage(w http.ResponseWriter, r *http.Request) {
	if !h.publicAllowed(w, r) {
		return
	}
	p, ok, err := h.publicFor(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Perfil não encontrado · Trophy Hunter</title><p>Perfil não encontrado.</p>`))
		return
	}
	base := strings.TrimRight(h.appURL, "/")
	data := map[string]string{
		"Name":  p.Name,
		"Title": p.Name + " no Trophy Hunter",
		"Desc":  fmt.Sprintf("%d platinas · %d troféus · %d raros · %d medalhas", p.Platinums, p.Trophies, p.Rare, len(p.Badges)),
		"Image": base + "/u/" + p.Slug + "/card.png",
		"URL":   base + "/u/" + p.Slug,
		"Page":  base + "/perfil/" + p.Slug,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = shareTmpl.Execute(w, data)
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "Caçador"
	}
	return f[0]
}

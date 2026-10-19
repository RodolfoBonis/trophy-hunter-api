// Package badges implementa as medalhas e temporadas do próprio Trophy Hunter, calculadas sobre o progresso já
// sincronizado. catalog.go é puro (regras + progresso); service.go lê o banco e grava as ganhas.
package badges

import (
	"fmt"
	"sort"
	"time"
)

// Stats são os agregados da pessoa (melhor conta por jogo — jogo em 2 contas não conta dobrado).
type Stats struct {
	Earned        int // troféus/conquistas obtidos
	Platinums     int // platina PSN + 100% na Steam (a "platina" do app)
	Rare          int // raridade < 5%
	UltraRare     int // raridade < 1%
	Completed     int // jogos 100%
	SteamComplete int // jogos 100% na Steam
	PSNPlatinum   int // platinas PSN
	LongestStreak int // dias seguidos com desbloqueio (Brasília)
	NightUnlocks  int // desbloqueios entre 0h e 5h (Brasília)
	ChallengesWon int
}

// Tier ordena a vitrine (e dá a cor na UI).
type Tier string

// Tiers das medalhas.
const (
	TierBronze   Tier = "bronze"
	TierSilver   Tier = "silver"
	TierGold     Tier = "gold"
	TierPlatinum Tier = "platinum"
)

// Def é uma medalha do catálogo.
type Def struct {
	Key         string
	Name        string
	Description string
	Tier        Tier
	Goal        int
	value       func(Stats) int
}

func ladder(key, name, desc string, tier Tier, goal int, v func(Stats) int) Def {
	return Def{Key: key, Name: name, Description: desc, Tier: tier, Goal: goal, value: v}
}

var (
	platinums = func(s Stats) int { return s.Platinums }
	earned    = func(s Stats) int { return s.Earned }
	rare      = func(s Stats) int { return s.Rare }
	ultra     = func(s Stats) int { return s.UltraRare }
	completed = func(s Stats) int { return s.Completed }
	streak    = func(s Stats) int { return s.LongestStreak }
	won       = func(s Stats) int { return s.ChallengesWon }
)

// Catalog é o catálogo de medalhas, na ordem da vitrine.
var Catalog = []Def{
	ladder("platinum_1", "Primeira platina", "Platina na PSN ou 100% na Steam pela primeira vez.", TierBronze, 1, platinums),
	ladder("platinum_10", "Dez platinas", "10 platinas (ou 100% na Steam).", TierSilver, 10, platinums),
	ladder("platinum_25", "Vinte e cinco platinas", "25 platinas (ou 100% na Steam).", TierGold, 25, platinums),
	ladder("platinum_50", "Cinquenta platinas", "50 platinas (ou 100% na Steam).", TierPlatinum, 50, platinums),
	ladder("trophies_500", "500 troféus", "500 troféus e conquistas obtidos.", TierBronze, 500, earned),
	ladder("trophies_1000", "Mil troféus", "1.000 troféus e conquistas obtidos.", TierSilver, 1000, earned),
	ladder("trophies_5000", "Cinco mil troféus", "5.000 troféus e conquistas obtidos.", TierGold, 5000, earned),
	ladder("rare_10", "Caçador de raros", "10 troféus com menos de 5% dos jogadores.", TierBronze, 10, rare),
	ladder("rare_50", "Colecionador de raros", "50 troféus com menos de 5% dos jogadores.", TierGold, 50, rare),
	ladder("ultra_1", "Ultra raro", "Um troféu que menos de 1% dos jogadores tem.", TierSilver, 1, ultra),
	ladder("ultra_10", "Lenda", "10 troféus com menos de 1% dos jogadores.", TierPlatinum, 10, ultra),
	ladder("complete_10", "Completista", "10 jogos com 100%.", TierSilver, 10, completed),
	ladder("complete_25", "Completista de carteirinha", "25 jogos com 100%.", TierGold, 25, completed),
	ladder("streak_7", "Semana cheia", "7 dias seguidos desbloqueando algo.", TierBronze, 7, streak),
	ladder("streak_30", "Mês sem folga", "30 dias seguidos desbloqueando algo.", TierGold, 30, streak),
	ladder("night_25", "Caçador noturno", "25 troféus entre meia-noite e 5h.", TierBronze, 25, func(s Stats) int { return s.NightUnlocks }),
	ladder("both_platforms", "Dois mundos", "100% na Steam e platina na PSN.", TierSilver, 1, func(s Stats) int {
		return min(s.SteamComplete, s.PSNPlatinum)
	}),
	ladder("challenge_1", "Primeira vitória", "Ganhou um desafio contra um amigo.", TierBronze, 1, won),
	ladder("challenge_5", "Imbatível", "Ganhou 5 desafios contra amigos.", TierGold, 5, won),
}

// Progress é uma medalha com o andamento da pessoa.
type Progress struct {
	Def
	Value  int
	Earned bool
}

// Evaluate devolve o andamento de cada medalha do catálogo (Value limitado ao Goal).
func Evaluate(s Stats) []Progress {
	out := make([]Progress, 0, len(Catalog))
	for _, d := range Catalog {
		v := d.value(s)
		out = append(out, Progress{Def: d, Value: min(v, d.Goal), Earned: v >= d.Goal})
	}
	return out
}

// Next são as medalhas ainda não ganhas, mais perto de sair primeiro (fração do objetivo).
func Next(ps []Progress, n int) []Progress {
	var open []Progress
	for _, p := range ps {
		if !p.Earned {
			open = append(open, p)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		fi := float64(open[i].Value) / float64(open[i].Goal)
		fj := float64(open[j].Value) / float64(open[j].Goal)
		return fi > fj
	})
	if len(open) > n {
		open = open[:n]
	}
	return open
}

// Metas da temporada (mês, Brasília): uma das duas fecha a temporada.
const (
	SeasonPlatinumGoal = 3
	SeasonRareGoal     = 10
)

// SeasonStats é o mês corrente da pessoa.
type SeasonStats struct {
	Key       string // YYYY-MM
	Platinums int
	Rare      int
}

// SeasonDone indica se a temporada do mês foi fechada.
func SeasonDone(s SeasonStats) bool {
	return s.Platinums >= SeasonPlatinumGoal || s.Rare >= SeasonRareGoal
}

var months = [...]string{"jan", "fev", "mar", "abr", "mai", "jun", "jul", "ago", "set", "out", "nov", "dez"}

// SeasonKey é o mês (Brasília) de t no formato YYYY-MM.
func SeasonKey(t time.Time) string {
	return t.In(saoPaulo()).Format("2006-01")
}

// SeasonLabel devolve o nome da temporada, ex.: "Temporada out/2026".
func SeasonLabel(key string) string {
	t, err := time.Parse("2006-01", key)
	if err != nil {
		return "Temporada " + key
	}
	return fmt.Sprintf("Temporada %s/%d", months[t.Month()-1], t.Year())
}

// SeasonBounds devolve [início, fim) do mês da chave em Brasília.
func SeasonBounds(key string) (time.Time, time.Time, error) {
	t, err := time.ParseInLocation("2006-01", key, saoPaulo())
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return t, t.AddDate(0, 1, 0), nil
}

func saoPaulo() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.FixedZone("BRT", -3*3600)
	}
	return loc
}

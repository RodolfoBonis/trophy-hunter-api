// Package recap tem as regras puras da retrospectiva: período (mês/ano no fuso de Brasília), maior
// sequência de dias com desbloqueio, dia da semana favorito e a semana do resumo da IA.
package recap

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Location é o fuso das datas da retrospectiva (o dono e os amigos estão no Brasil).
var Location = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.FixedZone("BRT", -3*3600)
	}
	return loc
}()

// Period é um intervalo [Since, Until) e o anterior de mesmo tamanho (para comparar).
type Period struct {
	Key       string // "2026-10" ou "2026"
	Label     string // "outubro de 2026" / "2026"
	Since     time.Time
	Until     time.Time
	PrevSince time.Time
	PrevUntil time.Time
	Partial   bool // o período ainda não acabou
}

var months = []string{"janeiro", "fevereiro", "março", "abril", "maio", "junho", "julho", "agosto", "setembro", "outubro", "novembro", "dezembro"}

// ParsePeriod entende "YYYY-MM" e "YYYY" (vazio = mês corrente).
func ParsePeriod(key string, now time.Time) (Period, error) {
	now = now.In(Location)
	if key == "" {
		key = now.Format("2006-01")
	}
	if y, m, ok := strings.Cut(key, "-"); ok {
		year, err1 := strconv.Atoi(y)
		month, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil || month < 1 || month > 12 || year < 2000 || year > 2100 {
			return Period{}, fmt.Errorf("período inválido: use AAAA-MM ou AAAA")
		}
		since := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, Location)
		until := since.AddDate(0, 1, 0)
		return Period{
			Key: key, Label: fmt.Sprintf("%s de %d", months[month-1], year), Since: since, Until: until,
			PrevSince: since.AddDate(0, -1, 0), PrevUntil: since, Partial: now.Before(until),
		}, nil
	}
	year, err := strconv.Atoi(key)
	if err != nil || year < 2000 || year > 2100 {
		return Period{}, fmt.Errorf("período inválido: use AAAA-MM ou AAAA")
	}
	since := time.Date(year, 1, 1, 0, 0, 0, 0, Location)
	until := since.AddDate(1, 0, 0)
	return Period{Key: key, Label: strconv.Itoa(year), Since: since, Until: until, PrevSince: since.AddDate(-1, 0, 0), PrevUntil: since, Partial: now.Before(until)}, nil
}

// LongestStreak devolve a maior sequência de dias consecutivos (datas já ordenadas, sem repetição).
func LongestStreak(days []time.Time) int {
	best, cur := 0, 0
	var prev time.Time
	for i, d := range days {
		if i > 0 && d.Sub(prev) == 24*time.Hour {
			cur++
		} else {
			cur = 1
		}
		if cur > best {
			best = cur
		}
		prev = d
	}
	return best
}

var weekdays = []string{"domingo", "segunda", "terça", "quarta", "quinta", "sexta", "sábado"}

// FavoriteWeekday é o dia da semana com mais desbloqueios ("" sem dados).
func FavoriteWeekday(days []time.Time, counts []int) string {
	var by [7]int
	for i, d := range days {
		by[d.Weekday()] += counts[i]
	}
	best, top := -1, 0
	for i, n := range by {
		if n > top {
			best, top = i, n
		}
	}
	if best < 0 {
		return ""
	}
	return weekdays[best]
}

// WeekStart é a segunda-feira (00:00, Brasília) da semana de t — chave do resumo semanal.
func WeekStart(t time.Time) time.Time {
	t = t.In(Location)
	offset := (int(t.Weekday()) + 6) % 7 // segunda = 0
	d := t.AddDate(0, 0, -offset)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, Location)
}

// WeeklyDue indica se já é hora do resumo da semana (domingo a partir das 18h, Brasília).
func WeeklyDue(now time.Time) bool {
	n := now.In(Location)
	return n.Weekday() == time.Sunday && n.Hour() >= 18
}

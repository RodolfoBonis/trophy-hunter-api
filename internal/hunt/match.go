// Package hunt liga o roadmap da IA (etapas com troféus citados pelo NOME) aos troféus reais do jogo,
// para o modo caçada marcar sozinho o que já foi desbloqueado. Puro: sem I/O.
package hunt

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Trophy é o mínimo de um troféu real para casar com o roadmap.
type Trophy struct {
	ID   string
	Name string
}

// StageMatch é o resultado de uma etapa: IDs dos troféus reais e os nomes que não casaram.
type StageMatch struct {
	TrophyIDs []string
	Unmatched []string
}

var (
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	// Sufixos que a IA às vezes cola no nome: "Lorde Éldero (platina)", "X — perdível".
	noteSuffix = regexp.MustCompile(`\s*(\(.*\)|\[.*\]|\s[-—–:]\s.*)$`)
)

// Normalize deixa em minúsculas, sem acento, só letras/dígitos separados por espaço.
func Normalize(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	folded, _, err := transform.String(t, s)
	if err != nil {
		folded = s
	}
	return strings.TrimSpace(nonAlnum.ReplaceAllString(strings.ToLower(folded), " "))
}

// Match casa os nomes citados em cada etapa com os troféus do jogo. Regras, nesta ordem:
//  1. nome igual (normalizado);
//  2. igual depois de tirar anotações no fim ("(platina)", "— perdível");
//  3. um único troféu cujo nome contém o citado (ou vice-versa, com ≥ 4 letras).
//
// Ambíguo ou sem par vira Unmatched (a UI mostra como texto, sem marcar sozinho).
func Match(stages [][]string, trophies []Trophy) []StageMatch {
	byName := map[string][]string{}
	norms := make([]string, len(trophies))
	for i, t := range trophies {
		n := Normalize(t.Name)
		norms[i] = n
		byName[n] = append(byName[n], t.ID)
	}
	out := make([]StageMatch, len(stages))
	for si, names := range stages {
		m := StageMatch{TrophyIDs: []string{}, Unmatched: []string{}}
		seen := map[string]bool{}
		for _, raw := range names {
			id, ok := find(raw, trophies, norms, byName)
			if !ok {
				m.Unmatched = append(m.Unmatched, raw)
				continue
			}
			if !seen[id] {
				seen[id] = true
				m.TrophyIDs = append(m.TrophyIDs, id)
			}
		}
		out[si] = m
	}
	return out
}

func find(raw string, trophies []Trophy, norms []string, byName map[string][]string) (string, bool) {
	for _, cand := range []string{Normalize(raw), Normalize(noteSuffix.ReplaceAllString(raw, ""))} {
		if cand == "" {
			continue
		}
		if ids := byName[cand]; len(ids) == 1 {
			return ids[0], true
		}
	}
	n := Normalize(noteSuffix.ReplaceAllString(raw, ""))
	if len(n) < 4 {
		return "", false
	}
	hit := -1
	for i, tn := range norms {
		contains := strings.Contains(tn, n) || (len(tn) >= 4 && strings.Contains(n, tn))
		if tn == "" || !contains {
			continue
		}
		if hit >= 0 {
			return "", false // ambíguo
		}
		hit = i
	}
	if hit < 0 {
		return "", false
	}
	return trophies[hit].ID, true
}

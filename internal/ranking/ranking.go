// Package ranking calcula a fila "Próxima Platina": quais jogos estão mais perto de virar platina
// (PSN) ou 100% (Steam). É puro (sem I/O) para ser testado isoladamente.
//
// A nota (0..~1.2) pondera:
//   - concluído (45%): fração já obtida;
//   - restantes (25%): poucos troféus faltando valem mais — 1/(1+restantes/8);
//   - facilidade (30%): raridade média do que falta (maior % de jogadores = mais fácil);
//   - bônus de prioridade manual (+0.05 por nível, até 3);
//   - urgência online (+0.05) quando falta troféu online/com risco de servidor fechar;
//   - bônus de tempo (até +0.10) quando há estimativa (HowLongToBeat) de poucas horas restantes —
//     0.10/(1+horas/10); sem estimativa, 0 (não pune quem não tem dado).
//
// Os fatores vão na resposta para a UI explicar a posição de cada jogo.
package ranking

import (
	"math"
	"sort"
)

// Pesos da nota base (somam 1).
const (
	WeightCompletion = 0.45
	WeightRemaining  = 0.25
	WeightEase       = 0.30

	priorityStep   = 0.05
	maxPriority    = 3
	onlineUrgency  = 0.05
	remainingScale = 8.0
	timeBonus      = 0.10
	timeScaleHours = 10.0
)

// Candidate é um jogo começado e não concluído (conta mais avançada).
type Candidate struct {
	ID string
	// Completion é a fração obtida (0..1).
	Completion float64
	Remaining  int
	// AvgRemainingRarity é a % média de jogadores que têm os troféus que faltam (0..100).
	AvgRemainingRarity float64
	// Priority é a prioridade manual (0..3; valores maiores são limitados a 3).
	Priority          int
	RemainingMissable int
	RemainingOnline   int
	// HoursLeft é a estimativa de horas que faltam (0 = sem estimativa).
	HoursLeft float64
}

// Factors detalha a nota (cada fator já ponderado, exceto os bônus, que são somados).
type Factors struct {
	Completion float64 `json:"completion"`
	Remaining  float64 `json:"remaining"`
	Ease       float64 `json:"ease"`
	Priority   float64 `json:"priority"`
	Urgency    float64 `json:"urgency"`
	Time       float64 `json:"time"`
}

// Ranked é um candidato com a nota e os fatores.
type Ranked struct {
	Candidate
	Score   float64
	Factors Factors
}

// Score calcula a nota e os fatores de um candidato.
func Score(c Candidate) (float64, Factors) {
	completion := clamp01(c.Completion)
	remaining := 1 / (1 + math.Max(0, float64(c.Remaining))/remainingScale)
	ease := clamp01(c.AvgRemainingRarity / 100)
	prio := c.Priority
	if prio < 0 {
		prio = 0
	}
	if prio > maxPriority {
		prio = maxPriority
	}
	f := Factors{
		Completion: WeightCompletion * completion,
		Remaining:  WeightRemaining * remaining,
		Ease:       WeightEase * ease,
		Priority:   priorityStep * float64(prio),
	}
	if c.RemainingOnline > 0 {
		f.Urgency = onlineUrgency
	}
	if c.HoursLeft > 0 {
		f.Time = timeBonus / (1 + c.HoursLeft/timeScaleHours)
	}
	return f.Completion + f.Remaining + f.Ease + f.Priority + f.Urgency + f.Time, f
}

// Rank ordena os candidatos pela nota (maior primeiro); empate → menos restantes → id.
func Rank(cands []Candidate) []Ranked {
	out := make([]Ranked, 0, len(cands))
	for _, c := range cands {
		s, f := Score(c)
		out = append(out, Ranked{Candidate: c, Score: s, Factors: f})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Remaining != out[j].Remaining {
			return out[i].Remaining < out[j].Remaining
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

package ranking_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/ranking"
)

func TestScoreFactors(t *testing.T) {
	s, f := ranking.Score(ranking.Candidate{Completion: 0.9, Remaining: 2, AvgRemainingRarity: 40, Priority: 1})
	assert.InDelta(t, 0.45*0.9, f.Completion, 1e-9)
	assert.InDelta(t, 0.25*(1/(1+2.0/8)), f.Remaining, 1e-9)
	assert.InDelta(t, 0.30*0.4, f.Ease, 1e-9)
	assert.InDelta(t, 0.05, f.Priority, 1e-9)
	assert.Zero(t, f.Urgency)
	assert.InDelta(t, f.Completion+f.Remaining+f.Ease+f.Priority, s, 1e-9)
}

func TestScoreClampsAndBonuses(t *testing.T) {
	tests := []struct {
		name string
		c    ranking.Candidate
		want func(t *testing.T, s float64, f ranking.Factors)
	}{
		{"prioridade limitada a 3", ranking.Candidate{Priority: 9}, func(t *testing.T, _ float64, f ranking.Factors) {
			assert.InDelta(t, 0.15, f.Priority, 1e-9)
		}},
		{"prioridade negativa vira 0", ranking.Candidate{Priority: -2}, func(t *testing.T, _ float64, f ranking.Factors) {
			assert.Zero(t, f.Priority)
		}},
		{"raridade > 100 limitada", ranking.Candidate{AvgRemainingRarity: 250}, func(t *testing.T, _ float64, f ranking.Factors) {
			assert.InDelta(t, 0.30, f.Ease, 1e-9)
		}},
		{"completion NaN vira 0", ranking.Candidate{Completion: math.NaN()}, func(t *testing.T, _ float64, f ranking.Factors) {
			assert.Zero(t, f.Completion)
		}},
		{"urgência online", ranking.Candidate{RemainingOnline: 2}, func(t *testing.T, _ float64, f ranking.Factors) {
			assert.InDelta(t, 0.05, f.Urgency, 1e-9)
		}},
		{"perdível não muda a nota", ranking.Candidate{RemainingMissable: 5}, func(t *testing.T, s float64, _ ranking.Factors) {
			base, _ := ranking.Score(ranking.Candidate{})
			assert.InDelta(t, base, s, 1e-9)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, f := ranking.Score(tc.c)
			tc.want(t, s, f)
		})
	}
}

func TestRankOrder(t *testing.T) {
	got := ranking.Rank([]ranking.Candidate{
		{ID: "longe", Completion: 0.2, Remaining: 40, AvgRemainingRarity: 10},
		{ID: "quase-facil", Completion: 0.95, Remaining: 1, AvgRemainingRarity: 60},
		{ID: "quase-dificil", Completion: 0.95, Remaining: 1, AvgRemainingRarity: 0.5},
		{ID: "meio", Completion: 0.6, Remaining: 10, AvgRemainingRarity: 30},
	})
	require.Len(t, got, 4)
	ids := []string{got[0].ID, got[1].ID, got[2].ID, got[3].ID}
	assert.Equal(t, []string{"quase-facil", "quase-dificil", "meio", "longe"}, ids)
	for i := 1; i < len(got); i++ {
		assert.GreaterOrEqual(t, got[i-1].Score, got[i].Score)
	}
}

func TestRankTieBreaksByRemaining(t *testing.T) {
	// Mesma nota: menos restantes primeiro, depois id (estável).
	a := ranking.Candidate{ID: "b", Completion: 0.5, Remaining: 4, AvgRemainingRarity: 20}
	b := ranking.Candidate{ID: "a", Completion: 0.5, Remaining: 4, AvgRemainingRarity: 20}
	got := ranking.Rank([]ranking.Candidate{a, b})
	assert.Equal(t, "a", got[0].ID)
	assert.Empty(t, ranking.Rank(nil))
}

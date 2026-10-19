package badges

import (
	"testing"
	"time"
)

func find(ps []Progress, key string) Progress {
	for _, p := range ps {
		if p.Key == key {
			return p
		}
	}
	return Progress{}
}

func TestEvaluate(t *testing.T) {
	ps := Evaluate(Stats{Earned: 1200, Platinums: 12, Rare: 9, UltraRare: 1, SteamComplete: 2, PSNPlatinum: 0, LongestStreak: 8})
	cases := map[string]struct {
		earned bool
		value  int
	}{
		"platinum_1":     {true, 1},
		"platinum_10":    {true, 10},
		"platinum_25":    {false, 12},
		"trophies_1000":  {true, 1000},
		"rare_10":        {false, 9},
		"ultra_1":        {true, 1},
		"streak_7":       {true, 7},
		"both_platforms": {false, 0},
	}
	for key, want := range cases {
		got := find(ps, key)
		if got.Earned != want.earned || got.Value != want.value {
			t.Errorf("%s: earned=%v value=%d, want %v %d", key, got.Earned, got.Value, want.earned, want.value)
		}
	}
	if len(ps) != len(Catalog) {
		t.Fatalf("len = %d", len(ps))
	}
}

func TestNextClosestFirst(t *testing.T) {
	next := Next(Evaluate(Stats{Platinums: 9, Rare: 1}), 2)
	if len(next) != 2 || next[0].Key != "platinum_10" {
		t.Fatalf("next = %+v", next)
	}
}

func TestCatalogKeysUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Catalog {
		if seen[d.Key] || d.Goal <= 0 || d.Name == "" {
			t.Fatalf("medalha inválida/duplicada: %+v", d)
		}
		seen[d.Key] = true
	}
}

func TestSeason(t *testing.T) {
	// 01/11 01:00 UTC ainda é 31/10 em Brasília.
	if k := SeasonKey(time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)); k != "2026-10" {
		t.Fatalf("key = %s", k)
	}
	if l := SeasonLabel("2026-10"); l != "Temporada out/2026" {
		t.Fatalf("label = %s", l)
	}
	from, to, err := SeasonBounds("2026-12")
	if err != nil || from.Month() != 12 || to.Year() != 2027 || to.Month() != 1 {
		t.Fatalf("bounds = %v %v %v", from, to, err)
	}
	if SeasonDone(SeasonStats{Platinums: 2, Rare: 9}) || !SeasonDone(SeasonStats{Rare: 10}) || !SeasonDone(SeasonStats{Platinums: 3}) {
		t.Fatal("regra da temporada")
	}
}

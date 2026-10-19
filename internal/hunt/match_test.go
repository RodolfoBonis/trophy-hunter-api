package hunt

import (
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Lorde Éldero":          "lorde eldero",
		"  Diário do Capitão!!": "diario do capitao",
		"S.T.A.R.S.":            "s t a r s",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatch(t *testing.T) {
	trophies := []Trophy{
		{"t1", "Lorde Éldero"},
		{"t2", "Diário do Capitão"},
		{"t3", "Caçador de Dragões"},
		{"t4", "Dragão Ancestral"},
		{"t5", "Dragão Ancestral"}, // duplicado (DLC com mesmo nome): ambíguo
		{"t6", "Mestre das Armas Lendárias"},
	}
	got := Match([][]string{
		{"lorde eldero", "Diário do capitão (perdível)", "Dragão Ancestral"},
		{"Caçador de Dragões — capítulo 3", "Armas Lendárias", "Troféu que não existe", "Lorde Éldero"},
		{},
	}, trophies)
	want := []StageMatch{
		{TrophyIDs: []string{"t1", "t2"}, Unmatched: []string{"Dragão Ancestral"}},
		{TrophyIDs: []string{"t3", "t6", "t1"}, Unmatched: []string{"Troféu que não existe"}},
		{TrophyIDs: []string{}, Unmatched: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Match =\n%+v\nwant\n%+v", got, want)
	}
}

func TestMatchShortNamesDontFuzzy(t *testing.T) {
	got := Match([][]string{{"Ás"}}, []Trophy{{"t1", "Ás do Volante"}, {"t2", "Ás"}})
	if !reflect.DeepEqual(got[0].TrophyIDs, []string{"t2"}) {
		t.Fatalf("exato vence: %+v", got)
	}
	got = Match([][]string{{"Ás"}}, []Trophy{{"t1", "Ás do Volante"}})
	if len(got[0].TrophyIDs) != 0 {
		t.Fatalf("nome curto não casa por trecho: %+v", got)
	}
}

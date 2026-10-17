// Package progress calcula o progresso de um jogo a partir dos seus troféus. É PURO (sem I/O):
// recebe os troféus e devolve um resumo, para ser 100% testável e reutilizável (a fase 3 estende
// ranking/raridade sobre os mesmos dados).
package progress

// Tipos de troféu.
const (
	TypePlatinum    = "platinum"
	TypeGold        = "gold"
	TypeSilver      = "silver"
	TypeBronze      = "bronze"
	TypeAchievement = "achievement"
)

// Status buckets de um jogo.
const (
	StatusPlatinum   = "platinum"
	StatusCompleted  = "completed"
	StatusAlmost     = "almost"
	StatusInProgress = "in_progress"
	StatusNotStarted = "not_started"
	StatusAbandoned  = "abandoned"
)

// DefaultAlmostThreshold é o limiar padrão do bucket "quase lá" (80%).
const DefaultAlmostThreshold = 0.8

// Trophy é a visão mínima de um troféu necessária ao cálculo.
type Trophy struct {
	Type   string
	Earned bool
	IsBase bool // pertence ao grupo base (vs DLC)
}

// Config parametriza o cálculo.
type Config struct {
	// AlmostThreshold é a fração (0..1) a partir da qual o jogo entra em "almost". <=0 usa o default.
	AlmostThreshold float64
}

func (c Config) threshold() float64 {
	if c.AlmostThreshold <= 0 {
		return DefaultAlmostThreshold
	}
	return c.AlmostThreshold
}

// TypeCount é a contagem total/obtida de um tipo de troféu.
type TypeCount struct {
	Total  int `json:"total"`
	Earned int `json:"earned"`
}

// Bucket agrega contagens de um conjunto (base ou DLC).
type Bucket struct {
	Total     int `json:"total"`
	Earned    int `json:"earned"`
	Remaining int `json:"remaining"`
}

// Summary é o resultado do cálculo de progresso.
type Summary struct {
	Total          int                  `json:"total"`
	Earned         int                  `json:"earned"`
	Remaining      int                  `json:"remaining"`
	CompletionPct  float64              `json:"completion_pct"` // 0..1 por contagem
	Status         string               `json:"status"`
	HasPlatinum    bool                 `json:"has_platinum"`
	PlatinumEarned bool                 `json:"platinum_earned"`
	ByType         map[string]TypeCount `json:"by_type"`
	Base           Bucket               `json:"base"`
	DLC            Bucket               `json:"dlc"`
}

// Compute calcula o resumo de progresso. abandoned marca o jogo como abandonado (status manual).
func Compute(trophies []Trophy, abandoned bool, cfg Config) Summary {
	s := Summary{ByType: map[string]TypeCount{}}

	for _, t := range trophies {
		s.Total++
		tc := s.ByType[t.Type]
		tc.Total++
		if t.Earned {
			s.Earned++
			tc.Earned++
		}
		s.ByType[t.Type] = tc

		if t.IsBase {
			s.Base.Total++
			if t.Earned {
				s.Base.Earned++
			}
		} else {
			s.DLC.Total++
			if t.Earned {
				s.DLC.Earned++
			}
		}

		if t.Type == TypePlatinum {
			s.HasPlatinum = true
			if t.Earned {
				s.PlatinumEarned = true
			}
		}
	}

	s.Remaining = s.Total - s.Earned
	s.Base.Remaining = s.Base.Total - s.Base.Earned
	s.DLC.Remaining = s.DLC.Total - s.DLC.Earned
	if s.Total > 0 {
		s.CompletionPct = float64(s.Earned) / float64(s.Total)
	}
	s.Status = bucket(s, abandoned, cfg.threshold())
	return s
}

func bucket(s Summary, abandoned bool, threshold float64) string {
	switch {
	case abandoned:
		return StatusAbandoned
	case s.Total > 0 && s.Earned == s.Total:
		if s.PlatinumEarned {
			return StatusPlatinum
		}
		return StatusCompleted
	case s.Total > 0 && s.CompletionPct >= threshold:
		return StatusAlmost
	case s.Earned > 0:
		return StatusInProgress
	default:
		return StatusNotStarted
	}
}

package preset

// Preset je recept na jeden vzhled. Struktura odpovídá data/presets/_schema.json —
// když se mění jedno, musí se změnit i druhé (schema se validuje při načtení).
type Preset struct {
	ID            string            `json:"id"`
	Version       int               `json:"version"`
	Category      string            `json:"category"`
	Label         map[string]string `json:"label,omitempty"`
	Checkpoint    string            `json:"checkpoint"`
	Profile       string            `json:"profile"`
	PromptDialect string            `json:"prompt_dialect"`
	LoRAs         []LoRA            `json:"loras,omitempty"`

	PromptTemplate   string `json:"prompt_template"`
	NegativeTemplate string `json:"negative_template,omitempty"`

	Identity   Identity     `json:"identity"`
	ControlNet []ControlNet `json:"controlnet,omitempty"`
	Sampler    Sampler      `json:"sampler"`
	Resolution *Resolution  `json:"resolution,omitempty"`

	SupportsAttrs    []string `json:"supports_attrs,omitempty"`
	NSFWNegativeLock bool     `json:"nsfw_negative_lock"`
	FaceRestore      float64  `json:"face_restore"`
	LicenseTier      string   `json:"license_tier"`
	Thumbnail        string   `json:"thumbnail,omitempty"`

	// effectiveTier je dopočítaný tier po křížové kontrole s licenčním
	// registrem. Není v JSONu presetu — plní ho loader.
	effectiveTier Tier
}

// EffectiveTier vrací tier zjištěný loaderem (ne to, co si preset deklaruje).
func (p *Preset) EffectiveTier() Tier { return p.effectiveTier }

type LoRA struct {
	Name     string  `json:"name"`
	Weight   float64 `json:"weight"`
	Optional bool    `json:"optional,omitempty"`
}

type Identity struct {
	Method string  `json:"method"`
	Weight float64 `json:"weight"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
}

type ControlNet struct {
	Type   string  `json:"type"`
	Weight float64 `json:"weight"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
}

type Sampler struct {
	Name      string  `json:"name"`
	Scheduler string  `json:"scheduler"`
	Steps     int     `json:"steps"`
	CFG       float64 `json:"cfg"`
}

type Resolution struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Res vrátí rozlišení presetu, nebo výchozích 1024×1024.
func (p *Preset) Res() Resolution {
	if p.Resolution != nil && p.Resolution.Width > 0 && p.Resolution.Height > 0 {
		return *p.Resolution
	}
	return Resolution{Width: 1024, Height: 1024}
}

// Supports říká, jestli preset umí daný atribut. UI podle toho slidery
// skrývá — viditelný slider, který nic nedělá, je horší než žádný (§9.2).
func (p *Preset) Supports(attr string) bool {
	for _, a := range p.SupportsAttrs {
		if a == attr {
			return true
		}
	}
	return false
}

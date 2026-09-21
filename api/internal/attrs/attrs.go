// Package attrs načítá mapovací tabulku atributů (data/attributes.yaml)
// a překládá hodnoty atributů na prompt fragmenty.
//
// Ve Fázi 0 se používá jen prompt vrstva. Vrstvy geometry a mask jsou
// v tabulce popsané, ale jejich vykonavatel (TPS warp, inpaint) přijde
// ve Fázi 2 — do té doby je API jen validuje a hlásí jako nepodporované.
package attrs

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Layer určuje, jak se atribut technicky vynutí (§2 plánu).
type Layer string

const (
	LayerGeometry Layer = "geometry" // TPS warp vstupní fotky před generací
	LayerMask     Layer = "mask"     // inpaint segmentované oblasti po base renderu
	LayerPrompt   Layer = "prompt"   // textový fragment při base renderu
)

type Kind string

const (
	KindContinuous Kind = "continuous"
	KindDiscrete   Kind = "discrete"
)

type Attribute struct {
	Layer   Layer             `yaml:"layer"`
	Type    Kind              `yaml:"type"`
	Range   []float64         `yaml:"range"`
	Default any               `yaml:"default"`
	Options []string          `yaml:"options"`
	Label   map[string]string `yaml:"label"`
	Group   string            `yaml:"group"`

	Geometry *GeometrySpec `yaml:"geometry"`
	Mask     *MaskSpec     `yaml:"mask"`
	Inpaint  *InpaintSpec  `yaml:"inpaint"`

	// Prompt fragmenty podle dialektu: prompt[dialekt][hodnota].
	// U spojitých atributů jsou klíče "-1" a "1", u diskrétních názvy voleb.
	Prompt map[string]map[string]string `yaml:"prompt"`
}

type GeometrySpec struct {
	Method    string  `yaml:"method"`
	Landmarks []int   `yaml:"landmarks"`
	Scale     float64 `yaml:"scale"`
}

type MaskSpec struct {
	Source       string   `yaml:"source"`
	Regions      []string `yaml:"regions"`
	Subtract     []string `yaml:"subtract"`
	PromptPoints string   `yaml:"prompt_points"`
}

type InpaintSpec struct {
	Denoise                float64 `yaml:"denoise"`
	IdentityWeightOverride float64 `yaml:"identity_weight_override"`
	Feather                int     `yaml:"feather"`
	Expand                 int     `yaml:"expand"`
}

type Group struct {
	ID    string            `yaml:"id"`
	Label map[string]string `yaml:"label"`
}

type IdentityGuard struct {
	WarnThreshold float64 `yaml:"warn_threshold"`
	MaxThreshold  float64 `yaml:"max_threshold"`
}

// Table je celá atributová tabulka.
type Table struct {
	Version       int                   `yaml:"version"`
	Attributes    map[string]*Attribute `yaml:"attributes"`
	Groups        []Group               `yaml:"groups"`
	IdentityGuard IdentityGuard         `yaml:"identity_guard"`
}

// Load načte a zvaliduje attributes.yaml.
func Load(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("attributes.yaml: %w", err)
	}
	var t Table
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("attributes.yaml parse: %w", err)
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// validate chytá chyby, které by se jinak projevily až při renderu jako
// nesmyslný obrázek — tedy nejdráž.
func (t *Table) validate() error {
	known := map[string]bool{}
	for _, g := range t.Groups {
		known[g.ID] = true
	}
	for name, a := range t.Attributes {
		switch a.Layer {
		case LayerGeometry, LayerMask, LayerPrompt:
		default:
			return fmt.Errorf("atribut %q: neznámá vrstva %q", name, a.Layer)
		}
		switch a.Type {
		case KindContinuous:
			if len(a.Range) != 2 || a.Range[0] >= a.Range[1] {
				return fmt.Errorf("atribut %q: spojitý atribut potřebuje range [min, max]", name)
			}
		case KindDiscrete:
			if len(a.Options) == 0 {
				return fmt.Errorf("atribut %q: diskrétní atribut potřebuje options", name)
			}
		default:
			return fmt.Errorf("atribut %q: neznámý type %q", name, a.Type)
		}
		if a.Group != "" && !known[a.Group] {
			return fmt.Errorf("atribut %q: group %q není v seznamu groups", name, a.Group)
		}
		if a.Layer == LayerGeometry && a.Geometry == nil {
			return fmt.Errorf("atribut %q: vrstva geometry bez geometry bloku", name)
		}
		if a.Layer == LayerMask && a.Mask == nil {
			return fmt.Errorf("atribut %q: vrstva mask bez mask bloku", name)
		}
	}
	return nil
}

// Get vrátí atribut podle jména.
func (t *Table) Get(name string) (*Attribute, bool) {
	a, ok := t.Attributes[name]
	return a, ok
}

// Vector je sada hodnot atributů zvolená uživatelem.
// Spojité drží float64, diskrétní string.
type Vector map[string]any

// Validate zkontroluje, že vektor odpovídá tabulce i tomu, co preset umí.
// supported == nil znamená "neomezuj presetem".
func (t *Table) Validate(v Vector, supported map[string]bool) error {
	for name, val := range v {
		a, ok := t.Attributes[name]
		if !ok {
			return fmt.Errorf("neznámý atribut %q", name)
		}
		if supported != nil && !supported[name] {
			return fmt.Errorf("preset nepodporuje atribut %q", name)
		}
		switch a.Type {
		case KindContinuous:
			f, err := toFloat(val)
			if err != nil {
				return fmt.Errorf("atribut %q: očekáváno číslo, přišlo %T", name, val)
			}
			if f < a.Range[0] || f > a.Range[1] {
				return fmt.Errorf("atribut %q: hodnota %v mimo rozsah [%v, %v]", name, f, a.Range[0], a.Range[1])
			}
		case KindDiscrete:
			s, ok := val.(string)
			if !ok {
				return fmt.Errorf("atribut %q: očekáván řetězec, přišlo %T", name, val)
			}
			if !contains(a.Options, s) {
				return fmt.Errorf("atribut %q: %q není mezi %v", name, s, a.Options)
			}
		}
	}
	return nil
}

// GuardScore sečte absolutní odchylky od výchozích hodnot. Slouží k varování,
// že uživatel odtlačil vzhled tak daleko, že se ztrácí podobnost (§12).
func (t *Table) GuardScore(v Vector) float64 {
	var sum float64
	for name, val := range v {
		a, ok := t.Attributes[name]
		if !ok {
			continue
		}
		switch a.Type {
		case KindContinuous:
			f, err := toFloat(val)
			if err != nil {
				continue
			}
			d, _ := toFloat(a.Default)
			sum += math.Abs(f - d)
		case KindDiscrete:
			// Diskrétní změna se počítá jako jeden celý krok — buď je
			// nastavená jinak než default, nebo ne.
			if s, ok := val.(string); ok {
				if d, ok := a.Default.(string); !ok || s != d {
					sum += 1.0
				}
			}
		}
	}
	return sum
}

// PromptFragments přeloží vektor na textové fragmenty pro daný dialekt.
//
// Vrací fragmenty jen pro atributy, které v tomhle dialektu něco mají.
// Vrstvy geometry a mask sem přispívají taky — prompt je podpůrný signál,
// hlavní vynucení dělá warp resp. maska. Ve Fázi 0, kde warp ani masky
// neběží, je prompt jediný mechanismus, takže je to zároveň fallback.
func (t *Table) PromptFragments(v Vector, dialect string) []string {
	names := make([]string, 0, len(v))
	for n := range v {
		names = append(names, n)
	}
	// Stabilní pořadí — jinak by stejný recept dával jiný prompt
	// a tím pádem jiný obrázek při stejném seedu.
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		a, ok := t.Attributes[name]
		if !ok || a.Prompt == nil {
			continue
		}
		byVal, ok := a.Prompt[dialect]
		if !ok {
			continue
		}
		frag := fragmentFor(a, byVal, v[name])
		if frag != "" {
			out = append(out, frag)
		}
	}
	return out
}

// fragmentFor vybere fragment pro jednu hodnotu.
func fragmentFor(a *Attribute, byVal map[string]string, val any) string {
	switch a.Type {
	case KindDiscrete:
		s, ok := val.(string)
		if !ok {
			return ""
		}
		// Hodnota shodná s defaultem se do promptu nepíše — jinak by se
		// prompt zaplevelil frázemi typu "neutral expression, brown eyes"
		// u obrázku, kde uživatel nic nezměnil.
		if d, ok := a.Default.(string); ok && s == d {
			return ""
		}
		return byVal[s]

	case KindContinuous:
		f, err := toFloat(val)
		if err != nil {
			return ""
		}
		d, _ := toFloat(a.Default)
		delta := f - d
		// Malé odchylky nemá smysl verbalizovat — prompt je hrubý nástroj,
		// jemné doladění je práce pro geometry vrstvu.
		if math.Abs(delta) < 0.25 {
			return ""
		}
		key := "1"
		if delta < 0 {
			key = "-1"
		}
		frag := byVal[key]
		if frag == "" {
			return ""
		}
		// Silná odchylka dostane zesílení. Držíme se jednoduchého
		// "very", ne SDXL závorkové syntaxe — ta se mezi checkpointy
		// chová nekonzistentně.
		if math.Abs(delta) > 0.7 {
			return "very " + frag
		}
		return frag
	}
	return ""
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("nelze převést %T na číslo", v)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Join spojí fragmenty do jednoho promptového bloku.
func Join(frags []string) string {
	return strings.Join(frags, ", ")
}

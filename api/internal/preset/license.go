package preset

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier vyjadřuje, jak moc si jsme jistí licencí modelu.
type Tier string

const (
	TierSafe    Tier = "safe"    // ověřeno, komerční hosting povolen
	TierVerify  Tier = "verify"  // nedořešeno — dev ano, produkce ne
	TierBlocked Tier = "blocked" // ověřeno jako nepoužitelné
)

// rank převádí tier na číslo, aby šlo porovnat, který je přísnější.
func (t Tier) rank() int {
	switch t {
	case TierSafe:
		return 0
	case TierVerify:
		return 1
	default:
		// Neznámý tier je záměrně nejpřísnější — překlep v datech
		// nesmí vést k tomu, že se něco povolí.
		return 2
	}
}

// stricter vrátí přísnější ze dvou tierů.
func stricter(a, b Tier) Tier {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// ModelEntry je jedna položka licenčního registru.
type ModelEntry struct {
	File    string `yaml:"file"`
	Tier    Tier   `yaml:"tier"`
	License string `yaml:"license"`
	Note    string `yaml:"note"`
	Source  string `yaml:"source"`
}

// LicenseRegistry je obsah data/licenses.yaml — jediný zdroj pravdy o tom,
// který model smí Avatar použít.
//
// Existuje proto, že na DGX Sparku leží checkpointy, které jsou pro tenhle
// produkt licenčně zakázané (Juggernaut, Pony), ale používají se tam pro jiné
// projekty. Spoléhat na to, že je nikdo omylem nenapíše do presetu, nestačí.
type LicenseRegistry struct {
	Version     int                   `yaml:"version"`
	VerifiedAt  string                `yaml:"verified_at"`
	Strict      bool                  `yaml:"strict"`
	Checkpoints map[string]ModelEntry `yaml:"checkpoints"`
	LoRAs       map[string]ModelEntry `yaml:"loras"`
	ControlNets map[string]ModelEntry `yaml:"controlnets"`
	IPAdapters  map[string]ModelEntry `yaml:"ipadapters"`
	CLIPVision  map[string]ModelEntry `yaml:"clip_vision"`
	Auxiliary   map[string]ModelEntry `yaml:"auxiliary"`
}

// LoadLicenseRegistry načte a zkontroluje licenses.yaml.
func LoadLicenseRegistry(path string) (*LicenseRegistry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("licenses.yaml: %w", err)
	}
	var reg LicenseRegistry
	if err := yaml.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("licenses.yaml parse: %w", err)
	}
	if len(reg.Checkpoints) == 0 {
		return nil, fmt.Errorf("licenses.yaml: prázdný seznam checkpointů — to je skoro jistě chyba v souboru")
	}
	return &reg, nil
}

// UserLoRAPlaceholder je značka, kterou preset použije místo konkrétní cesty
// k user LoRA. Resolver ji nahradí až podle face_v2; dokud LoRA neexistuje,
// položka z receptu vypadne a jede se instant trackem (§4.2 plánu).
const UserLoRAPlaceholder = "$user_lora"

// ModelRef je jeden vyřešený odkaz na model — z klíče na soubor.
type ModelRef struct {
	Key  string
	File string
	Tier Tier
}

// lookup najde položku v dané mapě a přeloží chybějící klíč na srozumitelnou chybu.
func lookup(kind string, m map[string]ModelEntry, key string) (ModelEntry, error) {
	e, ok := m[key]
	if !ok {
		return ModelEntry{}, fmt.Errorf("%s %q není v licenčním registru — přidej ho do data/licenses.yaml i s ověřenou licencí", kind, key)
	}
	if e.Tier == TierBlocked {
		return ModelEntry{}, fmt.Errorf("%s %q má tier blocked (%s): %s", kind, key, e.License, strings.TrimSpace(e.Note))
	}
	return e, nil
}

// ResolveCheckpoint přeloží klíč presetu na soubor a vrátí i jeho tier.
func (r *LicenseRegistry) ResolveCheckpoint(key string) (ModelRef, error) {
	e, err := lookup("checkpoint", r.Checkpoints, key)
	if err != nil {
		return ModelRef{}, err
	}
	return ModelRef{Key: key, File: e.File, Tier: e.Tier}, nil
}

// ResolveLoRA přeloží klíč LoRA. Placeholder $user_lora sem nepatří —
// ten řeší volající dřív, protože jeho cesta nepochází z registru.
func (r *LicenseRegistry) ResolveLoRA(key string) (ModelRef, error) {
	if key == UserLoRAPlaceholder {
		return ModelRef{}, fmt.Errorf("%s je runtime placeholder, ne položka registru", UserLoRAPlaceholder)
	}
	e, err := lookup("lora", r.LoRAs, key)
	if err != nil {
		return ModelRef{}, err
	}
	return ModelRef{Key: key, File: e.File, Tier: e.Tier}, nil
}

// ResolveControlNet vrátí soubor ControlNetu pro logický typ (depth, softedge…).
//
// Dnes všechny typy pokrývá jeden model — xinsir union promax. Kdyby se to
// změnilo, mapování se rozšíří tady a presety se měnit nemusí.
func (r *LicenseRegistry) ResolveControlNet(kind string) (ModelRef, error) {
	switch kind {
	case "depth", "softedge", "inpaint":
		e, err := lookup("controlnet", r.ControlNets, "xinsir_union_promax")
		if err != nil {
			return ModelRef{}, err
		}
		return ModelRef{Key: "xinsir_union_promax", File: e.File, Tier: e.Tier}, nil
	case "openpose":
		e, err := lookup("controlnet", r.ControlNets, "xinsir_openpose")
		if err != nil {
			return ModelRef{}, err
		}
		return ModelRef{Key: "xinsir_openpose", File: e.File, Tier: e.Tier}, nil
	default:
		return ModelRef{}, fmt.Errorf("neznámý typ controlnetu %q", kind)
	}
}

// ResolveIPAdapter vrátí IP-Adapter model pro identitu.
// Vždy plus-face varianta — je z Apache repa h94/IP-Adapter a nemá nic
// společného s FaceID variantami, které jsou CC-BY-NC.
func (r *LicenseRegistry) ResolveIPAdapter() (ModelRef, error) {
	e, err := lookup("ipadapter", r.IPAdapters, "ip_adapter_plus_face_sdxl")
	if err != nil {
		return ModelRef{}, err
	}
	return ModelRef{Key: "ip_adapter_plus_face_sdxl", File: e.File, Tier: e.Tier}, nil
}

// ResolveCLIPVision vrátí image encoder pro IP-Adapter.
func (r *LicenseRegistry) ResolveCLIPVision() (ModelRef, error) {
	e, err := lookup("clip_vision", r.CLIPVision, "clip_vit_h_14")
	if err != nil {
		return ModelRef{}, err
	}
	return ModelRef{Key: "clip_vit_h_14", File: e.File, Tier: e.Tier}, nil
}

// EffectiveTier spočítá skutečný tier presetu: nejpřísnější ze všech modelů,
// které preset použije, a z hodnoty, kterou si preset sám deklaruje.
//
// Deklarace v presetu se bere jako spodní hranice důvěry, ne jako pravda —
// preset si nemůže sám sobě povolit blocked checkpoint.
func (r *LicenseRegistry) EffectiveTier(p *Preset) (Tier, error) {
	tier := Tier(p.LicenseTier)
	if tier == "" {
		tier = TierVerify
	}

	ck, err := r.ResolveCheckpoint(p.Checkpoint)
	if err != nil {
		return TierBlocked, err
	}
	tier = stricter(tier, ck.Tier)

	for _, l := range p.LoRAs {
		if l.Name == UserLoRAPlaceholder {
			// User LoRA se trénuje na datech uživatele nad SDXL base —
			// licenčně čisté z definice, v registru nemá co dělat.
			continue
		}
		ref, err := r.ResolveLoRA(l.Name)
		if err != nil {
			return TierBlocked, err
		}
		tier = stricter(tier, ref.Tier)
	}

	for _, cn := range p.ControlNet {
		ref, err := r.ResolveControlNet(cn.Type)
		if err != nil {
			return TierBlocked, err
		}
		tier = stricter(tier, ref.Tier)
	}

	if p.Identity.Method == "ipadapter" {
		ref, err := r.ResolveIPAdapter()
		if err != nil {
			return TierBlocked, err
		}
		tier = stricter(tier, ref.Tier)
		cv, err := r.ResolveCLIPVision()
		if err != nil {
			return TierBlocked, err
		}
		tier = stricter(tier, cv.Tier)
	}

	// Face restore nemá v v0 čistou implementaci — GFPGAN i CodeFormer jsou
	// licenčně mimo. Preset, který ho zapne, tím padá do verify.
	if p.FaceRestore > 0 {
		tier = stricter(tier, TierVerify)
	}

	return tier, nil
}

// Allowed rozhodne, jestli se preset s daným tierem smí nabídnout uživateli.
// Ve strict režimu (produkce) projde jen safe.
func (r *LicenseRegistry) Allowed(t Tier) bool {
	if t == TierBlocked {
		return false
	}
	if r.Strict && t == TierVerify {
		return false
	}
	return true
}

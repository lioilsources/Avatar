package attrs

import (
	"path/filepath"
	"strings"
	"testing"
)

func table(t *testing.T) *Table {
	t.Helper()
	// Skutečný datový soubor, ne fixtura — testy tím hlídají i data.
	tb, err := Load(filepath.Join("..", "..", "..", "data", "attributes.yaml"))
	if err != nil {
		t.Fatalf("attributes.yaml se nenačetl: %v", err)
	}
	return tb
}

func TestRealTableLoadsAndValidates(t *testing.T) {
	tb := table(t)
	if len(tb.Attributes) == 0 {
		t.Fatal("prázdná tabulka atributů")
	}
	// Každý atribut musí mít vrstvu, která má i vykonavatele nebo aspoň
	// prompt fallback — jinak by slider nic nedělal.
	for name, a := range tb.Attributes {
		if a.Layer == LayerPrompt && a.Prompt == nil {
			t.Errorf("atribut %q je vrstvy prompt, ale nemá prompt fragmenty", name)
		}
		if a.Group == "" {
			t.Errorf("atribut %q nemá group — nezobrazí se v žádném tabu UI", name)
		}
	}
}

func TestValidateRejectsOutOfRange(t *testing.T) {
	tb := table(t)
	err := tb.Validate(Vector{"lips_fullness": 2.5}, nil)
	if err == nil || !strings.Contains(err.Error(), "mimo rozsah") {
		t.Fatalf("hodnota mimo rozsah prošla, err = %v", err)
	}
}

func TestValidateRejectsUnknownAttr(t *testing.T) {
	tb := table(t)
	if err := tb.Validate(Vector{"neexistuje": 0.5}, nil); err == nil {
		t.Fatal("neznámý atribut prošel validací")
	}
}

func TestValidateRejectsUnsupportedByPreset(t *testing.T) {
	tb := table(t)
	supported := map[string]bool{"eye_color": true}
	err := tb.Validate(Vector{"lips_fullness": 0.5}, supported)
	if err == nil || !strings.Contains(err.Error(), "nepodporuje") {
		t.Fatalf("atribut mimo supports_attrs prošel, err = %v", err)
	}
}

func TestValidateRejectsBadDiscreteOption(t *testing.T) {
	tb := table(t)
	if err := tb.Validate(Vector{"eye_color": "duhova"}, nil); err == nil {
		t.Fatal("neplatná volba diskrétního atributu prošla")
	}
	// A správná projít musí.
	if err := tb.Validate(Vector{"eye_color": "blue"}, nil); err != nil {
		t.Fatalf("platná volba neprošla: %v", err)
	}
}

func TestValidateRejectsWrongType(t *testing.T) {
	tb := table(t)
	if err := tb.Validate(Vector{"eye_color": 0.5}, nil); err == nil {
		t.Fatal("číslo místo řetězce u diskrétního atributu prošlo")
	}
	if err := tb.Validate(Vector{"lips_fullness": "hodne"}, nil); err == nil {
		t.Fatal("nečíselný řetězec u spojitého atributu prošel")
	}
}

// TestDefaultValuesProduceNoPromptNoise je jádro toho, proč se fragmenty
// filtrují: obrázek, kde uživatel nic nezměnil, nesmí mít prompt zaplevelený
// frázemi typu "neutral expression, brown eyes".
func TestDefaultValuesProduceNoPromptNoise(t *testing.T) {
	tb := table(t)
	v := Vector{"eye_color": "brown", "mood": "neutral", "lips_fullness": 0.0}
	frags := tb.PromptFragments(v, "natural")
	if len(frags) != 0 {
		t.Fatalf("výchozí hodnoty vyprodukovaly fragmenty %v, čekáno žádné", frags)
	}
}

func TestChangedDiscreteProducesFragment(t *testing.T) {
	tb := table(t)
	frags := tb.PromptFragments(Vector{"eye_color": "blue"}, "natural")
	if len(frags) != 1 || !strings.Contains(frags[0], "blue") {
		t.Fatalf("fragmenty = %v, čekán jeden o modrých očích", frags)
	}
}

func TestSmallContinuousDeltaIsIgnored(t *testing.T) {
	tb := table(t)
	// Malá odchylka se nemá verbalizovat — prompt je hrubý nástroj,
	// jemné doladění patří geometry vrstvě.
	if frags := tb.PromptFragments(Vector{"lips_fullness": 0.1}, "natural"); len(frags) != 0 {
		t.Fatalf("malá odchylka dala fragmenty %v", frags)
	}
	if frags := tb.PromptFragments(Vector{"lips_fullness": 0.5}, "natural"); len(frags) != 1 {
		t.Fatalf("střední odchylka dala %v, čekán jeden fragment", frags)
	}
}

func TestStrongContinuousDeltaGetsIntensifier(t *testing.T) {
	tb := table(t)
	frags := tb.PromptFragments(Vector{"lips_fullness": 0.9}, "natural")
	if len(frags) != 1 || !strings.HasPrefix(frags[0], "very ") {
		t.Fatalf("silná odchylka dala %v, čekáno zesílení předponou 'very'", frags)
	}
}

func TestNegativeDeltaPicksNegativeFragment(t *testing.T) {
	tb := table(t)
	frags := tb.PromptFragments(Vector{"lips_fullness": -0.8}, "natural")
	if len(frags) != 1 || !strings.Contains(frags[0], "thin") {
		t.Fatalf("záporná odchylka dala %v, čekán fragment o tenkých rtech", frags)
	}
}

// TestFragmentOrderIsStable je podmínka reprodukovatelnosti: stejný recept
// musí dát stejný prompt, jinak stejný seed nedá stejný obrázek.
func TestFragmentOrderIsStable(t *testing.T) {
	tb := table(t)
	v := Vector{"eye_color": "blue", "hair_style": "dreadlocks", "mood": "smiling", "lips_fullness": 0.8}

	first := Join(tb.PromptFragments(v, "natural"))
	for i := 0; i < 50; i++ {
		if got := Join(tb.PromptFragments(v, "natural")); got != first {
			t.Fatalf("pořadí fragmentů není stabilní:\n  %q\n  %q", first, got)
		}
	}
}

func TestUnknownDialectYieldsNoFragments(t *testing.T) {
	tb := table(t)
	if frags := tb.PromptFragments(Vector{"eye_color": "blue"}, "klingon"); len(frags) != 0 {
		t.Fatalf("neznámý dialekt dal fragmenty %v", frags)
	}
}

func TestBooruDialectDiffersFromNatural(t *testing.T) {
	tb := table(t)
	v := Vector{"mood": "smiling"}
	nat := Join(tb.PromptFragments(v, "natural"))
	boo := Join(tb.PromptFragments(v, "booru"))
	if nat == "" || boo == "" {
		t.Fatalf("chybí fragment: natural=%q booru=%q", nat, boo)
	}
	if nat == boo {
		t.Errorf("dialekty daly stejný text %q — pak nemá smysl je rozlišovat", nat)
	}
}

func TestGuardScoreCountsDeviations(t *testing.T) {
	tb := table(t)

	if s := tb.GuardScore(Vector{"eye_color": "brown", "lips_fullness": 0.0}); s != 0 {
		t.Fatalf("výchozí hodnoty daly skóre %v, čekáno 0", s)
	}

	// 1.0 za změněnou diskrétní + 0.8 za spojitou odchylku.
	s := tb.GuardScore(Vector{"eye_color": "blue", "lips_fullness": 0.8})
	if s < 1.7 || s > 1.9 {
		t.Fatalf("skóre = %v, čekáno kolem 1.8", s)
	}
}

func TestGuardThresholdsAreOrdered(t *testing.T) {
	tb := table(t)
	g := tb.IdentityGuard
	if g.WarnThreshold <= 0 || g.MaxThreshold <= g.WarnThreshold {
		t.Fatalf("prahy nedávají smysl: warn=%v max=%v", g.WarnThreshold, g.MaxThreshold)
	}
}

func TestLayersAreAssignedAsPlanned(t *testing.T) {
	tb := table(t)
	// Kontrola klíčového rozhodnutí z §2: barva očí NESMÍ být prompt vrstva,
	// protože silná identita ji promptem vrátí zpátky. A tvar rtů musí být
	// geometrie, ne prompt.
	if a, ok := tb.Get("eye_color"); !ok || a.Layer != LayerMask {
		t.Errorf("eye_color musí být vrstvy mask, je %q", a.Layer)
	}
	if a, ok := tb.Get("lips_fullness"); !ok || a.Layer != LayerGeometry {
		t.Errorf("lips_fullness musí být vrstvy geometry, je %q", a.Layer)
	}
	if a, ok := tb.Get("hair_style"); !ok || a.Layer != LayerMask {
		t.Errorf("hair_style musí být vrstvy mask, je %q", a.Layer)
	}
}

func TestMaskAttributesHaveInpaintParams(t *testing.T) {
	tb := table(t)
	for name, a := range tb.Attributes {
		if a.Layer != LayerMask {
			continue
		}
		if a.Inpaint == nil {
			t.Errorf("atribut %q je vrstvy mask, ale nemá inpaint parametry", name)
			continue
		}
		if a.Inpaint.Denoise <= 0 || a.Inpaint.Denoise > 1 {
			t.Errorf("atribut %q má denoise %v mimo (0,1]", name, a.Inpaint.Denoise)
		}
		// Snížená váha identity v masce je celý mechanismus, kterým se
		// atribut prosadí proti identitě (§2).
		if a.Inpaint.IdentityWeightOverride <= 0 || a.Inpaint.IdentityWeightOverride > 0.5 {
			t.Errorf("atribut %q má identity_weight_override %v — čekáno nízké kladné číslo",
				name, a.Inpaint.IdentityWeightOverride)
		}
	}
}

func TestVolumetricHairStylesGetMaskExpand(t *testing.T) {
	tb := table(t)
	// Dready a afro jsou objemnější než původní účes. Bez expandu masky
	// se ořežou na siluetu toho starého.
	a, ok := tb.Get("hair_style")
	if !ok {
		t.Fatal("hair_style chybí")
	}
	if a.Inpaint.Expand < 16 {
		t.Fatalf("hair_style má expand %d — pro dready/afro je to málo", a.Inpaint.Expand)
	}
}

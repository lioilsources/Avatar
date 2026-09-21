package preset

import (
	"path/filepath"
	"strings"
	"testing"
)

// registry načte skutečný data/licenses.yaml, ne fixturu. Testy tím hlídají
// i sám datový soubor — když někdo přidá blocked model do presetu nebo
// omylem přepne tier, spadne to tady, ne až v produkci.
func registry(t *testing.T) *LicenseRegistry {
	t.Helper()
	reg, err := LoadLicenseRegistry(filepath.Join("..", "..", "..", "data", "licenses.yaml"))
	if err != nil {
		t.Fatalf("nelze načíst licenses.yaml: %v", err)
	}
	return reg
}

func TestBlockedCheckpointsStayBlocked(t *testing.T) {
	reg := registry(t)

	// Tyhle checkpointy fyzicky leží na DGX Sparku a používají se tam pro
	// jiné projekty. Pro Avatar jsou licenčně zakázané a nesmí se dát
	// vyřešit ani omylem.
	for _, key := range []string{
		"juggernaut_xl_v9",
		"juggernaut_xl_lightning",
		"pony_diffusion_v6_xl",
		"atomix_pony_anime_xl_v30",
	} {
		if _, err := reg.ResolveCheckpoint(key); err == nil {
			t.Errorf("checkpoint %q se vyřešil, ale má být blocked", key)
		}
	}
}

func TestSafeCheckpointsResolve(t *testing.T) {
	reg := registry(t)
	for _, key := range []string{"sd_xl_base_1.0", "illustrious_xl_v2.0"} {
		ref, err := reg.ResolveCheckpoint(key)
		if err != nil {
			t.Errorf("checkpoint %q se má vyřešit, ale: %v", key, err)
			continue
		}
		if ref.Tier != TierSafe {
			t.Errorf("checkpoint %q má tier %q, čekáno safe", key, ref.Tier)
		}
		if !strings.HasSuffix(ref.File, ".safetensors") {
			t.Errorf("checkpoint %q ukazuje na %q, což nevypadá jako soubor modelu", key, ref.File)
		}
	}
}

func TestUnknownCheckpointFails(t *testing.T) {
	reg := registry(t)
	// Neznámý klíč musí spadnout, ne projít. Kdyby resolver neznámé klíče
	// propouštěl, celý registr by byl k ničemu.
	if _, err := reg.ResolveCheckpoint("nejaky_novy_model_co_nikdo_neoveril"); err == nil {
		t.Fatal("neznámý checkpoint se vyřešil, měl selhat")
	}
}

func TestIPAdapterIsNotFaceIDVariant(t *testing.T) {
	reg := registry(t)
	ref, err := reg.ResolveIPAdapter()
	if err != nil {
		t.Fatalf("IP-Adapter se nevyřešil: %v", err)
	}
	// FaceID varianty jsou CC-BY-NC kvůli závislosti na InsightFace.
	// Kdyby se sem taková varianta dostala, je to licenční průšvih.
	if strings.Contains(strings.ToLower(ref.File), "faceid") {
		t.Fatalf("resolver vrátil FaceID variantu %q — ta je CC-BY-NC", ref.File)
	}
	if ref.Tier != TierSafe {
		t.Fatalf("IP-Adapter má tier %q, čekáno safe", ref.Tier)
	}
}

func TestInstantIDControlNetIsBlocked(t *testing.T) {
	reg := registry(t)
	// InstantID ControlNet je sám o sobě neškodný soubor, ale je k ničemu
	// bez InsightFace pipeline. Musí zůstat blocked, ať nikoho neláká.
	e, ok := reg.ControlNets["instantid_controlnet"]
	if !ok {
		t.Fatal("instantid_controlnet zmizel z registru — má tam zůstat jako blocked")
	}
	if e.Tier != TierBlocked {
		t.Fatalf("instantid_controlnet má tier %q, musí být blocked", e.Tier)
	}
}

func TestDepthAnythingBaseIsBlocked(t *testing.T) {
	reg := registry(t)
	// Base/Large/Giant jsou CC-BY-NC-4.0; Apache je jen Small.
	// Původní plán tvrdil opak, proto je na to test.
	if e := reg.Auxiliary["depth_anything_v2_base"]; e.Tier != TierBlocked {
		t.Errorf("depth_anything_v2_base má tier %q, musí být blocked (CC-BY-NC-4.0)", e.Tier)
	}
	if e := reg.Auxiliary["depth_anything_v2_small"]; e.Tier != TierSafe {
		t.Errorf("depth_anything_v2_small má tier %q, čekáno safe", e.Tier)
	}
}

func TestFaceRestoreIsBlocked(t *testing.T) {
	reg := registry(t)
	// GFPGAN i CodeFormer jsou mimo — GFPGAN kvůli StyleGAN2 (NVIDIA
	// non-komerční) a DFDNet (CC-BY-NC-SA) ve vlastním LICENSE souboru.
	for _, key := range []string{"gfpgan", "codeformer"} {
		if e := reg.Auxiliary[key]; e.Tier != TierBlocked {
			t.Errorf("%s má tier %q, musí být blocked", key, e.Tier)
		}
	}
}

func TestEffectiveTierTakesStricterOfDeclaredAndRegistry(t *testing.T) {
	reg := registry(t)

	// Preset si nesmí sám sobě povolit blocked checkpoint tím, že
	// deklaruje license_tier: safe.
	p := &Preset{
		ID:          "podvrzeny",
		Checkpoint:  "juggernaut_xl_v9",
		LicenseTier: "safe",
		Identity:    Identity{Method: "lora_only"},
	}
	if _, err := reg.EffectiveTier(p); err == nil {
		t.Fatal("preset s blocked checkpointem a deklarací safe prošel — musí selhat")
	}

	// A naopak: preset s čistým checkpointem, který se sám deklaruje jako
	// verify, zůstane verify. Deklarace je spodní hranice důvěry.
	p2 := &Preset{
		ID:          "opatrny",
		Checkpoint:  "sd_xl_base_1.0",
		LicenseTier: "verify",
		Identity:    Identity{Method: "lora_only"},
	}
	tier, err := reg.EffectiveTier(p2)
	if err != nil {
		t.Fatalf("neočekávaná chyba: %v", err)
	}
	if tier != TierVerify {
		t.Fatalf("tier = %q, čekáno verify", tier)
	}
}

func TestFaceRestoreDowngradesPresetToVerify(t *testing.T) {
	reg := registry(t)
	p := &Preset{
		ID:          "s-restorem",
		Checkpoint:  "sd_xl_base_1.0",
		LicenseTier: "safe",
		Identity:    Identity{Method: "lora_only"},
		FaceRestore: 0.5,
	}
	tier, err := reg.EffectiveTier(p)
	if err != nil {
		t.Fatalf("neočekávaná chyba: %v", err)
	}
	// Žádná čistá implementace face restore neexistuje — preset, který ho
	// zapne, nesmí zůstat safe.
	if tier != TierVerify {
		t.Fatalf("preset s face_restore=0.5 má tier %q, čekáno verify", tier)
	}
}

func TestUserLoRAPlaceholderSkipsRegistry(t *testing.T) {
	reg := registry(t)
	p := &Preset{
		ID:          "s-user-lorou",
		Checkpoint:  "sd_xl_base_1.0",
		LicenseTier: "safe",
		Identity:    Identity{Method: "ipadapter"},
		LoRAs:       []LoRA{{Name: UserLoRAPlaceholder, Weight: 0.75, Optional: true}},
	}
	tier, err := reg.EffectiveTier(p)
	if err != nil {
		t.Fatalf("$user_lora neprošel: %v", err)
	}
	// User LoRA se trénuje na datech uživatele nad SDXL base — je čistá
	// z definice a v registru nemá co dělat.
	if tier != TierSafe {
		t.Fatalf("tier = %q, čekáno safe", tier)
	}
}

func TestStrictModeHidesVerifyPresets(t *testing.T) {
	reg := registry(t)

	reg.Strict = true
	if reg.Allowed(TierVerify) {
		t.Error("ve strict režimu musí být verify nedostupné")
	}
	if !reg.Allowed(TierSafe) {
		t.Error("safe musí být dostupné vždy")
	}
	if reg.Allowed(TierBlocked) {
		t.Error("blocked nesmí projít nikdy")
	}

	reg.Strict = false
	if !reg.Allowed(TierVerify) {
		t.Error("mimo strict režim má být verify dostupné")
	}
	if reg.Allowed(TierBlocked) {
		t.Error("blocked nesmí projít ani mimo strict režim")
	}
}

func TestStricterPicksHigherRank(t *testing.T) {
	cases := []struct{ a, b, want Tier }{
		{TierSafe, TierVerify, TierVerify},
		{TierVerify, TierSafe, TierVerify},
		{TierSafe, TierBlocked, TierBlocked},
		{TierVerify, TierBlocked, TierBlocked},
		{TierSafe, TierSafe, TierSafe},
		// Neznámá hodnota se chová jako nejpřísnější — překlep v datech
		// nesmí vést k povolení.
		{TierSafe, Tier("preklep"), Tier("preklep")},
	}
	for _, c := range cases {
		if got := stricter(c.a, c.b); got != c.want {
			t.Errorf("stricter(%q, %q) = %q, čekáno %q", c.a, c.b, got, c.want)
		}
	}
}

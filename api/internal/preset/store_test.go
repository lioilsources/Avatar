package preset

import (
	"os"
	"path/filepath"
	"testing"
)

func dataDir() string { return filepath.Join("..", "..", "..", "data") }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(dataDir(), "presets"), filepath.Join(dataDir(), "licenses.yaml"))
	if err != nil {
		t.Fatalf("store se nenačetl: %v", err)
	}
	return s
}

// TestRealPresetsLoad ověřuje, že skutečné presety v data/presets/ projdou
// schématem i licenční kontrolou. Rozbitý preset tím spadne v CI, ne až
// při startu API.
func TestRealPresetsLoad(t *testing.T) {
	s := newTestStore(t)

	all := s.List(false)
	if len(all) == 0 {
		t.Fatal("nenačten žádný preset")
	}

	for _, p := range all {
		if p.EffectiveTier() == TierBlocked {
			t.Errorf("preset %s vyšel jako blocked — do repa nemá co dělat", p.ID)
		}
		// face_restore musí být v v0 nulový; GFPGAN i CodeFormer jsou
		// licenčně mimo.
		if p.FaceRestore != 0 {
			t.Errorf("preset %s má face_restore=%v, v v0 musí být 0", p.ID, p.FaceRestore)
		}
		if !p.NSFWNegativeLock {
			t.Errorf("preset %s nemá nsfw_negative_lock", p.ID)
		}
	}
}

// TestPresetsOnlyReferenceKnownAttrs hlídá, že supports_attrs neodkazuje
// na atributy, které v tabulce neexistují — UI by pak nabídlo slider,
// který nikam nevede.
func TestPresetsOnlyReferenceKnownAttrs(t *testing.T) {
	s := newTestStore(t)

	raw, err := os.ReadFile(filepath.Join(dataDir(), "attributes.yaml"))
	if err != nil {
		t.Fatalf("attributes.yaml: %v", err)
	}
	body := string(raw)

	for _, p := range s.List(false) {
		for _, a := range p.SupportsAttrs {
			// Hrubá kontrola na přítomnost klíče — plný parser tabulky
			// žije v balíčku attrs a importovat ho sem by udělalo cyklus.
			if !contains(body, "\n  "+a+":") {
				t.Errorf("preset %s odkazuje na neznámý atribut %q", p.ID, a)
			}
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

func TestListAllowedFiltersByTier(t *testing.T) {
	s := newTestStore(t)

	all := s.List(false)
	allowed := s.List(true)
	if len(allowed) > len(all) {
		t.Fatal("povolených presetů nemůže být víc než všech")
	}
	for _, p := range allowed {
		if !s.Allowed(p) {
			t.Errorf("preset %s je ve výpisu povolených, ale Allowed() říká ne", p.ID)
		}
	}
}

func TestGetReturnsPresetRegardlessOfTier(t *testing.T) {
	s := newTestStore(t)
	all := s.List(false)
	if len(all) == 0 {
		t.Skip("žádné presety")
	}
	p, ok := s.Get(all[0].ID)
	if !ok || p.ID != all[0].ID {
		t.Fatalf("Get(%q) selhalo", all[0].ID)
	}
	if _, ok := s.Get("neexistujici-preset"); ok {
		t.Fatal("Get vrátil neexistující preset")
	}
}

func TestReloadIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	before := len(s.List(false))
	if err := s.Reload(); err != nil {
		t.Fatalf("reload selhal: %v", err)
	}
	if after := len(s.List(false)); after != before {
		t.Fatalf("po reloadu %d presetů, před ním %d", after, before)
	}
}

func TestSupports(t *testing.T) {
	p := &Preset{SupportsAttrs: []string{"eye_color", "hair_style"}}
	if !p.Supports("eye_color") {
		t.Error("eye_color má být podporovaný")
	}
	if p.Supports("beard") {
		t.Error("beard nemá být podporovaný")
	}
}

func TestResDefaults(t *testing.T) {
	p := &Preset{}
	if r := p.Res(); r.Width != 1024 || r.Height != 1024 {
		t.Fatalf("výchozí rozlišení = %dx%d, čekáno 1024x1024", r.Width, r.Height)
	}
	p2 := &Preset{Resolution: &Resolution{Width: 768, Height: 768}}
	if r := p2.Res(); r.Width != 768 {
		t.Fatalf("rozlišení = %d, čekáno 768", r.Width)
	}
}

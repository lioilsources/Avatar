package recipe

import (
	"testing"
	"time"

	"github.com/ol1n/avatar/api/internal/attrs"
)

func base() *Recipe {
	return &Recipe{
		ID:            "r1",
		FaceID:        "f1",
		PresetID:      "studio-headshot",
		PresetVersion: 1,
		Attrs:         attrs.Vector{"eye_color": "blue", "lips_fullness": 0.5},
		Seed:          42,
		FaceVersion:   1,
		ModelHashes:   map[string]string{"checkpoint": "sd_xl_base_1.0"},
		CreatedAt:     time.Now(),
	}
}

// TestFingerprintIsStable je podmínka cache: stejné vstupy musí dát stejný
// otisk pokaždé, jinak by cache nikdy netrefila.
func TestFingerprintIsStable(t *testing.T) {
	r := base()
	first := r.Fingerprint()
	for range 100 {
		if got := r.Fingerprint(); got != first {
			t.Fatalf("otisk není stabilní: %q vs %q", first, got)
		}
	}
}

// TestFingerprintIgnoresIdentityAndTime — otisk popisuje vstupy renderu,
// ne konkrétní záznam. Dva recepty se stejnými vstupy musí dát stejný
// obrázek, i když vznikly v jinou dobu a mají jiné id.
func TestFingerprintIgnoresIdentityAndTime(t *testing.T) {
	a := base()
	b := base()
	b.ID = "uplne-jine-id"
	b.CreatedAt = time.Now().Add(72 * time.Hour)
	b.ParentID = "neco"

	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("otisk se změnil kvůli id nebo času, což nemá vliv na výsledek")
	}
}

func TestFingerprintChangesWithRenderInputs(t *testing.T) {
	orig := base().Fingerprint()

	cases := map[string]func(*Recipe){
		"seed":           func(r *Recipe) { r.Seed = 43 },
		"atribut":        func(r *Recipe) { r.Attrs["eye_color"] = "green" },
		"preset":         func(r *Recipe) { r.PresetID = "jiny" },
		"verze presetu":  func(r *Recipe) { r.PresetVersion = 2 },
		"identita":       func(r *Recipe) { r.FaceID = "f2" },
		"verze identity": func(r *Recipe) { r.FaceVersion = 2 },
		"model":          func(r *Recipe) { r.ModelHashes["checkpoint"] = "jiny_model" },
	}

	for name, mutate := range cases {
		r := base()
		mutate(r)
		if r.Fingerprint() == orig {
			t.Errorf("změna %q nezměnila otisk, ale mění výsledek renderu", name)
		}
	}
}

// TestFaceVersionInFingerprint je jádro rerenderu po dotrénování (§4.2):
// tentýž recept s face_v2 musí dát jiný otisk než s face_v1, jinak by
// cache vrátila starý obrázek a zlepšení by se nikdy neprojevilo.
func TestFaceVersionChangesFingerprint(t *testing.T) {
	v1 := base()
	v2 := base()
	v2.FaceVersion = 2

	if v1.Fingerprint() == v2.Fingerprint() {
		t.Fatal("face_v1 a face_v2 mají stejný otisk — rerender by vrátil starý obrázek z cache")
	}
}

func TestStorePutAndGet(t *testing.T) {
	s := NewStore()
	r := base()
	s.Put(r)

	got, ok := s.Get("r1")
	if !ok || got.ID != "r1" {
		t.Fatal("Get nenašel uložený recept")
	}
	if _, ok := s.Get("neexistuje"); ok {
		t.Error("Get vrátil neexistující recept")
	}
}

func TestPutTwiceDoesNotDuplicateInFaceIndex(t *testing.T) {
	s := NewStore()
	r := base()
	s.Put(r)
	r.Seed = 99
	s.Put(r) // stejné id, aktualizace

	if got := len(s.ListByFace("f1")); got != 1 {
		t.Fatalf("recept je v indexu %dx, čekáno 1x", got)
	}
}

func TestListByFaceIsNewestFirst(t *testing.T) {
	s := NewStore()
	now := time.Now()

	for i, id := range []string{"a", "b", "c"} {
		r := base()
		r.ID = id
		r.CreatedAt = now.Add(time.Duration(i) * time.Minute)
		s.Put(r)
	}

	list := s.ListByFace("f1")
	if len(list) != 3 {
		t.Fatalf("vráceno %d receptů, čekáno 3", len(list))
	}
	// Historie miniatur v editoru chce nejnovější první (§9.2).
	if list[0].ID != "c" || list[2].ID != "a" {
		t.Fatalf("pořadí je %s, %s, %s — čekáno c, b, a", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestListByFaceIsolatesFaces(t *testing.T) {
	s := NewStore()
	a := base()
	s.Put(a)

	b := base()
	b.ID = "r2"
	b.FaceID = "f2"
	s.Put(b)

	if got := len(s.ListByFace("f1")); got != 1 {
		t.Errorf("f1 má %d receptů, čekáno 1", got)
	}
	if got := len(s.ListByFace("f2")); got != 1 {
		t.Errorf("f2 má %d receptů, čekáno 1", got)
	}
}

// TestDeleteByFaceRemovesEverything je součást mazání účtu (§12) — po
// smazání identity nesmí zůstat dohledatelný žádný její recept.
func TestDeleteByFaceRemovesEverything(t *testing.T) {
	s := NewStore()
	for _, id := range []string{"a", "b", "c"} {
		r := base()
		r.ID = id
		s.Put(r)
	}

	if n := s.DeleteByFace("f1"); n != 3 {
		t.Fatalf("smazáno %d receptů, čekáno 3", n)
	}
	if len(s.ListByFace("f1")) != 0 {
		t.Error("po smazání zůstaly recepty v indexu")
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := s.Get(id); ok {
			t.Errorf("recept %s je pořád dohledatelný podle id", id)
		}
	}
}

func TestParentIDFormsHistoryChain(t *testing.T) {
	s := NewStore()

	first := base()
	s.Put(first)

	second := base()
	second.ID = "r2"
	second.ParentID = first.ID
	s.Put(second)

	got, _ := s.Get("r2")
	if got.ParentID != "r1" {
		t.Fatalf("ParentID = %q, čekáno r1", got.ParentID)
	}
}

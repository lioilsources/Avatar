// Package recipe implementuje recept jako objekt první třídy (§7.3 plánu).
//
// Ukládá se recept, ne jen výsledné PNG. Zadarmo z toho plyne undo/redo,
// rerender po dotrénování LoRA, re-render ve vyšším rozlišení a regenerace
// po změně modelu nebo licence checkpointu.
package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/ol1n/avatar/api/internal/attrs"
)

// Recipe = (face_id, preset_id@version, attr_vector, seed, model_hashes, timestamp)
type Recipe struct {
	ID            string       `json:"id"`
	FaceID        string       `json:"face_id"`
	PresetID      string       `json:"preset_id"`
	PresetVersion int          `json:"preset_version"`
	Attrs         attrs.Vector `json:"attrs"`
	Seed          int64        `json:"seed"`

	// ModelHashes zaznamenává, čím se to vyrenderovalo. Bez toho nejde
	// poznat, že rerender dal jiný výsledek kvůli změně modelu.
	ModelHashes map[string]string `json:"model_hashes,omitempty"`

	// FaceVersion je verze identity použitá při renderu (1 = instant,
	// 2 = s user LoRA). Recept sám referencuje face_id bez verze —
	// tohle je jen záznam toho, co bylo k dispozici tehdy.
	FaceVersion int `json:"face_version"`

	CreatedAt time.Time `json:"created_at"`

	// ParentID skládá historii do stromu, ne do lineárního seznamu.
	// Uživatel se může vrátit o pět kroků a odbočit jinam (§9.2).
	ParentID string `json:"parent_id,omitempty"`
}

// Fingerprint je otisk vstupů, které determinují výsledek.
//
// Slouží k cache: dva recepty se stejným otiskem musí dát stejný obrázek.
// Timestamp ani ID se do něj schválně nepočítají.
func (r *Recipe) Fingerprint() string {
	type stable struct {
		FaceID        string            `json:"f"`
		FaceVersion   int               `json:"fv"`
		PresetID      string            `json:"p"`
		PresetVersion int               `json:"pv"`
		Attrs         map[string]any    `json:"a"`
		Seed          int64             `json:"s"`
		ModelHashes   map[string]string `json:"m"`
	}

	// Mapy se v Go serializují v náhodném pořadí jen u range; encoding/json
	// klíče řadí, takže je otisk stabilní. Explicitní kopie je tu proto,
	// aby se nezměnil, kdyby se do Recipe přidalo další pole.
	a := map[string]any{}
	for k, v := range r.Attrs {
		a[k] = v
	}

	b, _ := json.Marshal(stable{
		FaceID:        r.FaceID,
		FaceVersion:   r.FaceVersion,
		PresetID:      r.PresetID,
		PresetVersion: r.PresetVersion,
		Attrs:         a,
		Seed:          r.Seed,
		ModelHashes:   r.ModelHashes,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Store je in-memory úložiště receptů.
// Ve Fázi 3 ho nahradí Postgres s JSONB sloupcem (§8.3).
type Store struct {
	mu     sync.RWMutex
	byID   map[string]*Recipe
	byFace map[string][]string // face_id → seznam recipe_id v pořadí vzniku
}

func NewStore() *Store {
	return &Store{
		byID:   map[string]*Recipe{},
		byFace: map[string][]string{},
	}
}

func (s *Store) Put(r *Recipe) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[r.ID]; !exists {
		s.byFace[r.FaceID] = append(s.byFace[r.FaceID], r.ID)
	}
	s.byID[r.ID] = r
}

func (s *Store) Get(id string) (*Recipe, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	return r, ok
}

// ListByFace vrátí recepty jedné identity, nejnovější první.
// Tohle je ta historie miniatur v editoru (§9.2).
func (s *Store) ListByFace(faceID string) []*Recipe {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byFace[faceID]
	out := make([]*Recipe, 0, len(ids))
	for _, id := range ids {
		if r, ok := s.byID[id]; ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// DeleteByFace smaže všechny recepty identity — součást mazání účtu (§12).
func (s *Store) DeleteByFace(faceID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.byFace[faceID]
	for _, id := range ids {
		delete(s.byID, id)
	}
	delete(s.byFace, faceID)
	return len(ids)
}

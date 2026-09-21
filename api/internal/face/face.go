// Package face drží identitu uživatele — nahrané fotky, odvozená data
// a případnou natrénovanou LoRA.
package face

import (
	"sync"
	"time"
)

// Version rozlišuje, jak daleko je identita zpracovaná (§4.3 plánu).
type Version int

const (
	// V1 — instant track. Máme výřez obličeje pro IP-Adapter a landmarky.
	// Podobnost střední, dostupné během sekund.
	V1 Version = 1
	// V2 — quality track. Navíc natrénovaná per-user LoRA.
	// Podobnost výrazně lepší, dostupné po pár minutách.
	V2 Version = 2
)

// Face je identita jednoho uživatele.
//
// Recipe referencuje face_id, ne verzi — díky tomu je rerender po dotrénování
// LoRA zadarmo: stejný recept, jiná verze identity, lepší výsledek.
type Face struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`

	// Zdrojové fotky po quality gate. Cesty na disk, ne obsah.
	// Mají TTL — jsou to biometrické údaje podle GDPR čl. 9 (§12).
	SourcePhotos []string `json:"source_photos"`

	// Výřez obličeje, který jde do IP-Adapteru přes CLIP image encoder.
	RefCrop string `json:"ref_crop"`

	// Landmarky z MediaPipe. Ve Fázi 0 prázdné — analýza přijde ve Fázi 2.
	Landmarks []Landmark `json:"landmarks,omitempty"`

	// Per-user LoRA. Prázdné, dokud trénink nedoběhne.
	// POZOR: tohle je odvozený biometrický údaj — musí se mazat s účtem (§12).
	LoRAPath    string `json:"lora_path,omitempty"`
	LoRATrigger string `json:"lora_trigger_token,omitempty"`

	// Stav tréninku, aby UI mohlo nenápadně ukázat progress (§9.1).
	TrainState TrainState `json:"train_state"`
}

// Landmark je jeden bod obličeje v normalizovaných souřadnicích [0,1].
type Landmark struct {
	Index int     `json:"i"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
}

type TrainState string

const (
	TrainNone    TrainState = "none"    // trénink ještě nezačal
	TrainQueued  TrainState = "queued"  // čeká ve frontě q:train
	TrainRunning TrainState = "running" // běží
	TrainDone    TrainState = "done"    // hotovo, LoRAPath je vyplněná
	TrainFailed  TrainState = "failed"  // selhalo — appka jede dál na instant tracku
)

// Version vrátí, na jaké úrovni identita je.
func (f *Face) Version() Version {
	if f.LoRAPath != "" && f.TrainState == TrainDone {
		return V2
	}
	return V1
}

// Store je in-memory úložiště identit.
//
// Fáze 0 schválně nemá Postgres — vertikální řez má ověřit pipeline,
// ne perzistenci. Rozhraní je ale takové, aby ho šlo ve Fázi 1 vyměnit
// za Postgres bez zásahu do handlerů (§8.3).
type Store struct {
	mu    sync.RWMutex
	faces map[string]*Face
}

func NewStore() *Store {
	return &Store{faces: map[string]*Face{}}
}

// Upgrade povýší identitu z face_v1 na face_v2 po dotrénování LoRA (§4.3).
//
// Recipe referencuje face_id, ne verzi — proto se tady nic nekopíruje ani
// neverzuje. Stejný recept po tomhle volání vyrenderuje líp.
func (s *Store) Upgrade(id, loraPath, trigger string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.faces[id]
	if !ok {
		return false
	}
	f.LoRAPath = loraPath
	f.LoRATrigger = trigger
	f.TrainState = TrainDone
	return true
}

// SetTrainState zaznamená posun tréninku, aby ho UI mohlo nenápadně
// ukázat ("dolaďuji tvůj avatar…", §9.1).
func (s *Store) SetTrainState(id string, st TrainState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.faces[id]
	if !ok {
		return false
	}
	f.TrainState = st
	return true
}

func (s *Store) Put(f *Face) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faces[f.ID] = f
}

func (s *Store) Get(id string) (*Face, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.faces[id]
	return f, ok
}

// Delete smaže identitu z paměti. Volající je zodpovědný za smazání
// souborů na disku — včetně LoRA (GDPR, §12).
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.faces[id]
	delete(s.faces, id)
	return ok
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.faces)
}

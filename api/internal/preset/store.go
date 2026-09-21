package preset

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Store drží načtené presety a licenční registr. Je bezpečný pro souběžné
// čtení a umí hot-reload — presety se dají měnit bez restartu API.
type Store struct {
	dir         string
	schemaPath  string
	licensePath string

	mu       sync.RWMutex
	presets  map[string]*Preset
	reg      *LicenseRegistry
	loadedAt time.Time
}

// NewStore načte presety z adresáře a licenční registr.
func NewStore(dir, licensePath string) (*Store, error) {
	s := &Store{
		dir:         dir,
		schemaPath:  filepath.Join(dir, "_schema.json"),
		licensePath: licensePath,
		presets:     map[string]*Preset{},
	}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload načte licenční registr a všechny presety znovu.
//
// Nevalidní preset se přeskočí a zaloguje — jeden rozbitý soubor nesmí
// shodit celý katalog. Rozbitý licenční registr naopak reload zruší celý:
// bez něj nejde bezpečně rozhodnout, co se smí nabídnout.
func (s *Store) Reload() error {
	reg, err := LoadLicenseRegistry(s.licensePath)
	if err != nil {
		return err
	}

	// Strict režim jde vypnout jen v devu — v produkci se na hodnotu
	// v souboru spoléhat nechceme.
	if os.Getenv("AVATAR_STRICT_LICENSE") == "0" {
		reg.Strict = false
		slog.Warn("licenční strict režim VYPNUT přes AVATAR_STRICT_LICENSE=0 — presety s tierem verify budou dostupné")
	}

	schema, err := compileSchema(s.schemaPath)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("preset dir: %w", err)
	}

	loaded := map[string]*Preset{}
	var skipped int

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "_") {
			continue
		}
		path := filepath.Join(s.dir, name)
		p, err := loadPreset(path, schema, reg)
		if err != nil {
			slog.Error("preset přeskočen", "file", name, "err", err)
			skipped++
			continue
		}
		if prev, dup := loaded[p.ID]; dup {
			slog.Error("duplicitní preset id", "id", p.ID, "file", name, "already_from", prev.ID)
			skipped++
			continue
		}
		loaded[p.ID] = p
	}

	if len(loaded) == 0 {
		return fmt.Errorf("nenačten ani jeden validní preset z %s", s.dir)
	}

	s.mu.Lock()
	s.presets = loaded
	s.reg = reg
	s.loadedAt = time.Now()
	s.mu.Unlock()

	slog.Info("presety načteny",
		"count", len(loaded),
		"skipped", skipped,
		"strict_license", reg.Strict,
		"licenses_verified_at", reg.VerifiedAt,
	)
	return nil
}

func compileSchema(path string) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("preset schema: %w", err)
	}
	defer f.Close() //nolint:errcheck
	if err := c.AddResource(path, f); err != nil {
		return nil, fmt.Errorf("preset schema add: %w", err)
	}
	sch, err := c.Compile(path)
	if err != nil {
		return nil, fmt.Errorf("preset schema compile: %w", err)
	}
	return sch, nil
}

func loadPreset(path string, schema *jsonschema.Schema, reg *LicenseRegistry) (*Preset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Validace proti schematu jde první — dá lepší chybovou hlášku
	// než neúspěšný unmarshal do struktury.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("nevalidní JSON: %w", err)
	}
	if err := schema.Validate(generic); err != nil {
		return nil, fmt.Errorf("neodpovídá schematu: %w", err)
	}

	var p Preset
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}

	// Jméno souboru musí sedět na id — jinak se v adresáři nedá vyznat
	// a hot-reload začne být záhadný.
	if base := strings.TrimSuffix(filepath.Base(path), ".json"); base != p.ID {
		return nil, fmt.Errorf("id %q nesedí na jméno souboru %q", p.ID, base)
	}

	tier, err := reg.EffectiveTier(&p)
	if err != nil {
		return nil, err
	}
	p.effectiveTier = tier

	if tier != Tier(p.LicenseTier) {
		slog.Warn("preset deklaruje jiný license_tier, než vychází z registru — platí ten přísnější",
			"preset", p.ID, "declared", p.LicenseTier, "effective", tier)
	}

	return &p, nil
}

// Get vrátí preset podle id. Vrací i presety, které nejsou Allowed —
// rozhodnutí o dostupnosti dělá volající, protože rerender uloženého
// receptu se chová jinak než výpis katalogu.
func (s *Store) Get(id string) (*Preset, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.presets[id]
	return p, ok
}

// List vrátí presety seřazené podle id. Když onlyAllowed, vyfiltruje ty,
// které aktuální licenční režim nepovoluje.
func (s *Store) List(onlyAllowed bool) []*Preset {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Preset, 0, len(s.presets))
	for _, p := range s.presets {
		if onlyAllowed && !s.reg.Allowed(p.effectiveTier) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Registry vrátí aktuální licenční registr.
func (s *Store) Registry() *LicenseRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reg
}

// Allowed je zkratka pro kontrolu jednoho presetu.
func (s *Store) Allowed(p *Preset) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reg.Allowed(p.effectiveTier)
}

// WatchDir spouští Reload při změně v adresáři presetů nebo v licenses.yaml.
//
// Záměrně polling přes mtime, ne fsnotify: adresář je malý, interval 5 s
// je pro hot-reload dost a ušetří to závislost i platformní rozdíly.
func (s *Store) WatchDir(interval time.Duration, stop <-chan struct{}) {
	last := s.dirFingerprint()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			cur := s.dirFingerprint()
			if cur == last {
				continue
			}
			last = cur
			if err := s.Reload(); err != nil {
				slog.Error("hot-reload presetů selhal, běží se dál na staré sadě", "err", err)
			}
		}
	}
}

// dirFingerprint složí otisk z názvů a časů modifikace sledovaných souborů.
func (s *Store) dirFingerprint() string {
	var b strings.Builder
	if fi, err := os.Stat(s.licensePath); err == nil {
		fmt.Fprintf(&b, "lic:%d;", fi.ModTime().UnixNano())
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return b.String()
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(s.dir, n))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s:%d;", n, fi.ModTime().UnixNano())
	}
	return b.String()
}

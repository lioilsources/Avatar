package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config je konfigurace avatar-api. Všechno přes env, žádný config soubor —
// stejný pattern jako gen-queue v AiStacku.
type Config struct {
	ListenAddr string

	// DataDir obsahuje presets/, attributes.yaml a licenses.yaml.
	DataDir string
	// StorageDir drží nahrané fotky a vyrenderované výsledky.
	StorageDir string

	ComfyURL     string
	ComfyTimeout time.Duration

	// RenderWorkers omezuje souběžné rendery. ComfyUI má jednu GPU
	// a jednu frontu — víc než pár souběžných jobů jen zvýší latenci.
	RenderWorkers int

	JobTTL time.Duration

	// MaxUploadBytes je strop na jednu fotku.
	MaxUploadBytes int64
	// MaxPhotos je strop na počet fotek jedné identity (§3.3: doporuč 3–5).
	MaxPhotos int

	// PublicBaseURL se používá pro sestavení URL na výsledky.
	PublicBaseURL string

	CORSOrigins []string
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:     envStr("AVATAR_LISTEN_ADDR", ":8092"),
		DataDir:        envStr("AVATAR_DATA_DIR", "../data"),
		StorageDir:     envStr("AVATAR_STORAGE_DIR", "./storage"),
		ComfyURL:       envStr("AVATAR_COMFY_URL", "http://127.0.0.1:8188"),
		ComfyTimeout:   time.Duration(envInt("AVATAR_COMFY_TIMEOUT_SECONDS", 300)) * time.Second,
		RenderWorkers:  envInt("AVATAR_RENDER_WORKERS", 2),
		JobTTL:         time.Duration(envInt("AVATAR_JOB_TTL_SECONDS", 3600)) * time.Second,
		MaxUploadBytes: int64(envInt("AVATAR_MAX_UPLOAD_MB", 12)) * 1024 * 1024,
		MaxPhotos:      envInt("AVATAR_MAX_PHOTOS", 5),
		PublicBaseURL:  envStr("AVATAR_PUBLIC_BASE_URL", "http://127.0.0.1:8092"),
		CORSOrigins:    []string{envStr("AVATAR_CORS_ORIGIN", "*")},
	}

	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.DataDir = abs

	abs, err = filepath.Abs(c.StorageDir)
	if err != nil {
		return nil, err
	}
	c.StorageDir = abs

	if err := os.MkdirAll(filepath.Join(c.StorageDir, "faces"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(c.StorageDir, "renders"), 0o755); err != nil {
		return nil, err
	}

	// Chybějící datové soubory jsou chyba konfigurace, ne runtime —
	// ať to spadne při startu s jasnou hláškou, ne až u prvního requestu.
	for _, p := range []string{c.PresetsDir(), c.AttributesPath(), c.LicensesPath()} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("chybí datový soubor %s (nastav AVATAR_DATA_DIR): %w", p, err)
		}
	}

	return c, nil
}

func (c *Config) PresetsDir() string     { return filepath.Join(c.DataDir, "presets") }
func (c *Config) AttributesPath() string { return filepath.Join(c.DataDir, "attributes.yaml") }
func (c *Config) LicensesPath() string   { return filepath.Join(c.DataDir, "licenses.yaml") }
func (c *Config) FacesDir() string       { return filepath.Join(c.StorageDir, "faces") }
func (c *Config) RendersDir() string     { return filepath.Join(c.StorageDir, "renders") }

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

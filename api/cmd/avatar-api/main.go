// avatar-api je backend Face Avatar Studia.
//
// Fáze 0 (vertikální řez): upload → render → výsledek, se SSE progresem.
// Analýza obličeje, warp, masky a LoRA trénink přijdou v dalších fázích —
// datové modely pro ně už ale existují (data/attributes.yaml, face_v1/v2).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/ol1n/avatar/api/internal/attrs"
	"github.com/ol1n/avatar/api/internal/comfy"
	"github.com/ol1n/avatar/api/internal/config"
	"github.com/ol1n/avatar/api/internal/handler"
	"github.com/ol1n/avatar/api/internal/preset"
	"github.com/ol1n/avatar/api/internal/train"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(); err != nil {
		slog.Error("avatar-api skončilo chybou", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	presets, err := preset.NewStore(cfg.PresetsDir(), cfg.LicensesPath())
	if err != nil {
		return err
	}

	table, err := attrs.Load(cfg.AttributesPath())
	if err != nil {
		return err
	}
	slog.Info("atributová tabulka načtena", "version", table.Version, "attributes", len(table.Attributes))

	comfyClient := comfy.NewClient(cfg.ComfyURL, cfg.ComfyTimeout)

	// ComfyUI nemusí běžet při startu — API se nastartuje a /health to
	// ohlásí jako degraded. Tvrdý fail by zbytečně vázal start API na
	// pořadí spouštění služeb.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := comfyClient.Health(ctx); err != nil {
		slog.Warn("comfyui není dostupné, rendery zatím neprojdou", "url", cfg.ComfyURL, "err", err)
	} else {
		slog.Info("comfyui dostupné", "url", cfg.ComfyURL)
	}
	cancel()

	// Trénink běží na stroji s GPU. Ve vývoji se tam sahá přes SSH,
	// v produkci poběží avatar-api přímo na Sparku a SSHHost bude prázdný.
	trainCfg := train.DefaultConfig()
	trainCfg.SSHHost = os.Getenv("AVATAR_TRAIN_SSH_HOST")
	if v := os.Getenv("AVATAR_TRAIN_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			trainCfg.Steps = n
		}
	}
	if v := os.Getenv("AVATAR_TRAIN_RANK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			trainCfg.Rank = n
		}
	}
	if v := os.Getenv("AVATAR_TRAIN_RESOLUTION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			trainCfg.Resolution = n
		}
	}
	slog.Info("tréninková fronta připravena",
		"ssh_host", trainCfg.SSHHost, "rank", trainCfg.Rank,
		"steps", trainCfg.Steps, "resolution", trainCfg.Resolution,
		"max_concurrent", trainCfg.MaxConcurrent)

	api := handler.New(cfg, presets, table, comfyClient, trainCfg)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Recoverer)
	r.Use(requestLogger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: cfg.CORSOrigins,
		AllowedMethods: []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Accept", "Content-Type", "Authorization"},
		MaxAge:         300,
	}))
	api.Routes(r)

	stop := make(chan struct{})
	defer close(stop)

	// Hot-reload presetů a licenčního registru bez restartu (§8.1).
	go presets.WatchDir(5*time.Second, stop)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: r,
		// Bez WriteTimeout schválně: SSE stream u /renders/{id}/events
		// běží desítky sekund a fixní timeout by ho utnul uprostřed.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("avatar-api naslouchá",
			"addr", cfg.ListenAddr,
			"comfy", cfg.ComfyURL,
			"data", cfg.DataDir,
			"storage", cfg.StorageDir,
			"workers", cfg.RenderWorkers,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server spadl", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	slog.Info("ukončuji, dobíhají otevřená spojení")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

// requestLogger loguje requesty; SSE stream vynechává, aby log nezaplavil.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		if r.URL.Path[len(r.URL.Path)-len("/events"):] == "/events" && len(r.URL.Path) > len("/events") {
			return
		}
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"ms", time.Since(start).Milliseconds(),
			"req_id", chimw.GetReqID(r.Context()),
		)
	})
}

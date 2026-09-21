package handler

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ol1n/avatar/api/internal/attrs"
	"github.com/ol1n/avatar/api/internal/comfy"
	"github.com/ol1n/avatar/api/internal/config"
	"github.com/ol1n/avatar/api/internal/face"
	"github.com/ol1n/avatar/api/internal/preset"
	"github.com/ol1n/avatar/api/internal/recipe"
	"github.com/ol1n/avatar/api/internal/store"
	"github.com/ol1n/avatar/api/internal/train"
)

// API sdružuje závislosti HTTP vrstvy.
type API struct {
	cfg     *config.Config
	presets *preset.Store
	table   *attrs.Table
	faces   *face.Store
	recipes *recipe.Store
	jobs    *store.Jobs
	comfy   *comfy.Client
	trainQ  *train.Queue

	renderQ chan renderTask
}

func New(cfg *config.Config, presets *preset.Store, table *attrs.Table, comfyClient *comfy.Client, trainCfg train.Config) *API {
	a := &API{
		cfg:     cfg,
		presets: presets,
		table:   table,
		faces:   face.NewStore(),
		recipes: recipe.NewStore(),
		jobs:    store.NewJobs(cfg.JobTTL),
		comfy:   comfyClient,
		renderQ: make(chan renderTask, 128),
	}
	a.startRenderWorkers(cfg.RenderWorkers)

	// Trénink má vlastní frontu s vlastním stropem souběžnosti — pětiminutový
	// job nesmí zablokovat interaktivní rendery (§8.2).
	a.trainQ = train.NewQueue(trainCfg, train.NewScriptRunner(trainCfg), a.onTrainDone)

	return a
}

// onTrainDone povýší identitu na face_v2. Od tohohle okamžiku každý
// další render z uloženého receptu jede s user LoRA — recept se nemění.
func (a *API) onTrainDone(faceID string, res *train.Result) {
	if !a.faces.Upgrade(faceID, res.LoRAName, res.Trigger) {
		slog.Warn("trénink doběhl, ale identita už neexistuje (smazaná?)", "face", faceID)
		return
	}
	slog.Info("identita povýšena na face_v2", "face", faceID, "lora", res.LoRAName)
}

// Routes registruje endpointy.
func (a *API) Routes(r chi.Router) {
	r.Get("/health", a.health)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/presets", a.listPresets)
		r.Get("/attributes", a.getAttributes)

		r.Post("/faces", a.createFace)
		r.Get("/faces/{faceID}", a.getFace)
		r.Delete("/faces/{faceID}", a.deleteFace)

		r.Post("/faces/{faceID}/train", a.startTraining)
		r.Get("/faces/{faceID}/train", a.getTraining)

		r.Post("/renders", a.createRender)
		r.Post("/renders/rerender", a.rerender)
		r.Get("/renders/{jobID}", a.getRender)
		r.Get("/renders/{jobID}/events", a.renderEvents)
		r.Get("/renders/{jobID}/result", a.renderResult)

		r.Get("/recipes/{recipeID}", a.getRecipe)
	})
}

// ---------------------------------------------------------------- health

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"status":  "ok",
		"service": "avatar-api",
		"presets": len(a.presets.List(false)),
		"jobs":    a.jobs.Count(),
		"faces":   a.faces.Count(),
	}

	// ComfyUI je externí závislost — když neběží, API sice odpovídá,
	// ale render neprojde. Ať to zdravotní check řekne rovnou.
	if err := a.comfy.Health(r.Context()); err != nil {
		status["status"] = "degraded"
		status["comfyui"] = err.Error()
	} else {
		status["comfyui"] = "ok"
	}

	if reg := a.presets.Registry(); reg != nil {
		status["license_strict"] = reg.Strict
		status["licenses_verified_at"] = reg.VerifiedAt
	}

	writeJSON(w, http.StatusOK, status)
}

// ---------------------------------------------------------------- presety

type presetDTO struct {
	ID            string            `json:"id"`
	Version       int               `json:"version"`
	Category      string            `json:"category"`
	Label         map[string]string `json:"label,omitempty"`
	SupportsAttrs []string          `json:"supports_attrs"`
	Profile       string            `json:"profile"`
	LicenseTier   string            `json:"license_tier"`
	Resolution    preset.Resolution `json:"resolution"`
}

func (a *API) listPresets(w http.ResponseWriter, r *http.Request) {
	// Katalog nabízí jen to, co licenční režim povoluje. Blocked a (ve
	// strict režimu) verify presety se uživateli vůbec neukážou.
	list := a.presets.List(true)

	out := make([]presetDTO, 0, len(list))
	for _, p := range list {
		out = append(out, presetDTO{
			ID:            p.ID,
			Version:       p.Version,
			Category:      p.Category,
			Label:         p.Label,
			SupportsAttrs: p.SupportsAttrs,
			Profile:       p.Profile,
			LicenseTier:   string(p.EffectiveTier()),
			Resolution:    p.Res(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"presets": out})
}

func (a *API) getAttributes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        a.table.Version,
		"attributes":     a.table.Attributes,
		"groups":         a.table.Groups,
		"identity_guard": a.table.IdentityGuard,
	})
}

// ---------------------------------------------------------------- identita

func (a *API) createFace(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(a.cfg.MaxUploadBytes * int64(a.cfg.MaxPhotos)); err != nil {
		writeErr(w, http.StatusBadRequest, "nepodařilo se přečíst upload: "+err.Error())
		return
	}

	files := r.MultipartForm.File["photos"]
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "chybí soubory pod klíčem 'photos'")
		return
	}
	if len(files) > a.cfg.MaxPhotos {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("maximálně %d fotek, přišlo %d", a.cfg.MaxPhotos, len(files)))
		return
	}

	faceID := uuid.NewString()
	dir := filepath.Join(a.cfg.FacesDir(), faceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	f := &face.Face{
		ID:         faceID,
		CreatedAt:  time.Now(),
		TrainState: face.TrainNone,
	}

	for i, fh := range files {
		if fh.Size > a.cfg.MaxUploadBytes {
			cleanup(dir)
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("fotka %s má %d B, limit je %d B", fh.Filename, fh.Size, a.cfg.MaxUploadBytes))
			return
		}

		src, err := fh.Open()
		if err != nil {
			cleanup(dir)
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}

		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".webp" {
			src.Close() //nolint:errcheck
			cleanup(dir)
			writeErr(w, http.StatusBadRequest, "podporované formáty: jpg, png, webp (přišlo "+ext+")")
			return
		}

		dst := filepath.Join(dir, fmt.Sprintf("%02d%s", i, ext))
		out, err := os.Create(dst)
		if err != nil {
			src.Close() //nolint:errcheck
			cleanup(dir)
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := io.Copy(out, io.LimitReader(src, a.cfg.MaxUploadBytes)); err != nil {
			src.Close() //nolint:errcheck
			out.Close() //nolint:errcheck
			cleanup(dir)
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		src.Close() //nolint:errcheck
		out.Close() //nolint:errcheck

		f.SourcePhotos = append(f.SourcePhotos, dst)
	}

	// Fáze 0: jako referenční výřez se bere první fotka tak, jak přišla.
	// Quality gate běží na zařízení (§3.1), MediaPipe analýza a skutečný
	// crop podle landmarků přijdou ve Fázi 2.
	f.RefCrop = f.SourcePhotos[0]

	a.faces.Put(f)
	slog.Info("identita vytvořena", "face_id", faceID, "photos", len(f.SourcePhotos))

	writeJSON(w, http.StatusCreated, map[string]any{
		"face_id":     f.ID,
		"photos":      len(f.SourcePhotos),
		"version":     f.Version(),
		"train_state": f.TrainState,
	})
}

func (a *API) getFace(w http.ResponseWriter, r *http.Request) {
	f, ok := a.faces.Get(chi.URLParam(r, "faceID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "identita nenalezena")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"face_id":     f.ID,
		"version":     f.Version(),
		"photos":      len(f.SourcePhotos),
		"train_state": f.TrainState,
		"created_at":  f.CreatedAt,
	})
}

// deleteFace maže identitu včetně souborů. Je to biometrický údaj podle
// GDPR čl. 9 — mazání musí zahrnout i odvozená data, tedy i user LoRA (§12).
func (a *API) deleteFace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "faceID")
	f, ok := a.faces.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "identita nenalezena")
		return
	}

	// User LoRA je odvozený biometrický údaj — musí zmizet spolu s identitou
	// (§12). Žije v adresáři modelů ComfyUI, ne v našem storage, takže
	// mazání jde přes runner, který ví, jestli je to lokálně nebo přes SSH.
	if f.LoRAPath != "" {
		if err := a.trainQ.DeleteLoRA(r.Context(), f.LoRAPath); err != nil {
			// Nezdařené smazání je compliance problém, ne kosmetika —
			// nesmí zapadnout a nesmí se tvářit jako úspěch.
			slog.Error("NEPODAŘILO SE SMAZAT USER LORA", "face_id", id, "lora", f.LoRAPath, "err", err)
			writeErr(w, http.StatusInternalServerError, "nepodařilo se smazat odvozená data")
			return
		}
		slog.Info("user LoRA smazána", "face_id", id, "lora", f.LoRAPath)
	}

	cleanup(filepath.Join(a.cfg.FacesDir(), id))
	n := a.recipes.DeleteByFace(id)
	a.faces.Delete(id)

	slog.Info("identita smazána", "face_id", id, "recipes", n)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- render

type createRenderReq struct {
	FaceID   string       `json:"face_id"`
	PresetID string       `json:"preset_id"`
	Attrs    attrs.Vector `json:"attrs"`
	Seed     *int64       `json:"seed"`
	ParentID string       `json:"parent_recipe_id,omitempty"`
}

func (a *API) createRender(w http.ResponseWriter, r *http.Request) {
	var req createRenderReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "nevalidní JSON: "+err.Error())
		return
	}

	p, ok := a.presets.Get(req.PresetID)
	if !ok {
		writeErr(w, http.StatusNotFound, "preset "+req.PresetID+" neexistuje")
		return
	}
	// Kontrola i tady, ne jen ve výpisu — klient si mohl id presetu
	// zapamatovat z doby, kdy byl ještě povolený.
	if !a.presets.Allowed(p) {
		writeErr(w, http.StatusForbidden,
			fmt.Sprintf("preset %s je nedostupný (license_tier=%s)", p.ID, p.EffectiveTier()))
		return
	}

	var f *face.Face
	if req.FaceID != "" {
		f, ok = a.faces.Get(req.FaceID)
		if !ok {
			writeErr(w, http.StatusNotFound, "identita "+req.FaceID+" neexistuje")
			return
		}
	}

	supported := map[string]bool{}
	for _, s := range p.SupportsAttrs {
		supported[s] = true
	}
	if err := a.table.Validate(req.Attrs, supported); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Vrstvy geometry a mask nemají ve Fázi 0 vykonavatele. Prompt fallback
	// sice něco udělá, ale slabě — ať klient ví, že to zatím není ono.
	var degraded []string
	for name := range req.Attrs {
		if at, ok := a.table.Get(name); ok && at.Layer != attrs.LayerPrompt {
			degraded = append(degraded, name)
		}
	}

	seed := int64(0)
	if req.Seed != nil {
		seed = *req.Seed
	} else {
		seed = randomSeed()
	}

	rec := &recipe.Recipe{
		ID:            uuid.NewString(),
		FaceID:        req.FaceID,
		PresetID:      p.ID,
		PresetVersion: p.Version,
		Attrs:         req.Attrs,
		Seed:          seed,
		CreatedAt:     time.Now(),
		ParentID:      req.ParentID,
	}
	if f != nil {
		rec.FaceVersion = int(f.Version())
	}
	a.recipes.Put(rec)

	job := &store.Job{
		ID:       uuid.NewString(),
		RecipeID: rec.ID,
		FaceID:   req.FaceID,
		PresetID: p.ID,
	}
	a.jobs.Create(job)

	select {
	case a.renderQ <- renderTask{job: job, rec: rec, preset: p, face: f}:
	default:
		writeErr(w, http.StatusServiceUnavailable, "fronta renderů je plná, zkus to za chvíli")
		return
	}

	resp := map[string]any{
		"job_id":    job.ID,
		"recipe_id": rec.ID,
		"seed":      seed,
		"status":    store.StatusQueued,
		"events":    fmt.Sprintf("%s/v1/renders/%s/events", a.cfg.PublicBaseURL, job.ID),
	}
	if len(degraded) > 0 {
		resp["degraded_attrs"] = degraded
		resp["degraded_note"] = "atributy vrstev geometry/mask jedou ve Fázi 0 jen přes prompt — přesné vynucení přijde ve Fázi 2"
	}
	if score := a.table.GuardScore(req.Attrs); score > a.table.IdentityGuard.WarnThreshold {
		resp["identity_warning"] = fmt.Sprintf(
			"součet změn %.1f překračuje práh %.1f — podobnost se může ztrácet",
			score, a.table.IdentityGuard.WarnThreshold)
	}

	writeJSON(w, http.StatusAccepted, resp)
}

func (a *API) getRender(w http.ResponseWriter, r *http.Request) {
	j, ok := a.jobs.Get(chi.URLParam(r, "jobID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "job nenalezen (možná vypršel TTL)")
		return
	}
	writeJSON(w, http.StatusOK, jobDTO(&j, a.cfg.PublicBaseURL))
}

// renderEvents streamuje postup renderu přes SSE.
func (a *API) renderEvents(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")

	ch, ok := a.jobs.Subscribe(jobID)
	if !ok {
		writeErr(w, http.StatusNotFound, "job nenalezen")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "server neumí streamovat")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Vypnutí bufferování na proxy — bez toho Cloudflare/Caddy drží
	// události a SSE ztrácí smysl.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Heartbeat drží spojení otevřené přes proxy s idle timeoutem.
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-beat.C:
			fmt.Fprint(w, ": beat\n\n") //nolint:errcheck
			flusher.Flush()

		case j, open := <-ch:
			if !open {
				fmt.Fprint(w, "event: end\ndata: {}\n\n") //nolint:errcheck
				flusher.Flush()
				return
			}
			b, err := json.Marshal(jobDTO(&j, a.cfg.PublicBaseURL))
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: progress\ndata: %s\n\n", b) //nolint:errcheck
			flusher.Flush()
		}
	}
}

func (a *API) renderResult(w http.ResponseWriter, r *http.Request) {
	j, ok := a.jobs.Get(chi.URLParam(r, "jobID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "job nenalezen")
		return
	}
	if j.Status != store.StatusDone {
		writeErr(w, http.StatusConflict, "render ještě není hotový (stav: "+string(j.Status)+")")
		return
	}
	if j.ResultPath == "" {
		writeErr(w, http.StatusInternalServerError, "job je hotový, ale nemá výsledek")
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, j.ResultPath)
}

func (a *API) getRecipe(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.recipes.Get(chi.URLParam(r, "recipeID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "recept nenalezen")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recipe":      rec,
		"fingerprint": rec.Fingerprint(),
	})
}

// ---------------------------------------------------------------- pomocné

func jobDTO(j *store.Job, baseURL string) map[string]any {
	out := map[string]any{
		"job_id":     j.ID,
		"recipe_id":  j.RecipeID,
		"preset_id":  j.PresetID,
		"status":     j.Status,
		"percent":    j.Percent,
		"stage":      j.Stage,
		"created_at": j.Created,
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	if j.Status == store.StatusDone {
		out["result_url"] = fmt.Sprintf("%s/v1/renders/%s/result", baseURL, j.ID)
		out["duration_ms"] = j.Duration().Milliseconds()
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("zápis odpovědi selhal", "err", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func cleanup(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("úklid adresáře selhal", "dir", dir, "err", err)
	}
}

// randomSeed vrátí náhodný seed. crypto/rand, aby dva současné requesty
// nedostaly stejnou hodnotu.
func randomSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano() % (1 << 32)
	}
	// Kladné a v rozsahu, který ComfyUI přijme.
	return int64(binary.LittleEndian.Uint64(b[:]) % (1 << 32))
}

// ---------------------------------------------------------------- trénink

// startTraining zařadí trénink per-user LoRA (§4.2).
//
// Vrací se okamžitě. Trénink běží na pozadí a nesmí blokovat nic —
// dokud nedoběhne, appka jede na instant tracku (§9.1 krok 3).
func (a *API) startTraining(w http.ResponseWriter, r *http.Request) {
	faceID := chi.URLParam(r, "faceID")
	f, ok := a.faces.Get(faceID)
	if !ok {
		writeErr(w, http.StatusNotFound, "identita nenalezena")
		return
	}

	if f.TrainState == face.TrainRunning || f.TrainState == face.TrainQueued {
		writeErr(w, http.StatusConflict, "trénink už běží")
		return
	}
	if len(f.SourcePhotos) == 0 {
		writeErr(w, http.StatusBadRequest, "identita nemá žádné fotky")
		return
	}

	// Rare token, na který se identita naváže. Odvozený z face_id, aby
	// dvě LoRA v jednom promptu nekolidovaly.
	trigger := "ohwx" + strings.ReplaceAll(faceID[:8], "-", "") + " person"

	jobID := uuid.NewString()

	job, accepted := a.trainQ.Submit(train.Request{
		JobID:   jobID,
		FaceID:  faceID,
		Images:  f.SourcePhotos,
		Trigger: trigger,
	})
	if !accepted {
		writeErr(w, http.StatusServiceUnavailable, "tréninková fronta je plná")
		return
	}

	a.faces.SetTrainState(faceID, face.TrainQueued)
	slog.Info("trénink zařazen", "face", faceID, "job", jobID, "photos", len(f.SourcePhotos))

	writeJSON(w, http.StatusAccepted, map[string]any{
		"train_job_id": job.ID,
		"face_id":      faceID,
		"state":        job.State,
		"steps":        job.Steps,
		"trigger":      trigger,
		"note":         "appka mezitím jede na instant tracku; po dokončení se recepty rerendrují ve vyšší kvalitě",
	})
}

func (a *API) getTraining(w http.ResponseWriter, r *http.Request) {
	faceID := chi.URLParam(r, "faceID")
	f, ok := a.faces.Get(faceID)
	if !ok {
		writeErr(w, http.StatusNotFound, "identita nenalezena")
		return
	}

	job, hasJob := a.trainQ.ByFace(faceID)
	out := map[string]any{
		"face_id":      faceID,
		"face_version": f.Version(),
		"train_state":  f.TrainState,
	}
	if hasJob {
		out["job"] = job
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- rerender

type rerenderReq struct {
	RecipeID string `json:"recipe_id"`
	// PresetOverride umožní zrenderovat tentýž vzhled jiným profilem
	// (např. preview → final). Prázdné = původní preset.
	PresetOverride string `json:"preset_id,omitempty"`
}

// rerender zrenderuje uložený recept znovu (§7.3).
//
// Hlavní důvod, proč recepty existují: po dotrénování LoRA se stejný
// recept vyrenderuje s face_v2, tedy výrazně podobněji — a uživatel
// za to nic nedělá. Recept se přitom nemění, mění se jen identita,
// na kterou odkazuje.
func (a *API) rerender(w http.ResponseWriter, r *http.Request) {
	var req rerenderReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "nevalidní JSON: "+err.Error())
		return
	}

	rec, ok := a.recipes.Get(req.RecipeID)
	if !ok {
		writeErr(w, http.StatusNotFound, "recept nenalezen")
		return
	}

	presetID := rec.PresetID
	if req.PresetOverride != "" {
		presetID = req.PresetOverride
	}
	p, ok := a.presets.Get(presetID)
	if !ok {
		writeErr(w, http.StatusNotFound, "preset "+presetID+" neexistuje")
		return
	}
	// Preset mohl mezitím spadnout do blocked (změna licence). Rerender
	// starého receptu to nesmí obejít.
	if !a.presets.Allowed(p) {
		writeErr(w, http.StatusForbidden,
			fmt.Sprintf("preset %s je nedostupný (license_tier=%s)", p.ID, p.EffectiveTier()))
		return
	}

	var f *face.Face
	if rec.FaceID != "" {
		f, ok = a.faces.Get(rec.FaceID)
		if !ok {
			writeErr(w, http.StatusNotFound, "identita receptu už neexistuje")
			return
		}
	}

	// Nový recept se stejnými vstupy, ale aktuální verzí identity —
	// původní zůstává v historii nedotčený.
	fresh := &recipe.Recipe{
		ID:            uuid.NewString(),
		FaceID:        rec.FaceID,
		PresetID:      p.ID,
		PresetVersion: p.Version,
		Attrs:         rec.Attrs,
		Seed:          rec.Seed,
		CreatedAt:     time.Now(),
		ParentID:      rec.ID,
	}
	if f != nil {
		fresh.FaceVersion = int(f.Version())
	}
	a.recipes.Put(fresh)

	job := &store.Job{
		ID:       uuid.NewString(),
		RecipeID: fresh.ID,
		FaceID:   fresh.FaceID,
		PresetID: p.ID,
	}
	a.jobs.Create(job)

	select {
	case a.renderQ <- renderTask{job: job, rec: fresh, preset: p, face: f}:
	default:
		writeErr(w, http.StatusServiceUnavailable, "fronta renderů je plná")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":           job.ID,
		"recipe_id":        fresh.ID,
		"parent_recipe_id": rec.ID,
		"seed":             fresh.Seed,
		"face_version_was": rec.FaceVersion,
		"face_version_now": fresh.FaceVersion,
		"upgraded":         fresh.FaceVersion > rec.FaceVersion,
		"events":           fmt.Sprintf("%s/v1/renders/%s/events", a.cfg.PublicBaseURL, job.ID),
	})
}

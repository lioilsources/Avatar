package handler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ol1n/avatar/api/internal/comfy"
	"github.com/ol1n/avatar/api/internal/face"
	"github.com/ol1n/avatar/api/internal/preset"
	"github.com/ol1n/avatar/api/internal/recipe"
	"github.com/ol1n/avatar/api/internal/store"
)

// renderTask je jedna položka fronty renderů.
type renderTask struct {
	job    *store.Job
	rec    *recipe.Recipe
	preset *preset.Preset
	face   *face.Face
}

// startRenderWorkers spustí pool workerů, které berou z fronty.
//
// Jedna GPU, jedna ComfyUI fronta — smysl poolu není paralelizovat render,
// ale mít strop a nezahltit ComfyUI stovkami čekajících grafů.
func (a *API) startRenderWorkers(n int) {
	for i := 0; i < n; i++ {
		go func(worker int) {
			for t := range a.renderQ {
				a.runRender(t, worker)
			}
		}(i)
	}
}

func (a *API) runRender(t renderTask, worker int) {
	start := time.Now()
	log := slog.With("job", t.job.ID, "preset", t.preset.ID, "worker", worker)

	a.jobs.Update(t.job.ID, func(j *store.Job) {
		j.Status = store.StatusRunning
		j.Started = time.Now()
		j.Stage = "příprava"
	})

	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ComfyTimeout)
	defer cancel()

	fail := func(err error) {
		log.Error("render selhal", "err", err, "after", time.Since(start))
		a.jobs.Update(t.job.ID, func(j *store.Job) {
			j.Status = store.StatusError
			j.Error = err.Error()
			j.Finished = time.Now()
		})
	}

	// --- referenční fotka do ComfyUI -------------------------------------
	var refName string
	if t.face != nil && t.face.RefCrop != "" {
		data, err := os.ReadFile(t.face.RefCrop)
		if err != nil {
			fail(err)
			return
		}
		refName, err = a.comfy.UploadImage(ctx, "avatar_ref_"+t.face.ID+filepath.Ext(t.face.RefCrop), data)
		if err != nil {
			fail(err)
			return
		}
	}

	// --- sestavení grafu --------------------------------------------------
	g, hashes, err := comfy.Build(comfy.BuildInput{
		Preset:       t.preset,
		Recipe:       t.rec,
		Face:         t.face,
		Table:        a.table,
		Registry:     a.presets.Registry(),
		RefImageName: refName,
	})
	if err != nil {
		fail(err)
		return
	}

	// Hashe modelů patří do receptu — bez nich nejde poznat, že rerender
	// dal jiný výsledek kvůli výměně modelu (§7.3).
	t.rec.ModelHashes = hashes
	a.recipes.Put(t.rec)

	// --- odeslání do ComfyUI ---------------------------------------------
	promptID, err := a.comfy.Queue(ctx, g)
	if err != nil {
		fail(err)
		return
	}
	log.Info("graf odeslán do comfyui", "prompt_id", promptID)

	a.jobs.Update(t.job.ID, func(j *store.Job) { j.Stage = "ve frontě" })

	// --- sledování postupu ------------------------------------------------
	progress := make(chan comfy.Progress, 32)
	go func() {
		for p := range progress {
			a.jobs.Update(t.job.ID, func(j *store.Job) {
				if p.Percent > 0 {
					j.Percent = p.Percent
				}
				if p.Stage != "" {
					j.Stage = p.Stage
				}
			})
		}
	}()

	images, err := a.comfy.Watch(ctx, promptID, comfy.SamplerNodeID, progress)
	if err != nil {
		fail(err)
		return
	}

	// --- uložení výsledku -------------------------------------------------
	data, err := a.comfy.FetchImage(ctx, images[0])
	if err != nil {
		fail(err)
		return
	}

	out := filepath.Join(a.cfg.RendersDir(), t.job.ID+".png")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		fail(err)
		return
	}

	elapsed := time.Since(start)
	log.Info("render hotov", "duration", elapsed, "bytes", len(data))

	a.jobs.Update(t.job.ID, func(j *store.Job) {
		j.Status = store.StatusDone
		j.Percent = 100
		j.Stage = "hotovo"
		j.ResultPath = out
		j.Finished = time.Now()
	})
}

package train

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRunner nahrazuje skutečný trénink, aby testy nesahaly na GPU.
type fakeRunner struct {
	mu       sync.Mutex
	calls    int
	deleted  []string
	err      error
	delay    time.Duration
	progress []float64

	// concurrent sleduje, kolik tréninků běželo najednou.
	running    int
	maxRunning int
}

func (f *fakeRunner) Run(ctx context.Context, req Request, onProgress func(Job)) (*Result, error) {
	f.mu.Lock()
	f.calls++
	f.running++
	if f.running > f.maxRunning {
		f.maxRunning = f.running
	}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()

	for _, p := range f.progress {
		if onProgress != nil {
			onProgress(Job{Percent: p, Step: int(p * 10), Steps: 1000, Loss: 0.1})
		}
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if f.err != nil {
		return nil, f.err
	}
	return &Result{
		LoRAName: "avatar/" + req.FaceID + ".safetensors",
		LoRAFile: "/models/loras/avatar/" + req.FaceID + ".safetensors",
		Trigger:  req.Trigger,
		SizeMB:   28.4,
		Duration: time.Second,
	}, nil
}

func (f *fakeRunner) DeleteLoRA(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func testCfg() Config {
	c := DefaultConfig()
	c.MaxConcurrent = 1
	c.Timeout = 5 * time.Second
	return c
}

// waitFor čeká na podmínku, aby testy nezávisely na časování goroutin.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nedočkal jsem se: %s", what)
}

func TestSuccessfulTrainingUpgradesFace(t *testing.T) {
	var gotFace string
	var gotRes *Result
	var mu sync.Mutex

	q := NewQueue(testCfg(), &fakeRunner{}, func(faceID string, res *Result) {
		mu.Lock()
		defer mu.Unlock()
		gotFace, gotRes = faceID, res
	})

	job, ok := q.Submit(Request{JobID: "j1", FaceID: "face-abc", Images: []string{"a.png"}, Trigger: "ohwx person"})
	if !ok {
		t.Fatal("submit odmítnut")
	}

	waitFor(t, "dokončení tréninku", func() bool {
		j, _ := q.Get(job.ID)
		return j.State == StateDone
	})

	mu.Lock()
	defer mu.Unlock()
	if gotFace != "face-abc" {
		t.Errorf("callback dostal face %q, čekáno face-abc", gotFace)
	}
	// LoRAName musí být to, čím na ni odkáže ComfyUI — ne absolutní cesta.
	if gotRes.LoRAName != "avatar/face-abc.safetensors" {
		t.Errorf("LoRAName = %q", gotRes.LoRAName)
	}
	if gotRes.Trigger != "ohwx person" {
		t.Errorf("trigger = %q", gotRes.Trigger)
	}
}

// TestFailedTrainingDoesNotUpgrade je klíčová vlastnost z §9.1: když
// trénink selže, appka jede dál na instant tracku. Nesmí se stát, že
// se identita povýší na v2 s neexistující LoRA.
func TestFailedTrainingDoesNotUpgrade(t *testing.T) {
	var upgraded bool
	var mu sync.Mutex

	q := NewQueue(testCfg(), &fakeRunner{err: errors.New("CUDA out of memory")},
		func(string, *Result) {
			mu.Lock()
			defer mu.Unlock()
			upgraded = true
		})

	job, _ := q.Submit(Request{JobID: "j2", FaceID: "face-fail", Images: []string{"a.png"}})

	waitFor(t, "selhání tréninku", func() bool {
		j, _ := q.Get(job.ID)
		return j.State == StateFailed
	})

	j, _ := q.Get(job.ID)
	if j.Error == "" {
		t.Error("selhaný job nemá popis chyby")
	}

	mu.Lock()
	defer mu.Unlock()
	if upgraded {
		t.Fatal("identita se povýšila i po selhání tréninku")
	}
}

func TestProgressReachesJob(t *testing.T) {
	q := NewQueue(testCfg(), &fakeRunner{progress: []float64{10, 50, 90}}, nil)
	job, _ := q.Submit(Request{JobID: "j3", FaceID: "f", Images: []string{"a.png"}})

	waitFor(t, "dokončení", func() bool {
		j, _ := q.Get(job.ID)
		return j.State == StateDone
	})

	j, _ := q.Get(job.ID)
	if j.Percent != 100 {
		t.Errorf("hotový job má percent %v, čekáno 100", j.Percent)
	}
	if j.Steps != 1000 {
		t.Errorf("steps = %d, čekáno 1000", j.Steps)
	}
}

// TestMaxConcurrentIsRespected hlídá §8.2: jedna GPU, dva souběžné
// tréninky jsou pomalejší než dva po sobě a riskují OOM.
func TestMaxConcurrentIsRespected(t *testing.T) {
	fr := &fakeRunner{delay: 60 * time.Millisecond}
	cfg := testCfg()
	cfg.MaxConcurrent = 1

	q := NewQueue(cfg, fr, nil)
	for i := range 4 {
		q.Submit(Request{JobID: string(rune('a' + i)), FaceID: "f", Images: []string{"x.png"}})
	}

	waitFor(t, "dokončení všech", func() bool {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		return fr.calls == 4
	})

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.maxRunning > 1 {
		t.Fatalf("běželo až %d tréninků najednou, strop je 1", fr.maxRunning)
	}
}

func TestByFaceReturnsNewestJob(t *testing.T) {
	q := NewQueue(testCfg(), &fakeRunner{}, nil)

	q.Submit(Request{JobID: "old", FaceID: "same-face", Images: []string{"a.png"}})
	waitFor(t, "první job", func() bool {
		j, _ := q.Get("old")
		return j.State == StateDone
	})

	// Bez odstupu by oba měly stejný timestamp a pořadí by bylo nedefinované.
	time.Sleep(5 * time.Millisecond)
	q.Submit(Request{JobID: "new", FaceID: "same-face", Images: []string{"a.png"}})
	waitFor(t, "druhý job", func() bool {
		j, _ := q.Get("new")
		return j.State == StateDone
	})

	j, ok := q.ByFace("same-face")
	if !ok {
		t.Fatal("ByFace nic nenašlo")
	}
	if j.ID != "new" {
		t.Fatalf("ByFace vrátilo %q, čekáno new", j.ID)
	}

	if _, ok := q.ByFace("neznama"); ok {
		t.Error("ByFace našlo job pro neexistující identitu")
	}
}

func TestDeleteLoRADelegatesToRunner(t *testing.T) {
	fr := &fakeRunner{}
	q := NewQueue(testCfg(), fr, nil)

	if err := q.DeleteLoRA(context.Background(), "avatar/x.safetensors"); err != nil {
		t.Fatalf("mazání selhalo: %v", err)
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.deleted) != 1 || fr.deleted[0] != "avatar/x.safetensors" {
		t.Fatalf("smazáno %v", fr.deleted)
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	cfg := testCfg()
	cfg.Timeout = 30 * time.Millisecond

	q := NewQueue(cfg, &fakeRunner{delay: 2 * time.Second}, nil)
	job, _ := q.Submit(Request{JobID: "slow", FaceID: "f", Images: []string{"a.png"}})

	waitFor(t, "vypršení timeoutu", func() bool {
		j, _ := q.Get(job.ID)
		return j.State == StateFailed
	})
}

func TestQueueFullIsReported(t *testing.T) {
	cfg := testCfg()
	cfg.MaxConcurrent = 1

	q := NewQueue(cfg, &fakeRunner{delay: time.Second}, nil)
	// Kanál má kapacitu 64; naplníme ho a ještě jeden navíc.
	var refused bool
	for i := range 200 {
		if _, ok := q.Submit(Request{JobID: string(rune(i)) + "x", FaceID: "f", Images: []string{"a.png"}}); !ok {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("plná fronta nikdy neodmítla zadání")
	}
}

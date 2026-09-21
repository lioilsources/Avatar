// Package train spouští trénink per-user LoRA (§4.2 plánu).
//
// Fronta je záměrně oddělená od renderů (`q:train` v §8.2). Trénink trvá
// minuty; kdyby sdílel frontu s interaktivními rendery, jeden trénink by
// na celou tu dobu zablokoval slider drag každému uživateli.
package train

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State je stav tréninkového jobu.
type State string

const (
	StateQueued  State = "queued"
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Job je jeden trénink.
type Job struct {
	ID      string    `json:"id"`
	FaceID  string    `json:"face_id"`
	State   State     `json:"state"`
	Percent float64   `json:"percent"`
	Step    int       `json:"step"`
	Steps   int       `json:"steps"`
	Loss    float64   `json:"loss,omitempty"`
	Error   string    `json:"error,omitempty"`
	Created time.Time `json:"created_at"`
	Started time.Time `json:"started_at,omitempty"`
	Ended   time.Time `json:"ended_at,omitempty"`

	OutputPath string  `json:"-"`
	Trigger    string  `json:"trigger,omitempty"`
	SizeMB     float64 `json:"size_mb,omitempty"`
}

// Config popisuje, jak a čím se trénuje.
type Config struct {
	// Python a skript žijí na stroji s GPU. Ve vývoji se volají přes SSH,
	// v produkci poběží avatar-api přímo na Sparku a Runner bude lokální.
	PythonBin  string
	ScriptPath string
	Checkpoint string

	// SSHHost je neprázdný, když má trénink běžet vzdáleně.
	SSHHost string
	// RemoteWorkDir je adresář na stroji s GPU pro vstupní fotky.
	RemoteWorkDir string

	// LoRADir je adresář LoRA modelů ComfyUI. Hotová LoRA musí přistát
	// právě sem — LoraLoader uzel neumí absolutní cesty, hledá relativně
	// k models/loras/. Bez toho by render s user LoRA selhal.
	LoRADir string
	// LoRASubdir je podadresář uvnitř LoRADir, aby se user LoRA nemíchaly
	// se stylovými. ComfyUI podadresáře podporuje.
	LoRASubdir string

	Rank       int
	Steps      int
	LR         float64
	Resolution int

	// MaxConcurrent je tvrdý strop souběžných tréninků. Jedna GPU —
	// dva souběžné tréninky jsou pomalejší než dva po sobě a riskují OOM.
	MaxConcurrent int

	Timeout time.Duration
}

func DefaultConfig() Config {
	return Config{
		PythonBin:     "/home/ol1n/Code/ComfyUI/.venv/bin/python",
		ScriptPath:    "/home/ol1n/avatar/train_lora_sdxl.py",
		Checkpoint:    "/home/ol1n/Code/ComfyUI/models/checkpoints/sd_xl_base_1.0.safetensors",
		RemoteWorkDir: "/home/ol1n/avatar/work",
		LoRADir:       "/home/ol1n/Code/ComfyUI/models/loras",
		LoRASubdir:    "avatar",
		// rank 8, ne 16: rank 16 dá 44,5 MB, což se nevejde do storage
		// rozpočtu 30 MB/uživatel (§8.3). Plán připouští 8–16.
		Rank:          8,
		Steps:         1000,
		LR:            1e-4,
		Resolution:    768,
		MaxConcurrent: 1,
		Timeout:       30 * time.Minute,
	}
}

// Request je zadání jednoho tréninku.
type Request struct {
	JobID   string
	FaceID  string
	Images  []string // lokální cesty ke zdrojovým fotkám
	Trigger string
}

// Result je výsledek dokončeného tréninku.
type Result struct {
	// LoRAName je jméno, kterým na LoRA odkazuje ComfyUI LoraLoader —
	// relativní k models/loras/, např. "avatar/<face_id>.safetensors".
	// Tohle jde do Face.LoRAPath a odtud do grafu.
	LoRAName string
	// LoRAFile je absolutní cesta na disku. Potřebná pro mazání (GDPR).
	LoRAFile string
	Trigger  string
	SizeMB   float64
	Duration time.Duration
}

// Runner provádí tréninky. Rozhraní existuje, aby šlo v testech
// podstrčit fake a nemuselo se sahat na GPU.
type Runner interface {
	Run(ctx context.Context, req Request, onProgress func(Job)) (*Result, error)
	// DeleteLoRA smaže natrénovaný soubor. Odděleně od Run, protože
	// mazání identity může přijít kdykoliv, i dlouho po tréninku.
	DeleteLoRA(ctx context.Context, loraName string) error
}

// Queue je fronta tréninků se stropem souběžnosti.
type Queue struct {
	cfg    Config
	runner Runner

	mu   sync.RWMutex
	jobs map[string]*Job

	work chan Request
	// onDone se volá po úspěšném tréninku — tím se identita povýší
	// z face_v1 na face_v2 (§4.3).
	onDone func(faceID string, res *Result)
}

func NewQueue(cfg Config, runner Runner, onDone func(string, *Result)) *Queue {
	q := &Queue{
		cfg:    cfg,
		runner: runner,
		jobs:   map[string]*Job{},
		work:   make(chan Request, 64),
		onDone: onDone,
	}
	for i := 0; i < max(1, cfg.MaxConcurrent); i++ {
		go q.worker()
	}
	return q
}

// Submit zařadí trénink. Vrací false, když je fronta plná.
func (q *Queue) Submit(req Request) (*Job, bool) {
	job := &Job{
		ID:      req.JobID,
		FaceID:  req.FaceID,
		State:   StateQueued,
		Steps:   q.cfg.Steps,
		Trigger: req.Trigger,
		Created: time.Now(),
	}

	q.mu.Lock()
	q.jobs[job.ID] = job
	q.mu.Unlock()

	select {
	case q.work <- req:
		return job, true
	default:
		q.update(job.ID, func(j *Job) {
			j.State = StateFailed
			j.Error = "tréninková fronta je plná"
		})
		return job, false
	}
}

func (q *Queue) worker() {
	for req := range q.work {
		q.run(req)
	}
}

func (q *Queue) run(req Request) {
	q.update(req.JobID, func(j *Job) {
		j.State = StateRunning
		j.Started = time.Now()
	})

	ctx, cancel := context.WithTimeout(context.Background(), q.cfg.Timeout)
	defer cancel()

	res, err := q.runner.Run(ctx, req, func(p Job) {
		q.update(req.JobID, func(j *Job) {
			j.Percent = p.Percent
			j.Step = p.Step
			if p.Steps > 0 {
				j.Steps = p.Steps
			}
			if p.Loss > 0 {
				j.Loss = p.Loss
			}
		})
	})

	if err != nil {
		// Selhaný trénink nesmí shodit appku — jede se dál na instant
		// tracku, jen o něco hůř (§9.1 krok 3).
		slog.Error("trénink LoRA selhal, identita zůstává na face_v1",
			"job", req.JobID, "face", req.FaceID, "err", err)
		q.update(req.JobID, func(j *Job) {
			j.State = StateFailed
			j.Error = err.Error()
			j.Ended = time.Now()
		})
		return
	}

	q.update(req.JobID, func(j *Job) {
		j.State = StateDone
		j.Percent = 100
		j.OutputPath = res.LoRAFile
		j.Trigger = res.Trigger
		j.SizeMB = res.SizeMB
		j.Ended = time.Now()
	})

	slog.Info("LoRA natrénována",
		"face", req.FaceID, "duration", res.Duration,
		"size_mb", res.SizeMB, "trigger", res.Trigger)

	if q.onDone != nil {
		q.onDone(req.FaceID, res)
	}
}

// DeleteLoRA smaže natrénovanou LoRA daného jména.
func (q *Queue) DeleteLoRA(ctx context.Context, loraName string) error {
	return q.runner.DeleteLoRA(ctx, loraName)
}

func (q *Queue) update(id string, fn func(*Job)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j, ok := q.jobs[id]; ok {
		fn(j)
	}
}

func (q *Queue) Get(id string) (Job, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// ByFace najde poslední trénink dané identity.
func (q *Queue) ByFace(faceID string) (Job, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	var newest *Job
	for _, j := range q.jobs {
		if j.FaceID != faceID {
			continue
		}
		if newest == nil || j.Created.After(newest.Created) {
			newest = j
		}
	}
	if newest == nil {
		return Job{}, false
	}
	return *newest, true
}

// ---------------------------------------------------------------- runner

// ScriptRunner spouští trénovací skript. Když je nastavený SSHHost,
// jede přes SSH na stroj s GPU; jinak lokálně.
type ScriptRunner struct {
	cfg Config
}

func NewScriptRunner(cfg Config) *ScriptRunner { return &ScriptRunner{cfg: cfg} }

func (r *ScriptRunner) Run(ctx context.Context, req Request, onProgress func(Job)) (*Result, error) {
	start := time.Now()
	remote := r.cfg.SSHHost != ""

	// LoRA se ukládá rovnou do adresáře, ze kterého ji ComfyUI umí načíst.
	loraName := filepath.Join(r.cfg.LoRASubdir, req.FaceID+".safetensors")
	outPath := filepath.Join(r.cfg.LoRADir, loraName)

	images := req.Images
	if remote {
		// Fotky musí být tam, kde běží GPU.
		var err error
		images, err = r.stageRemote(ctx, req)
		if err != nil {
			return nil, err
		}
	}

	args := []string{
		r.cfg.ScriptPath,
		"--checkpoint", r.cfg.Checkpoint,
		"--output", outPath,
		"--trigger", req.Trigger,
		"--rank", fmt.Sprint(r.cfg.Rank),
		"--steps", fmt.Sprint(r.cfg.Steps),
		"--lr", fmt.Sprint(r.cfg.LR),
		"--resolution", fmt.Sprint(r.cfg.Resolution),
		"--images",
	}
	args = append(args, images...)

	var cmd *exec.Cmd
	if remote {
		cmd = exec.CommandContext(ctx, "ssh", append([]string{r.cfg.SSHHost, r.cfg.PythonBin}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, r.cfg.PythonBin, args...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spuštění trenéru: %w", err)
	}

	var result *Result
	var scriptErr string

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue // trenér občas píše i nestrukturované věci
		}

		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}

		switch rec["msg"] {
		case "progress":
			if onProgress != nil {
				onProgress(Job{
					Percent: num(rec["percent"]),
					Step:    int(num(rec["step"])),
					Steps:   int(num(rec["total"])),
					Loss:    num(rec["loss"]),
				})
			}
		case "hotovo":
			result = &Result{
				LoRAName: loraName,
				LoRAFile: str(rec["output"]),
				Trigger:  str(rec["trigger"]),
				SizeMB:   num(rec["size_mb"]),
			}
		case "chyba":
			scriptErr = str(rec["detail"])
		}
	}

	if err := cmd.Wait(); err != nil {
		if scriptErr != "" {
			return nil, fmt.Errorf("trenér selhal: %s", scriptErr)
		}
		return nil, fmt.Errorf("trenér skončil chybou: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("trenér doběhl, ale neohlásil výsledek")
	}

	result.Duration = time.Since(start)
	return result, nil
}

// stageRemote nakopíruje fotky na GPU stroj a vrátí vzdálené cesty.
func (r *ScriptRunner) stageRemote(ctx context.Context, req Request) ([]string, error) {
	dir := filepath.Join(r.cfg.RemoteWorkDir, req.FaceID)
	loraDir := filepath.Join(r.cfg.LoRADir, r.cfg.LoRASubdir)

	mk := exec.CommandContext(ctx, "ssh", r.cfg.SSHHost, "mkdir", "-p", dir, loraDir)
	if out, err := mk.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("příprava vzdálených adresářů: %w (%s)", err, out)
	}

	args := append([]string{}, req.Images...)
	args = append(args, r.cfg.SSHHost+":"+dir+"/")
	cp := exec.CommandContext(ctx, "scp", args...)
	if out, err := cp.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("kopírování fotek: %w (%s)", err, out)
	}

	remoteImgs := make([]string, len(req.Images))
	for i, p := range req.Images {
		remoteImgs[i] = filepath.Join(dir, filepath.Base(p))
	}
	return remoteImgs, nil
}

// DeleteLoRA smaže natrénovanou LoRA. Je to odvozený biometrický údaj —
// musí zmizet spolu s účtem (§12 plánu).
func (r *ScriptRunner) DeleteLoRA(ctx context.Context, loraName string) error {
	path := filepath.Join(r.cfg.LoRADir, loraName)
	if r.cfg.SSHHost != "" {
		cmd := exec.CommandContext(ctx, "ssh", r.cfg.SSHHost, "rm", "-f", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("mazání vzdálené LoRA: %w (%s)", err, out)
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

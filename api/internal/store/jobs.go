// Package store drží stav běžících renderů a jejich výsledky.
//
// Fáze 0 je čistě in-memory s TTL — stejný pattern jako gen-queue v AiStacku.
// Restart API ztratí běžící joby; recepty jsou ale uložené zvlášť, takže
// se dá zrenderovat znovu.
package store

import (
	"sync"
	"time"
)

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusError   Status = "error"
)

// Job je jeden render.
type Job struct {
	ID       string    `json:"id"`
	RecipeID string    `json:"recipe_id"`
	FaceID   string    `json:"face_id"`
	PresetID string    `json:"preset_id"`
	Status   Status    `json:"status"`
	Percent  float64   `json:"percent"`
	Stage    string    `json:"stage,omitempty"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created_at"`
	Started  time.Time `json:"started_at,omitempty"`
	Finished time.Time `json:"finished_at,omitempty"`

	// ResultPath je cesta k PNG na disku. Klient ji nedostane —
	// dostane URL, která se na ni namapuje.
	ResultPath string `json:"-"`

	// subs jsou odběratelé SSE. Každý má vlastní buffered kanál, aby
	// pomalý klient nezpomalil render ani ostatní odběratele.
	subs []chan Job
}

// Duration vrátí, jak dlouho render trval (nebo trvá).
func (j *Job) Duration() time.Duration {
	if j.Started.IsZero() {
		return 0
	}
	end := j.Finished
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(j.Started)
}

// Jobs je úložiště jobů s TTL evikcí.
type Jobs struct {
	mu   sync.RWMutex
	jobs map[string]*Job
	ttl  time.Duration
}

func NewJobs(ttl time.Duration) *Jobs {
	return &Jobs{jobs: map[string]*Job{}, ttl: ttl}
}

func (s *Jobs) Create(j *Job) {
	j.Created = time.Now()
	j.Status = StatusQueued
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
}

func (s *Jobs) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return s.snapshot(j), true
}

// snapshot vrátí kopii bez odběratelů — volající nesmí dostat kanály,
// do kterých bychom mu mohli psát za zády.
func (s *Jobs) snapshot(j *Job) Job {
	c := *j
	c.subs = nil
	return c
}

// Update aplikuje změnu a rozešle ji odběratelům.
func (s *Jobs) Update(id string, fn func(*Job)) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	fn(j)
	snap := s.snapshot(j)
	subs := append([]chan Job(nil), j.subs...)
	terminal := j.Status == StatusDone || j.Status == StatusError
	if terminal {
		// Po terminálním stavu už nic nepřijde — zavři kanály, aby SSE
		// handlery skončily a nedržely spojení donekonečna.
		j.subs = nil
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- snap:
		default: // pomalý odběratel prostě mezikrok neuvidí
		}
		if terminal {
			close(ch)
		}
	}
}

// Subscribe vrátí kanál se změnami jobu. Kanál se zavře, jakmile job
// skončí. Vrací false, když job neexistuje.
//
// Pokud už job skončil, vrátí kanál s jedním posledním stavem a zavře ho —
// klient, který se připojí pozdě, tak nezůstane viset.
func (s *Jobs) Subscribe(id string) (<-chan Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}

	ch := make(chan Job, 8)
	if j.Status == StatusDone || j.Status == StatusError {
		ch <- s.snapshot(j)
		close(ch)
		return ch, true
	}

	ch <- s.snapshot(j)
	j.subs = append(j.subs, ch)
	return ch, true
}

// Reap maže joby starší než TTL. Spouští se periodicky z main.
func (s *Jobs) Reap() int {
	cutoff := time.Now().Add(-s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for id, j := range s.jobs {
		if j.Created.Before(cutoff) {
			for _, ch := range j.subs {
				close(ch)
			}
			delete(s.jobs, id)
			n++
		}
	}
	return n
}

func (s *Jobs) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.jobs)
}

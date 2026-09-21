package face

import "testing"

func TestVersionReflectsLoRAAvailability(t *testing.T) {
	f := &Face{ID: "x", TrainState: TrainNone}
	if f.Version() != V1 {
		t.Errorf("čerstvá identita je verze %d, čekáno 1", f.Version())
	}

	// Samotná cesta k LoRA nestačí — dokud trénink neskončil stavem done,
	// soubor nemusí být kompletní.
	f.LoRAPath = "avatar/x.safetensors"
	f.TrainState = TrainRunning
	if f.Version() != V1 {
		t.Errorf("identita s běžícím tréninkem je verze %d, čekáno 1", f.Version())
	}

	f.TrainState = TrainDone
	if f.Version() != V2 {
		t.Errorf("dotrénovaná identita je verze %d, čekáno 2", f.Version())
	}
}

func TestFailedTrainingStaysV1(t *testing.T) {
	// Když trénink selže, identita musí zůstat na v1 — jinak by graf
	// odkazoval na LoRA, která neexistuje, a render by spadl.
	f := &Face{ID: "x", LoRAPath: "avatar/x.safetensors", TrainState: TrainFailed}
	if f.Version() != V1 {
		t.Fatalf("identita po selhaném tréninku je verze %d, čekáno 1", f.Version())
	}
}

func TestUpgradeSetsAllFields(t *testing.T) {
	s := NewStore()
	s.Put(&Face{ID: "abc", TrainState: TrainRunning})

	if !s.Upgrade("abc", "avatar/abc.safetensors", "ohwx person") {
		t.Fatal("Upgrade vrátil false")
	}

	f, _ := s.Get("abc")
	if f.LoRAPath != "avatar/abc.safetensors" {
		t.Errorf("LoRAPath = %q", f.LoRAPath)
	}
	if f.LoRATrigger != "ohwx person" {
		t.Errorf("LoRATrigger = %q", f.LoRATrigger)
	}
	if f.TrainState != TrainDone {
		t.Errorf("TrainState = %q, čekáno done", f.TrainState)
	}
	if f.Version() != V2 {
		t.Errorf("po Upgrade je verze %d, čekáno 2", f.Version())
	}
}

func TestUpgradeOnMissingFaceIsSafe(t *testing.T) {
	s := NewStore()
	// Uživatel může identitu smazat, zatímco trénink běží. Callback
	// pak nesmí nic vytvořit ani spadnout.
	if s.Upgrade("neexistuje", "x.safetensors", "ohwx") {
		t.Fatal("Upgrade uspěl na neexistující identitě")
	}
	if s.Count() != 0 {
		t.Fatal("Upgrade vyrobil identitu, která neexistovala")
	}
}

func TestSetTrainState(t *testing.T) {
	s := NewStore()
	s.Put(&Face{ID: "abc", TrainState: TrainNone})

	if !s.SetTrainState("abc", TrainQueued) {
		t.Fatal("SetTrainState vrátil false")
	}
	f, _ := s.Get("abc")
	if f.TrainState != TrainQueued {
		t.Errorf("TrainState = %q", f.TrainState)
	}

	if s.SetTrainState("neexistuje", TrainDone) {
		t.Error("SetTrainState uspěl na neexistující identitě")
	}
}

func TestDeleteRemovesFace(t *testing.T) {
	s := NewStore()
	s.Put(&Face{ID: "abc"})

	if !s.Delete("abc") {
		t.Fatal("Delete vrátil false")
	}
	if _, ok := s.Get("abc"); ok {
		t.Fatal("identita po smazání pořád existuje")
	}
	if s.Delete("abc") {
		t.Error("druhé Delete vrátilo true")
	}
}

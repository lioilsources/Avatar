package comfy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ol1n/avatar/api/internal/attrs"
	"github.com/ol1n/avatar/api/internal/face"
	"github.com/ol1n/avatar/api/internal/preset"
	"github.com/ol1n/avatar/api/internal/recipe"
)

func fixtures(t *testing.T) (*preset.Store, *attrs.Table) {
	t.Helper()
	data := filepath.Join("..", "..", "..", "data")
	ps, err := preset.NewStore(filepath.Join(data, "presets"), filepath.Join(data, "licenses.yaml"))
	if err != nil {
		t.Fatalf("preset store: %v", err)
	}
	tb, err := attrs.Load(filepath.Join(data, "attributes.yaml"))
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	return ps, tb
}

func buildDefault(t *testing.T, mutate func(*BuildInput)) (Graph, map[string]string) {
	t.Helper()
	ps, tb := fixtures(t)

	p, ok := ps.Get("studio-headshot")
	if !ok {
		t.Fatal("preset studio-headshot chybí")
	}

	in := BuildInput{
		Preset:   p,
		Table:    tb,
		Registry: ps.Registry(),
		Recipe: &recipe.Recipe{
			ID:    "test-recipe",
			Seed:  42,
			Attrs: attrs.Vector{},
		},
		Face:         &face.Face{ID: "test-face", RefCrop: "/tmp/x.jpg"},
		RefImageName: "ref.jpg",
	}
	if mutate != nil {
		mutate(&in)
	}

	g, hashes, err := Build(in)
	if err != nil {
		t.Fatalf("Build selhal: %v", err)
	}
	return g, hashes
}

// findNode najde první uzel daného typu.
func findNode(g Graph, classType string) (string, Node, bool) {
	for id, n := range g {
		if n.ClassType == classType {
			return id, n, true
		}
	}
	return "", Node{}, false
}

func TestBuildProducesCompleteGraph(t *testing.T) {
	g, _ := buildDefault(t, nil)

	// Bez těchhle uzlů se nic nevyrenderuje.
	for _, ct := range []string{
		"CheckpointLoaderSimple", "CLIPTextEncode", "KSampler",
		"VAEDecode", "SaveImage", "EmptyLatentImage",
	} {
		if _, _, ok := findNode(g, ct); !ok {
			t.Errorf("v grafu chybí uzel %s", ct)
		}
	}
}

func TestBuildIsSerializableToComfyAPIFormat(t *testing.T) {
	g, _ := buildDefault(t, nil)
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("graf nejde serializovat: %v", err)
	}
	// ComfyUI API formát je mapa id → {class_type, inputs}.
	var back map[string]map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("graf nejde deserializovat: %v", err)
	}
	for id, n := range back {
		if _, ok := n["class_type"]; !ok {
			t.Errorf("uzel %s nemá class_type", id)
		}
		if _, ok := n["inputs"]; !ok {
			t.Errorf("uzel %s nemá inputs", id)
		}
	}
}

// TestAllLinksPointToExistingNodes je nejcennější test v souboru: ComfyUI
// odmítne graf s odkazem na neexistující uzel až za běhu, s hláškou, ze
// které se špatně pozná příčina.
func TestAllLinksPointToExistingNodes(t *testing.T) {
	g, _ := buildDefault(t, nil)

	for id, n := range g {
		for input, val := range n.Inputs {
			arr, ok := val.([]any)
			if !ok || len(arr) != 2 {
				continue // literál, ne odkaz
			}
			target, ok := arr[0].(string)
			if !ok {
				continue
			}
			if _, exists := g[target]; !exists {
				t.Errorf("uzel %s (%s) má vstup %q odkazující na neexistující uzel %q",
					id, n.ClassType, input, target)
			}
		}
	}
}

func TestSeedReachesSampler(t *testing.T) {
	g, _ := buildDefault(t, nil)
	_, ks, ok := findNode(g, "KSampler")
	if !ok {
		t.Fatal("KSampler chybí")
	}
	if ks.Inputs["seed"] != int64(42) {
		t.Fatalf("seed = %v, čekáno 42", ks.Inputs["seed"])
	}
}

func TestIPAdapterUsesFacePlusVariant(t *testing.T) {
	g, _ := buildDefault(t, nil)

	_, loader, ok := findNode(g, "IPAdapterModelLoader")
	if !ok {
		t.Fatal("IPAdapterModelLoader chybí — identita by nefungovala")
	}
	file, _ := loader.Inputs["ipadapter_file"].(string)
	if !strings.Contains(file, "plus-face") {
		t.Errorf("IP-Adapter soubor = %q, čekána plus-face varianta", file)
	}
	// FaceID varianty jsou CC-BY-NC.
	if strings.Contains(strings.ToLower(file), "faceid") {
		t.Errorf("graf používá FaceID variantu %q — ta je licenčně zakázaná", file)
	}
}

func TestDepthPreprocessorUsesSmallCheckpoint(t *testing.T) {
	g, _ := buildDefault(t, nil)
	_, dep, ok := findNode(g, "DepthAnythingV2Preprocessor")
	if !ok {
		t.Fatal("depth preprocesor chybí")
	}
	ck, _ := dep.Inputs["ckpt_name"].(string)
	// vits = Small (Apache 2.0). vitb/vitl = Base/Large (CC-BY-NC-4.0).
	if !strings.Contains(ck, "vits") {
		t.Fatalf("depth checkpoint = %q — musí to být vits (Small), ostatní jsou CC-BY-NC", ck)
	}
}

func TestNoRefImageSkipsIdentityAndControlNet(t *testing.T) {
	// Bez referenční fotky nemá IP-Adapter ani ControlNet co zpracovat.
	// Graf musí zůstat validní, jen bez nich.
	g, _ := buildDefault(t, func(in *BuildInput) {
		in.RefImageName = ""
		in.Face = nil
	})

	if _, _, ok := findNode(g, "IPAdapterAdvanced"); ok {
		t.Error("bez referenční fotky nemá být IP-Adapter v grafu")
	}
	if _, _, ok := findNode(g, "ControlNetApplyAdvanced"); ok {
		t.Error("bez referenční fotky nemá být ControlNet v grafu")
	}
	if _, _, ok := findNode(g, "KSampler"); !ok {
		t.Error("KSampler musí zůstat i bez referenční fotky")
	}
}

// TestUserLoRAAbsentFallsBackToInstantTrack ověřuje mechanismus z §4.2:
// dokud se LoRA nedotrénuje, položka z grafu vypadne a jede se dál.
func TestUserLoRAAbsentFallsBackToInstantTrack(t *testing.T) {
	g, hashes := buildDefault(t, func(in *BuildInput) {
		in.Face = &face.Face{ID: "bez-lory", RefCrop: "/tmp/x.jpg"} // LoRAPath prázdná
	})

	for id, n := range g {
		if n.ClassType == "LoraLoader" {
			t.Errorf("uzel %s načítá LoRA, přestože user LoRA není natrénovaná", id)
		}
	}
	if _, ok := hashes["user_lora"]; ok {
		t.Error("hashes obsahují user_lora, i když LoRA neexistuje")
	}
	// Render ale musí projít.
	if _, _, ok := findNode(g, "KSampler"); !ok {
		t.Error("bez user LoRA má graf pořád fungovat")
	}
}

func TestUserLoRAPresentEntersGraph(t *testing.T) {
	g, hashes := buildDefault(t, func(in *BuildInput) {
		in.Face = &face.Face{
			ID:          "s-lorou",
			RefCrop:     "/tmp/x.jpg",
			LoRAPath:    "faces/s-lorou/lora.safetensors",
			LoRATrigger: "ohwx person",
			TrainState:  face.TrainDone,
		}
	})

	id, n, ok := findNode(g, "LoraLoader")
	if !ok {
		t.Fatal("natrénovaná user LoRA se do grafu nedostala")
	}
	if got := n.Inputs["lora_name"]; got != "faces/s-lorou/lora.safetensors" {
		t.Errorf("uzel %s načítá %v, čekána cesta k user LoRA", id, got)
	}
	if hashes["user_lora"] != "s-lorou" {
		t.Errorf("hashes[user_lora] = %q, čekáno face_id", hashes["user_lora"])
	}
}

func TestTriggerTokenReachesPrompt(t *testing.T) {
	g, _ := buildDefault(t, func(in *BuildInput) {
		in.Face = &face.Face{
			ID: "s-lorou", RefCrop: "/tmp/x.jpg",
			LoRAPath: "x.safetensors", LoRATrigger: "ohwx",
			TrainState: face.TrainDone,
		}
	})
	pos := positivePrompt(t, g)
	if !strings.Contains(pos, "ohwx") {
		t.Fatalf("trigger token není v promptu: %q", pos)
	}
}

func TestNoTriggerLeavesNoPlaceholderOrDoubleSpace(t *testing.T) {
	g, _ := buildDefault(t, nil) // face bez LoRA
	pos := positivePrompt(t, g)

	if strings.Contains(pos, "{") || strings.Contains(pos, "}") {
		t.Errorf("v promptu zůstal nerozvinutý placeholder: %q", pos)
	}
	if strings.Contains(pos, "  ") {
		t.Errorf("prompt obsahuje zdvojenou mezeru po prázdném placeholderu: %q", pos)
	}
	if strings.Contains(pos, ", ,") || strings.HasPrefix(pos, ", ") {
		t.Errorf("prompt má prázdnou položku: %q", pos)
	}
}

func TestNSFWLockExpandsInNegative(t *testing.T) {
	g, _ := buildDefault(t, nil)
	neg := negativePrompt(t, g)

	if strings.Contains(neg, "{nsfw_lock}") {
		t.Fatal("{nsfw_lock} zůstal nerozvinutý v negativním promptu")
	}
	for _, term := range []string{"nsfw", "child", "minor"} {
		if !strings.Contains(neg, term) {
			t.Errorf("negativní prompt neobsahuje %q: %q", term, neg)
		}
	}
}

func TestAttributesReachPositivePrompt(t *testing.T) {
	g, _ := buildDefault(t, func(in *BuildInput) {
		in.Recipe.Attrs = attrs.Vector{"eye_color": "blue", "mood": "smiling"}
	})
	pos := positivePrompt(t, g)
	if !strings.Contains(pos, "blue") {
		t.Errorf("atribut eye_color se nedostal do promptu: %q", pos)
	}
	if !strings.Contains(pos, "smile") {
		t.Errorf("atribut mood se nedostal do promptu: %q", pos)
	}
}

func TestBlockedCheckpointFailsBuild(t *testing.T) {
	ps, tb := fixtures(t)
	p, _ := ps.Get("studio-headshot")

	// Kopie presetu s licenčně zakázaným checkpointem — build musí spadnout,
	// ne vyrobit graf, který by se pak v klidu vyrenderoval.
	bad := *p
	bad.Checkpoint = "juggernaut_xl_v9"

	_, _, err := Build(BuildInput{
		Preset:   &bad,
		Table:    tb,
		Registry: ps.Registry(),
		Recipe:   &recipe.Recipe{ID: "x", Seed: 1, Attrs: attrs.Vector{}},
	})
	if err == nil {
		t.Fatal("build s blocked checkpointem prošel")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("chyba nezmiňuje důvod: %v", err)
	}
}

func TestClampEndPreventsSilentlyDisabledConditioning(t *testing.T) {
	// end == 0 by vliv úplně vypnulo a render by "uspěl" s tichým nesmyslem.
	if got := clampEnd(0); got != 1.0 {
		t.Errorf("clampEnd(0) = %v, čekáno 1.0", got)
	}
	if got := clampEnd(0.9); got != 0.9 {
		t.Errorf("clampEnd(0.9) = %v, čekáno 0.9", got)
	}
}

func TestPreviewPresetUsesLightningLoRA(t *testing.T) {
	ps, tb := fixtures(t)
	p, ok := ps.Get("studio-headshot-preview")
	if !ok {
		t.Skip("preview preset není")
	}

	g, _, err := Build(BuildInput{
		Preset:       p,
		Table:        tb,
		Registry:     ps.Registry(),
		Recipe:       &recipe.Recipe{ID: "x", Seed: 1, Attrs: attrs.Vector{}},
		Face:         &face.Face{ID: "f", RefCrop: "/tmp/x.jpg"},
		RefImageName: "ref.jpg",
	})
	if err != nil {
		t.Fatalf("build preview presetu selhal: %v", err)
	}

	_, n, ok := findNode(g, "LoraLoader")
	if !ok {
		t.Fatal("preview preset nemá akcelerační LoRA")
	}
	if name, _ := n.Inputs["lora_name"].(string); !strings.Contains(name, "lightning") {
		t.Errorf("preview LoRA = %q, čekána lightning", name)
	}

	_, ks, _ := findNode(g, "KSampler")
	if steps, _ := ks.Inputs["steps"].(int); steps > 10 {
		t.Errorf("preview má %d kroků — na 1–2 s je to moc", steps)
	}
}

func TestTidyCollapsesEmptyFragments(t *testing.T) {
	cases := map[string]string{
		"a,  , b":    "a, b",
		"  a  b , c": "a b, c",
		", a,":       "a",
		"a,,b":       "a, b",
	}
	for in, want := range cases {
		if got := tidy(in); got != want {
			t.Errorf("tidy(%q) = %q, čekáno %q", in, got, want)
		}
	}
}

// --- pomocné ---------------------------------------------------------------

func positivePrompt(t *testing.T, g Graph) string { return promptByTitle(t, g, "positive") }
func negativePrompt(t *testing.T, g Graph) string { return promptByTitle(t, g, "negative") }

func promptByTitle(t *testing.T, g Graph, title string) string {
	t.Helper()
	for _, n := range g {
		if n.ClassType == "CLIPTextEncode" && n.Meta != nil && n.Meta.Title == title {
			s, _ := n.Inputs["text"].(string)
			return s
		}
	}
	t.Fatalf("uzel s promptem %q nenalezen", title)
	return ""
}

func TestMapNodeProgressIsMonotonicAcrossNodes(t *testing.T) {
	const sampler = SamplerNodeID

	// Přípravný uzel nikdy nepřeleze vyhrazený podíl — jinak by ukazatel
	// vyskočil na 100 % ještě před začátkem samplingu.
	if got := mapNodeProgress("50", sampler, 1.0); got > preSamplerBudget*100+0.001 {
		t.Errorf("přípravný uzel na 100 %% dal %v %%, strop je %v %%", got, preSamplerBudget*100)
	}

	// Sampling začíná tam, kde příprava skončila, ne od nuly.
	start := mapNodeProgress(sampler, sampler, 0)
	if start < preSamplerBudget*100-0.001 {
		t.Errorf("sampler začíná na %v %%, čekáno aspoň %v %%", start, preSamplerBudget*100)
	}
	if end := mapNodeProgress(sampler, sampler, 1.0); end < 99.9 {
		t.Errorf("sampler na konci dal %v %%, čekáno 100 %%", end)
	}

	// Celá sekvence musí být neklesající.
	var prev float64
	for _, step := range []struct {
		node string
		frac float64
	}{
		{"50", 0.0}, {"50", 0.5}, {"50", 1.0},
		{sampler, 0.0}, {sampler, 0.25}, {sampler, 0.5}, {sampler, 0.75}, {sampler, 1.0},
	} {
		got := mapNodeProgress(step.node, sampler, step.frac)
		if got < prev {
			t.Fatalf("postup couvl z %v %% na %v %% (uzel %s, frac %v)", prev, got, step.node, step.frac)
		}
		prev = got
	}
}

func TestMapNodeProgressClampsOutOfRange(t *testing.T) {
	if got := mapNodeProgress(SamplerNodeID, SamplerNodeID, -0.5); got < 0 {
		t.Errorf("záporný zlomek dal %v %%", got)
	}
	if got := mapNodeProgress(SamplerNodeID, SamplerNodeID, 1.7); got > 100.001 {
		t.Errorf("zlomek nad 1 dal %v %%", got)
	}
}

func TestSamplerNodeIDMatchesGraph(t *testing.T) {
	// Konstanta a graf se musí shodovat — jinak by mapování postupu
	// tiše přestalo fungovat a nikdo by si toho nevšiml.
	g, _ := buildDefault(t, nil)
	n, ok := g[SamplerNodeID]
	if !ok {
		t.Fatalf("uzel %q v grafu není", SamplerNodeID)
	}
	if n.ClassType != "KSampler" {
		t.Fatalf("uzel %q je %q, čekán KSampler", SamplerNodeID, n.ClassType)
	}
}

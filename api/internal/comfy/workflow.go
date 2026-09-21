package comfy

import (
	"fmt"
	"strings"

	"github.com/ol1n/avatar/api/internal/attrs"
	"github.com/ol1n/avatar/api/internal/face"
	"github.com/ol1n/avatar/api/internal/preset"
	"github.com/ol1n/avatar/api/internal/recipe"
)

// Node je jeden uzel ComfyUI grafu v API formátu.
type Node struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
	Meta      *NodeMeta      `json:"_meta,omitempty"`
}

type NodeMeta struct {
	Title string `json:"title"`
}

// Graph je celý workflow — mapa id uzlu na uzel.
type Graph map[string]Node

// SamplerNodeID je id uzlu KSampler v grafu, který staví Build.
//
// Watch ho potřebuje, aby uměl odlišit postup samplingu (který zabere
// většinu času) od postupu pomocných uzlů. Bez toho by ukazatel průběhu
// couval — každý uzel hlásí vlastních 0–100 %.
const SamplerNodeID = "81"

// link odkazuje na výstup jiného uzlu: [id_uzlu, index_výstupu].
func link(nodeID string, slot int) []any { return []any{nodeID, slot} }

// nsfwNegative je sdílený blok, který se dosadí za {nsfw_lock}.
// Drží se v kódu, ne v presetu, aby ho nešlo omylem oslabit editací dat.
const nsfwNegative = "nsfw, nude, naked, nudity, explicit, sexual, lingerie, underwear, child, minor, loli"

// qualityFragment je společný ocásek pro {quality}.
const qualityFragment = "highly detailed, sharp focus, natural skin texture, 85mm lens"

// BuildInput je všechno, co builder potřebuje.
type BuildInput struct {
	Preset   *preset.Preset
	Recipe   *recipe.Recipe
	Face     *face.Face
	Table    *attrs.Table
	Registry *preset.LicenseRegistry

	// RefImageName je jméno souboru, který už byl nahraný do ComfyUI
	// přes /upload/image — ne cesta na náš disk.
	RefImageName string
}

// Build sestaví ComfyUI graf pro base render (§3 krok [4]).
//
// Struktura grafu:
//
//	checkpoint ─┬─ LoRA řetěz ─┬─ IPAdapter (identita) ─ KSampler ─ VAEDecode ─ SaveImage
//	            │              │                            │
//	            └─ CLIP ─ pozitivní/negativní prompt ────────┤
//	                                                        │
//	     ref foto ─ DepthAnythingV2 ─ ControlNet ────────────┘
//
// Vrací graf a mapu hashů modelů pro záznam do receptu.
func Build(in BuildInput) (Graph, map[string]string, error) {
	p := in.Preset
	g := Graph{}
	hashes := map[string]string{}

	ck, err := in.Registry.ResolveCheckpoint(p.Checkpoint)
	if err != nil {
		return nil, nil, err
	}
	hashes["checkpoint"] = ck.Key

	res := p.Res()

	// --- checkpoint -------------------------------------------------------
	g["1"] = Node{
		ClassType: "CheckpointLoaderSimple",
		Inputs:    map[string]any{"ckpt_name": ck.File},
		Meta:      &NodeMeta{Title: "checkpoint"},
	}
	modelOut, clipOut, vaeOut := link("1", 0), link("1", 1), link("1", 2)

	// --- LoRA řetěz -------------------------------------------------------
	// Každá LoRA přebírá model i CLIP od předchozí. Položky, které nejde
	// vyřešit a jsou optional, tiše vypadnou — to je mechanismus, díky
	// kterému appka jede na instant tracku, dokud se user LoRA nedotrénuje.
	nextID := 10
	for _, l := range p.LoRAs {
		var file string

		if l.Name == preset.UserLoRAPlaceholder {
			if in.Face == nil || in.Face.LoRAPath == "" {
				continue // ještě není natrénovaná — instant track
			}
			file = in.Face.LoRAPath
			hashes["user_lora"] = in.Face.ID
		} else {
			ref, err := in.Registry.ResolveLoRA(l.Name)
			if err != nil {
				if l.Optional {
					continue
				}
				return nil, nil, err
			}
			file = ref.File
			hashes["lora:"+ref.Key] = ref.Key
		}

		id := fmt.Sprintf("%d", nextID)
		nextID++
		g[id] = Node{
			ClassType: "LoraLoader",
			Inputs: map[string]any{
				"lora_name":      file,
				"strength_model": l.Weight,
				"strength_clip":  l.Weight,
				"model":          modelOut,
				"clip":           clipOut,
			},
			Meta: &NodeMeta{Title: "lora " + l.Name},
		}
		modelOut, clipOut = link(id, 0), link(id, 1)
	}

	// --- prompty ----------------------------------------------------------
	positive, negative := renderPrompts(p, in.Recipe, in.Face, in.Table)

	g["20"] = Node{
		ClassType: "CLIPTextEncode",
		Inputs:    map[string]any{"text": positive, "clip": clipOut},
		Meta:      &NodeMeta{Title: "positive"},
	}
	g["21"] = Node{
		ClassType: "CLIPTextEncode",
		Inputs:    map[string]any{"text": negative, "clip": clipOut},
		Meta:      &NodeMeta{Title: "negative"},
	}
	posOut, negOut := link("20", 0), link("21", 0)

	// --- referenční fotka -------------------------------------------------
	hasRef := in.RefImageName != ""
	if hasRef {
		g["30"] = Node{
			ClassType: "LoadImage",
			Inputs:    map[string]any{"image": in.RefImageName},
			Meta:      &NodeMeta{Title: "ref foto"},
		}
	}

	// --- identita přes IP-Adapter ----------------------------------------
	// Vždy plus-face varianta z Apache repa h94/IP-Adapter. FaceID varianty
	// jsou CC-BY-NC (závisí na InsightFace) a resolver je nezná.
	if p.Identity.Method == "ipadapter" && hasRef {
		ipa, err := in.Registry.ResolveIPAdapter()
		if err != nil {
			return nil, nil, err
		}
		cv, err := in.Registry.ResolveCLIPVision()
		if err != nil {
			return nil, nil, err
		}
		hashes["ipadapter"] = ipa.Key

		g["40"] = Node{
			ClassType: "IPAdapterModelLoader",
			Inputs:    map[string]any{"ipadapter_file": ipa.File},
		}
		g["41"] = Node{
			ClassType: "CLIPVisionLoader",
			Inputs:    map[string]any{"clip_name": cv.File},
		}
		g["42"] = Node{
			ClassType: "IPAdapterAdvanced",
			Inputs: map[string]any{
				"model":          modelOut,
				"ipadapter":      link("40", 0),
				"image":          link("30", 0),
				"clip_vision":    link("41", 0),
				"weight":         p.Identity.Weight,
				"weight_type":    "linear",
				"combine_embeds": "concat",
				"start_at":       p.Identity.Start,
				"end_at":         clampEnd(p.Identity.End),
				"embeds_scaling": "V only",
			},
			Meta: &NodeMeta{Title: "identita"},
		}
		modelOut = link("42", 0)
	}

	// --- ControlNet -------------------------------------------------------
	// Depth drží pozici a proporce hlavy. Preprocesor běží na referenční
	// fotce; ve Fázi 2 sem místo ní půjde warpnutá varianta (§3 krok [3]).
	cnID := 50
	for _, cn := range p.ControlNet {
		if !hasRef {
			break
		}
		ref, err := in.Registry.ResolveControlNet(cn.Type)
		if err != nil {
			return nil, nil, err
		}
		hashes["controlnet:"+cn.Type] = ref.Key

		hintID := fmt.Sprintf("%d", cnID)
		applyID := fmt.Sprintf("%d", cnID+1)
		loaderID := fmt.Sprintf("%d", cnID+2)
		cnID += 3

		hint, err := preprocessorNode(cn.Type, link("30", 0), res)
		if err != nil {
			return nil, nil, err
		}
		g[hintID] = hint

		g[loaderID] = Node{
			ClassType: "ControlNetLoader",
			Inputs:    map[string]any{"control_net_name": ref.File},
		}
		g[applyID] = Node{
			ClassType: "ControlNetApplyAdvanced",
			Inputs: map[string]any{
				"positive":      posOut,
				"negative":      negOut,
				"control_net":   link(loaderID, 0),
				"image":         link(hintID, 0),
				"strength":      cn.Weight,
				"start_percent": cn.Start,
				"end_percent":   clampEnd(cn.End),
				"vae":           vaeOut,
			},
			Meta: &NodeMeta{Title: "controlnet " + cn.Type},
		}
		posOut, negOut = link(applyID, 0), link(applyID, 1)
	}

	// --- latent + sampler + výstup ---------------------------------------
	g["80"] = Node{
		ClassType: "EmptyLatentImage",
		Inputs:    map[string]any{"width": res.Width, "height": res.Height, "batch_size": 1},
	}
	g[SamplerNodeID] = Node{
		ClassType: "KSampler",
		Inputs: map[string]any{
			"model":        modelOut,
			"positive":     posOut,
			"negative":     negOut,
			"latent_image": link("80", 0),
			"seed":         in.Recipe.Seed,
			"steps":        p.Sampler.Steps,
			"cfg":          p.Sampler.CFG,
			"sampler_name": p.Sampler.Name,
			"scheduler":    p.Sampler.Scheduler,
			"denoise":      1.0,
		},
		Meta: &NodeMeta{Title: "sampler"},
	}
	g["82"] = Node{
		ClassType: "VAEDecode",
		Inputs:    map[string]any{"samples": link(SamplerNodeID, 0), "vae": vaeOut},
	}
	g["83"] = Node{
		ClassType: "SaveImage",
		Inputs:    map[string]any{"images": link("82", 0), "filename_prefix": "avatar/" + in.Recipe.ID},
		Meta:      &NodeMeta{Title: "výstup"},
	}

	return g, hashes, nil
}

// preprocessorNode vrátí uzel, který z fotky spočítá hint pro ControlNet.
func preprocessorNode(kind string, image []any, res preset.Resolution) (Node, error) {
	// Preprocesor jede na kratší straně výstupu — vyšší rozlišení hintu
	// nic nepřidá a jen zdržuje.
	side := res.Width
	if res.Height < side {
		side = res.Height
	}

	switch kind {
	case "depth":
		return Node{
			ClassType: "DepthAnythingV2Preprocessor",
			Inputs: map[string]any{
				"image": image,
				// JEDINÁ licenčně čistá varianta — Base/Large/Giant jsou
				// CC-BY-NC-4.0, jen Small je Apache 2.0.
				"ckpt_name":  "depth_anything_v2_vits.pth",
				"resolution": side,
			},
			Meta: &NodeMeta{Title: "depth hint"},
		}, nil
	case "softedge":
		return Node{
			ClassType: "PiDiNetPreprocessor",
			Inputs:    map[string]any{"image": image, "safe": "enable", "resolution": side},
			Meta:      &NodeMeta{Title: "softedge hint"},
		}, nil
	case "openpose":
		return Node{
			ClassType: "OpenposePreprocessor",
			Inputs: map[string]any{
				"image": image, "detect_hand": "disable",
				"detect_body": "enable", "detect_face": "enable", "resolution": side,
			},
			Meta: &NodeMeta{Title: "pose hint"},
		}, nil
	default:
		return Node{}, fmt.Errorf("pro typ controlnetu %q není preprocesor", kind)
	}
}

// clampEnd hlídá, že end nikdy nespadne na 0 — to by celý vliv vypnulo
// a chybu by nikdo nenašel, protože render proběhne "úspěšně".
func clampEnd(v float64) float64 {
	if v <= 0 {
		return 1.0
	}
	return v
}

// renderPrompts dosadí do šablon presetu atributy, trigger token a NSFW zámek.
func renderPrompts(p *preset.Preset, r *recipe.Recipe, f *face.Face, t *attrs.Table) (string, string) {
	var attrText string
	if t != nil && len(r.Attrs) > 0 {
		attrText = attrs.Join(t.PromptFragments(r.Attrs, p.PromptDialect))
	}

	// Trigger token user LoRA. Bez natrénované LoRA je prázdný —
	// šablona s ním počítá a zbylé mezery se stáhnou níž.
	var trigger string
	if f != nil && f.LoRATrigger != "" && f.LoRAPath != "" {
		trigger = f.LoRATrigger
	}

	subject := "a person"

	pos := p.PromptTemplate
	pos = strings.ReplaceAll(pos, "{subject}", subject)
	pos = strings.ReplaceAll(pos, "{attrs}", attrText)
	pos = strings.ReplaceAll(pos, "{quality}", qualityFragment)
	pos = strings.ReplaceAll(pos, "{trigger}", trigger)

	neg := p.NegativeTemplate
	if p.NSFWNegativeLock {
		neg = strings.ReplaceAll(neg, "{nsfw_lock}", nsfwNegative)
	} else {
		neg = strings.ReplaceAll(neg, "{nsfw_lock}", "")
	}

	return tidy(pos), tidy(neg)
}

// tidy odstraní prázdné položky a zdvojené mezery, které vzniknou, když se
// placeholder rozvine na prázdný řetězec.
func tidy(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Join(strings.Fields(p), " ")
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

# Fáze 1 — LoRA track

Plán označuje podobnost bez InstantID za **hlavní technické riziko celého
návrhu** (§12) a chce ji změřit právě tady, ne později. Tenhle dokument je
ten záznam.

---

## Co se postavilo

| část | kde |
|---|---|
| trenér SDXL LoRA | `scripts/train_lora_sdxl.py` |
| měření podobnosti | `scripts/eval_similarity.py` |
| fronta `q:train` | `api/internal/train/` |
| `face_v1` → `face_v2` | `api/internal/face/face.go` (`Upgrade`) |
| rerender z receptu | `POST /v1/renders/rerender` |
| UI stav tréninku | `app/lib/screens/result_screen.dart` (`_TrainNotice`) |

### Proč vlastní trenér a ne kohya

Plán připouští obojí („kohya nebo vlastní"). Ve venv ComfyUI na Sparku už
jsou `diffusers 0.37` + `peft 0.20` na torch 2.11/CUDA 13, tedy verze, které
na aarch64/Blackwell prokazatelně běží. Postavit vedle toho kohya sd-scripts
s vlastním stromem závislostí je větší riziko než dvě stě řádků, které dělají
přesně to, co potřebujeme.

Zásadní pro rychlost: **latenty z VAE a textové embeddingy se předpočítají
jednou dopředu**. Každý krok je pak jen forward/backward přes UNet. Bez toho
by trénink trval násobně déle.

### Formát výstupu

ComfyUI čte LoRA klíče ve tvaru `lora_unet_<diffusers cesta s podtržítky>`
(viz `comfy/lora.py`, `model_lora_keys_unet`). Trenér proto převádí peft
klíče na kohya formát. Ověřeno tím, že ComfyUI po načtení nevypíše **ani
jedno** `lora key not loaded`.

Dva detaily, které stály čas a stojí za zapsání:

- **Textové enkodéry se musí ručně přesunout na GPU.** `encode_prompt(device=…)`
  přesune vstupy, ale ne váhy — jinak to spadne na „tensors on different devices".
- **Každá `.alpha` musí být samostatný tensor.** Sdílení jednoho objektu napříč
  klíči safetensors odmítne uložit („some tensors share memory").

---

## Naměřeno

### Trénink

Konfigurace: 4 fotky, rank 16, alpha 16, 1000 kroků, LR 1e-4 (OneCycle),
rozlišení 768, batch 1, bf16, AdamW.

| | hodnota |
|---|---|
| načtení modelu | 4,2 s |
| předpočet latentů + embeddingů | 6,5 s |
| trénink 1000 kroků | **511 s** (0,51 s/krok) |
| celkem | **523 s = 8,7 min** |
| velikost výstupu | **44,5 MB** (1680 tensorů, 23,2 M parametrů) |

**Dva cíle z plánu nesplněny:**

1. **Čas: 8,7 min proti cíli 2–5 min.** Měřeno při souběžném běhu jiné úlohy
   na téže GPU (projekt Cards), takže na volné GPU to bude rychlejší — ale
   nezměřeno v klidu, takže netvrdím o kolik.
2. **Velikost: 44,5 MB proti cíli 10–30 MB.** Tohle není o zátěži GPU, je to
   přímý důsledek `rank 16`. Storage rozpočet v §8.3 počítá s 30 MB × N
   uživatelů, takže rank 16 se do něj nevejde.

**Doporučení: výchozí rank snížit na 8.** Plán připouští 8–16 a rank 8 dá
~22 MB, což do rozpočtu sedí. Vliv na podobnost je potřeba změřit — dokud
se to nezměří, není to rozhodnutí, jen hypotéza.

### Podobnost

**Neprůkazné — DoD „podobnost měřitelně lepší než instant track" NENÍ splněné.**
Podrobně v `docs/faze1-mereni.md`. Shrnutí:

- infrastruktura měření stojí a je licenčně čistá (DINOv2 + CLIP, ne InsightFace)
- první měření je metodologicky vadné: referenční sadu jsem vyrobil
  IP-Adapterem, tedy jednou ze dvou porovnávaných metod
- metriky si odporují (DINOv2 favorizuje instant, CLIP kombinaci)
- vizuální kontrola našla konkrétní vadu: **LoRA samotná uletí s barvou vlasů**
- kombinace IP-Adapter + LoRA vypadá nejlíp — plán předpokládal, že se obě
  metody doplňují, a tohle to slabě podporuje

---

## Poznámka k metrice podobnosti

Správný nástroj je ArcFace embedding. Jenže ArcFace váhy chodí přes
InsightFace a ty jsou non-komerční — přesně to, kvůli čemu celý plán
nepoužívá InstantID. Mít takovou závislost v repu jen kvůli internímu
měření by porušovalo vlastní pravidla projektu.

Používáme proto dvě licenčně čisté náhrady:

- **DINOv2** (Apache 2.0) — self-supervised ViT, na instance-level podobnost
  výrazně lepší než CLIP. Hlavní číslo.
- **CLIP** (MIT) — sémantická podobnost, doplňkové číslo.

**Ani jedno není face recognition.** Absolutní hodnoty neříkají „podobnost je
87 %". Co říkají, je relativní rozdíl mezi dvěma variantami téhož pipeline —
a to je přesně otázka Fáze 1: je LoRA track měřitelně lepší než instant track?

Proto se počítá i **podlaha** (nesouvisející obličeje) a **strop** (jak podobné
jsou si navzájem samotné referenční fotky). Bez nich je jakékoliv číslo
neinterpretovatelné.

---

## Fallback při selhání

Plán (§9.1 krok 3) vyžaduje, aby selhaný trénink nezablokoval nic. Ověřeno
testy v `api/internal/train/train_test.go`:

- selhaný trénink **nepovýší** identitu na v2 (`TestFailedTrainingDoesNotUpgrade`) —
  jinak by graf odkazoval na LoRA, která neexistuje, a padal by každý render
- `Face.Version()` vrací v1, dokud `TrainState != done`, i když je `LoRAPath`
  vyplněná (`TestFailedTrainingStaysV1`)
- builder grafu položku `$user_lora` tiše vypustí, když LoRA není
  (`TestUserLoRAAbsentFallsBackToInstantTrack`)
- UI selhaný trénink uživateli **neukazuje** — appka funguje dál, jen o něco
  hůř, a není co hlásit

Strop souběžnosti tréninků je 1 (`TestMaxConcurrentIsRespected`): jedna GPU,
dva souběžné tréninky jsou pomalejší než dva po sobě a riskují OOM.

---

## GDPR

User LoRA je **odvozený biometrický údaj** (§12) a žije v adresáři modelů
ComfyUI, ne v našem storage. `DELETE /v1/faces/{id}` ji proto maže přes
runner, který ví, jestli je to lokálně nebo přes SSH. Když se smazání
nepovede, endpoint vrátí 500 a zaloguje to jako chybu — nesmí se tvářit
jako úspěch.

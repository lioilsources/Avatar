# Face Avatar Studio

Flutter (Android + iOS) + Go API + ComfyUI na DGX Sparku.
Plán a rozhodnutí: [`AVATAR_PLAN.md`](AVATAR_PLAN.md).

**Stav: Fáze 0 + Fáze 1 (LoRA track) hotové a ověřené na reálné GPU.**

```
foto → quality gate on-device → upload ─┬─ instant track: SDXL + IP-Adapter + depth ControlNet → portrét (sekundy)
                                        │
                                        └─ na pozadí: trénink user LoRA → face_v2 → rerender receptů (minuty)
```

---

## Struktura

```
data/                     datové soubory — na těch stojí všechno ostatní
  licenses.yaml           licenční registr: co se smí použít (vynucené v kódu)
  attributes.yaml         mapovací tabulka atributů (vrstvy geometry/mask/prompt)
  presets/_schema.json    JSON schema presetu
  presets/*.json          presety
  workflows/*.api.json    vygenerované referenční ComfyUI grafy (make workflows)

api/                      Go backend (chi)
  cmd/avatar-api          vstupní bod
  internal/preset         preset store, hot-reload, licenční vynucení
  internal/attrs          atributová tabulka, překlad na prompt fragmenty
  internal/comfy          ComfyUI klient + builder workflow grafu
  internal/face           identita (face_v1 / face_v2)
  internal/train          fronta q:train, trénink LoRA, mazání (GDPR)
  internal/recipe         recept jako objekt první třídy
  internal/store          joby a výsledky (in-memory, TTL)
  tools/dumpworkflow      export grafů do data/workflows

scripts/
  train_lora_sdxl.py      trenér per-user LoRA (běží na Sparku)
  face_crop.py            ořez na obličej přes MediaPipe (Apache 2.0)
  eval_similarity.py      měření podobnosti (DINOv2 + CLIP, licenčně čisté)

app/                      Flutter
  lib/services/quality_gate.dart   kontrola fotky před uploadem
  lib/services/api_client.dart     REST + SSE
  lib/screens/                     upload + výsledek
```

---

## Rozjetí

ComfyUI běží host-native na DGX Sparku (`spark:8188`, mimo Docker).

```bash
# terminál 1 — tunel na ComfyUI
cd api && make tunnel

# terminál 2 — API
cd api && make run          # :8092

# terminál 3 — appka
cd app && flutter run --dart-define=AVATAR_API_URL=http://<ip-mac>:8092
```

Kontrola, že to žije:

```bash
curl -s localhost:8092/health | jq
```

`status: degraded` znamená, že API běží, ale ComfyUI není vidět — zkontroluj tunel.

### Konfigurace

| proměnná | výchozí | popis |
|---|---|---|
| `AVATAR_LISTEN_ADDR` | `:8092` | |
| `AVATAR_DATA_DIR` | `../data` | presety, atributy, licence |
| `AVATAR_STORAGE_DIR` | `./storage` | fotky a výsledky |
| `AVATAR_COMFY_URL` | `http://127.0.0.1:8188` | |
| `AVATAR_RENDER_WORKERS` | `2` | strop souběžných renderů |
| `AVATAR_MAX_PHOTOS` | `5` | |
| `AVATAR_STRICT_LICENSE` | (zapnuto) | `0` odemkne presety s tierem `verify` — **jen dev** |
| `AVATAR_TRAIN_SSH_HOST` | (prázdné) | host s GPU pro trénink; prázdné = lokálně |
| `AVATAR_TRAIN_RANK` | `8` | rank LoRA (8 ≈ 22 MB, 16 ≈ 44 MB) |
| `AVATAR_TRAIN_STEPS` | `1000` | kroků tréninku |
| `AVATAR_TRAIN_RESOLUTION` | `768` | rozlišení tréninku |

---

## API

| metoda | cesta | |
|---|---|---|
| `GET` | `/health` | stav včetně dostupnosti ComfyUI |
| `GET` | `/v1/presets` | katalog — filtrovaný podle licenčního tieru |
| `GET` | `/v1/attributes` | atributová tabulka pro UI |
| `POST` | `/v1/faces` | multipart `photos` → `face_id` |
| `GET` | `/v1/faces/{id}` | |
| `DELETE` | `/v1/faces/{id}` | maže i odvozená data včetně LoRA (GDPR) |
| `POST` | `/v1/faces/{id}/train` | spustí trénink LoRA na pozadí |
| `GET` | `/v1/faces/{id}/train` | stav tréninku + verze identity |
| `POST` | `/v1/renders` | `{face_id, preset_id, attrs, seed?}` → `job_id` |
| `GET` | `/v1/renders/{id}` | stav |
| `GET` | `/v1/renders/{id}/events` | SSE průběh |
| `GET` | `/v1/renders/{id}/result` | PNG |
| `POST` | `/v1/renders/rerender` | zrenderuje uložený recept znovu (po dotrénování LoRA) |
| `GET` | `/v1/recipes/{id}` | recept + fingerprint |

---

## Naměřeno na GB10 (2026-08-06)

Časy jsou vlastní měřítko ComfyUI (`execution_start` → `execution_success`),
tedy bez čekání ve frontě.

| konfigurace | čas |
|---|---|
| holý SDXL, 8 kroků, 768 | 4,6 s |
| holý SDXL, 28 kroků, 1024 | 13,6 s |
| **preview profil** (8 kroků, 768, IP-Adapter + depth + Lightning) | **12,6 s** |
| **final profil** (28 kroků, 1024, IP-Adapter + depth) | **21,4 s** |

Identita drží — výsledek je poznatelně tentýž člověk.

**Dvě věci k dořešení, obě mimo rozsah Fáze 0:**

1. **Final je 21,4 s proti cíli < 20 s.** Měřeno při souběžném běhu jiné úlohy
   na téže GPU (projekt Cards), takže na volné GPU to nejspíš projde — ale
   nebylo to změřeno v klidu, takže to netvrdím.

2. **Identita + ControlNet stojí fixních ~8 s** bez ohledu na rozlišení
   (21,4 − 13,6 na 1024; 12,6 − 4,6 na 768). To je hlavní kandidát na
   optimalizaci: **depth hint závisí jen na referenční fotce**, ne na presetu
   ani atributech, takže se dá spočítat jednou při zakládání identity
   a cachovat. Preview profil je kvůli tomu 12,6 s místo cílových 1–2 s (§8.4).

---

## Co je hotové a co ne

### Fáze 1 — trénink LoRA

Konfigurace: 4 fotky, rank 16, 1000 kroků, 768 px, bf16.

| | hodnota | cíl plánu |
|---|---|---|
| trénink | **523 s = 8,7 min** | 2–5 min ❌ |
| velikost | **44,5 MB** | 10–30 MB ❌ |

Výchozí rank snížen na 8 (≈ 22 MB), aby se vešel do storage rozpočtu
30 MB/uživatel. Čas je opět měřen při souběžné zátěži GPU.

**Podobnost: DoD nesplněné.** Měření je postavené a licenčně čisté, ale první
výsledek je metodologicky vadný — referenční sadu jsem vyrobil IP-Adapterem,
tedy jednou ze dvou porovnávaných metod, takže srovnání je předem nakloněné.
Podrobně i s tím, co je potřeba k platnému měření: [`docs/faze1-mereni.md`](docs/faze1-mereni.md).

Konkrétní nález z vizuální kontroly: **LoRA samotná uletí s barvou vlasů**;
kombinace IP-Adapter + LoRA vypadá nejlíp. To podporuje předpoklad plánu,
že se obě metody doplňují (§4).

**Hotové (Fáze 1):**
- vlastní SDXL LoRA trenér (diffusers + peft), výstup v kohya formátu pro ComfyUI
- oddělená fronta `q:train` se stropem souběžnosti 1
- `face_v1` → `face_v2` upgrade, rerender receptů s lepší identitou
- fallback: selhaný trénink identitu nepovýší, appka jede dál na instant tracku
- mazání LoRA jako součást mazání identity (GDPR)
- licenčně čisté měření podobnosti (DINOv2 Apache + CLIP MIT — ne InsightFace)

**Hotové (Fáze 0):**
- licenční registr vynucený v resolveru + testy, že blocked modely neprojdou
- preset store se schema validací a hot-reloadem
- atributová tabulka se všemi třemi vrstvami popsanými
- ComfyUI graf: SDXL base + IP-Adapter plus-face + depth ControlNet
- recepty s otiskem, SSE průběh, mazání identity včetně LoRA
- Flutter: on-device quality gate (rozlišení, Laplacian ostrost), upload, výsledek

**Není (podle plánu později):**
- MediaPipe analýza, TPS warp, masky, inpaint → Fáze 2
- Postgres (dnes in-memory), auth, kredity, IAP
- editor s slidery → Fáze 3

Atributy vrstev `geometry` a `mask` dnes jedou jen přes prompt.
API to přiznává v odpovědi polem `degraded_attrs` — příjemce má vědět,
že to zatím není přesné vynucení.

---

## Licence

**Licenční čistota je tvrdý constraint, ne preference.** Zjištění z ověření
2026-08-06 jsou v [`docs/licence-audit.md`](docs/licence-audit.md) a strojově
v [`data/licenses.yaml`](data/licenses.yaml).

Tři věci, které změnily zadání oproti původnímu plánu:

- **Depth Anything V2 Base není Apache** — jen Small. Base/Large/Giant jsou CC-BY-NC-4.0.
- **GFPGAN není čistý** — jeho vlastní LICENSE vyjímá StyleGAN2 (NVIDIA, non-komerční).
  Face restore je proto v v0 vypnutý úplně.
- **Illustrious XL v2.0 je čistý** (Open RAIL-M) — anime kategorie je odblokovaná dřív,
  než plán čekal. Platí to pro v2.0, ne pro v0.1.

Na Sparku fyzicky leží checkpointy, které Avatar použít nesmí (Juggernaut, Pony).
Proto je v registru vede explicitně jako `blocked` a testy hlídají, že se
nedají vyřešit ani omylem.

```bash
cd api && go test ./internal/preset/    # licenční testy
```

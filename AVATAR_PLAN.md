# AVATAR_PLAN.md — Face Avatar Studio

> Flutter (Android + iOS) + Go API + ComfyUI na DGX Sparku.
> **Stack: SDXL only. Žádný FLUX, žádný InsightFace.** Viz §4 — licenční čistota je tvrdý constraint, ne preference.
> Jádro produktu: **rozpor identita ↔ atribut**. Vysoká váha identity zachová obličej, ale zablokuje změny (modré oči, dready). Nízká váha změny povolí, ale ztratí podobnost.
> Celá architektura je odpověď na tenhle konflikt: **atributy se nevynucují promptem, ale geometrií a maskou.**

---

## 1. Vize a uživatelská smyčka

Upload 1–5 fotek → analýza obličeje → galerie presetů, kde už je **vidět vlastní tvář** → výběr presetu → editor atributů → live preview → finální render → export.

**Pocit, o který jde:** uživatel hne sliderem "rty" a změní se **jen rty**. Nezmění se pozadí, účes ani výraz. V momentě, kdy se při jedné změně překreslí celý obrázek, uživatel ztratí důvěru a přestane experimentovat.

---

## 2. Tři vrstvy tweaků

Každý atribut patří právě do jedné vrstvy. Vrstva určuje, jak se změna technicky vynutí.

| Vrstva | Atributy | Mechanismus | Kdy se aplikuje |
|---|---|---|---|
| **Geometrie** | rty, nos, čelist, šířka obličeje, tvar/velikost očí | landmark warp (TPS) | **před** generací, na vstupní foto |
| **Maska** | barva očí, vlasy, dready, vousy, obočí, tón pleti | segmentace → inpaint jen dané oblasti | **po** base renderu |
| **Prompt** | styl, nálada, materiál, světlo, prostředí | textový fragment z preset šablony | při base renderu |

**Proč to takhle:** barva očí je ~200 px. Silná identita ji při každém kroku vrátí zpátky k originálu, ať do promptu napíšeš cokoliv. Maskovaný inpaint se sníženou váhou identity v masce to vyřeší deterministicky.

**Proč geometrie před generací:** warp vstupní fotky změní landmarky, které jdou do ControlNetu, i vstup pro identitu. Tvar se propíše do celého renderu konzistentně.

---

## 3. Pipeline

```
[1] Ingest      on-device: MediaPipe detekce, quality gate, crop
       ↓
[2] Analyze     478 landmarků (+iris), odvozené masky, (LoRA training job)
       ↓
[3] Warp        geometrické atributy → TPS warp vstupní fotky
       ↓
[4] Base render preset → SDXL checkpoint + user LoRA + IP-Adapter + ControlNet
       ↓
[5] Inpaint     maskované atributy, sekvenčně, identita ↓ v masce
       ↓
[6] Post        face restore (jemně/vypnuto), color match, crop varianty, rembg
```

### 3.1 Quality gate (na zařízení, blokuje upload)
- rozlišení obličeje < 512 px → odmítnout
- Laplacian variance < práh → "fotka je rozmazaná"
- více obličejů → nechat vybrat
- yaw/pitch > 30° → varovat
- okluze (brýle, ruka, vlasy přes oči) → varovat

Gate běží **před** uploadem. Šetří round-tripy, GPU čas, a je to lepší privacy story do App Store review.

### 3.2 Masky bez segmentačního modelu

**Nepoužívej BiSeNet face-parsing.** Běžné váhy jsou trénované na CelebAMask-HQ, což je research-only dataset — stejná kategorie problému jako InsightFace. (Ověř; pokud najdeš váhy trénované na permisivním datasetu, platí jinak.)

Čistá náhrada, která je pro tenhle use-case i přesnější:

| Oblast | Zdroj masky | Licence |
|---|---|---|
| rty, oči, **duhovka**, obočí, obrys obličeje, nos | MediaPipe Face Landmarker → polygon | Apache 2.0 |
| vlasy | SAM 2, point prompt odvozený z landmarků (nad čelem) | Apache 2.0 |
| vousy | landmarky (čelist ∩ pod-rty) mínus rty | Apache 2.0 |
| pleť | obrys obličeje mínus oči/rty/obočí | Apache 2.0 |

MediaPipe s `refine_landmarks` dává **iris landmarky** — maska duhovky je přesnější než cokoliv, co ti dá sémantická segmentace na 512 px. Pro barvu očí je to přesně to, co chceš.

Vlasy jsou jediná oblast, kde geometrie nestačí (siluetu nelze odvodit z landmarků) → SAM 2.

### 3.3 Multi-foto
Doporuč 3–5 fotek. Je to zásadní pro kvalitu LoRA (§4.2) a pro robustnost warpu. Jedna fotka je fallback, ne default.

### 3.4 Face restore opatrně
**Ověřeno: obě volby padly.** CodeFormer je NTU S-Lab (non-komerční). GFPGAN se tváří jako Apache 2.0,
ale jeho vlastní LICENSE soubor vyjímá **StyleGAN2 (NVIDIA License, výslovně non-komerční)** a **DFDNet (CC-BY-NC-SA)**.
StyleGAN2 prior není volitelná závislost — je to jádro generátoru. Pro placenou appku je GFPGAN mimo hru.

**Závěr: face restore je v v0 vypnutý úplně** (`face_restore: 0.0` ve všech presetech).
Plán s tím stejně počítal jako s preferovanou variantou — teď je to i licenční nutnost, ne jen estetická volba.
Kdyby se ukázalo, že bez něj kvalita nestačí, čistá cesta je vlastní restore model nebo upscaler
s permisivní licencí (např. 4x-UltraSharp / RealESRGAN — ověřit zvlášť), ne GFPGAN.

---

## 4. Identita: dvoustupňový model

FLUX i InstantID jsou mimo hru. InstantID stojí na InsightFace/antelopev2, jehož **váhy** jsou non-komerční (kód je MIT, váhy ne). Totéž platí pro IP-Adapter-**FaceID** varianty — jsou CC-BY-NC právě kvůli téhle závislosti.

Zbývají dvě cesty a plán používá **obě**, protože se doplňují.

### 4.1 Instant track — první dojem (sekundy)
- **IP-Adapter** (základní SDXL varianty, Apache 2.0) s výřezem obličeje přes CLIP image encoder
- **PhotoMaker V1** (Apache 2.0, CLIP-based — pozor, **V2 přidal InsightFace**, ten nepoužívej)
- ControlNet depth + soft edge (xinsir **union SDXL promax**, Apache 2.0 — jeden model pokryje depth, softedge i inpaint) drží pozici a proporce

Podobnost: střední. Stačí na "to jsem já-ish" během onboardingu.

### 4.2 Quality track — per-user LoRA (minuty, jednorázově)
Trénink SDXL LoRA na 3–5 fotkách uživatele. **Byl to non-goal v původním plánu — teď je to hlavní identity mechanismus.**

- rank 8–16, ~800–1500 kroků, cca 2–5 min na GB10
- výstup ~10–30 MB, ukládá se k `face_id`
- licenčně úplně čisté: trénuješ na uživatelových vlastních datech nad SDXL base
- podobnost: výrazně nejlepší z dostupných možností

**Onboarding flow:** upload → instant track vyrenderuje 6 presetů (uživatel má co dělat) → na pozadí běží LoRA job → po dokončení se **rerendrují** z uložených recipes ve vyšší kvalitě, s notifikací "tvůj avatar je připravený".

Tohle je vlastně lepší UX než původní návrh, ne kompromis. Uživatel vidí okamžitý výsledek a pak nečekané zlepšení.

### 4.3 `face_id` má verze
```
face_v1 = { landmarks, masks, ipadapter_ref_crop }
face_v2 = { ...v1, lora_path, lora_trigger_token }
```
Recipe referencuje `face_id`, ne verzi → rerender po dotrénování je zadarmo.

---

## 5. Checkpointy a licenční verify-list

**Nejsem právník a licence se mění — ověřeno 2026-08-06, přeověřuj před releasem (§11 Fáze 4).**

Sloupec „ověřeno" = zjištěno z model karty / LICENSE souboru, ne domněnka.

| Model | Licence (ověřeno 2026-08-06) | Závěr |
|---|---|---|
| SDXL base 1.0 | CreativeML Open RAIL++-M | ✅ komerční OK, use-restrictions propsat do ToS |
| SDXL-Lightning / DMD2 LoRA | openrail++ / MIT | ✅ |
| **Juggernaut XL v9 / Lightning** | RunDiffusion, vlastní podmínky | ❌ **„may not be deployed behind paid API services without explicit licensing"** — přesně náš případ. Buď deal s RunDiffusion, nebo nepoužívat |
| RealVisXL V5.0 | openrail++ (na kartě bez dodatků) | ✅ pravděpodobně OK, dodatky nenalezeny |
| **Pony Diffusion V6 XL** | modifikovaná Fair AI PL 1.0-SD | ❌ modifikace **zakazuje inference na monetizovaných appkách**; blanket souhlas jen pro CivitAI/HF |
| **Illustrious XL v2.0** | CreativeML Open RAIL-M | ✅ **v2.0 přelicencováno z FAIPL** — anime kategorie odblokována dřív, než plán čekal (v0.1 je pořád FAIPL) |
| IP-Adapter base + **plus-face** SDXL (h94) | Apache 2.0 | ✅ plus-face je v Apache repu, žádný InsightFace |
| IP-Adapter **FaceID** | CC-BY-NC (samostatný repo) | ❌ |
| PhotoMaker V1 | Apache 2.0 | ✅ |
| PhotoMaker V2 | Apache 2.0 kód, ale vyžaduje soulad s **InsightFace** licencí | ❌ |
| xinsir ControlNet **union SDXL promax** | Apache 2.0 | ✅ depth + softedge + inpaint v jednom modelu |
| Depth Anything V2 **Small** | Apache 2.0 | ✅ jediná použitelná varianta |
| Depth Anything V2 **Base / Large / Giant** | **CC-BY-NC-4.0** | ❌ **oprava plánu: Base NENÍ Apache**, jen Small |
| SAM 2 | Apache 2.0 | ✅ |
| MediaPipe | Apache 2.0 | ✅ |
| BiRefNet (rembg) | MIT | ✅ |
| **GFPGAN** | Apache 2.0 **s výjimkami ve vlastním LICENSE**: StyleGAN2 (NVIDIA, non-komerční) + DFDNet (CC-BY-NC-SA) | ❌ **oprava plánu: není čistý.** StyleGAN2 prior je jádro architektury, ne volitelná závislost |
| CodeFormer | NTU S-Lab, non-komerční | ❌ |
| SD 3.5 | Stability AI Community License | ✅ komerčně zdarma pod $1M ročního obratu, pak Enterprise |

**Bezpečné jádro, kdyby všechno ostatní padlo:** SDXL base + vlastní fine-tune/merge + user LoRA + xinsir ControlNet + IP-Adapter base. Na tomhle postavíš v0 bez jediné pochybnosti.

### 5.1 Zvažované a zamítnuté alternativy
- **FLUX.1-dev** — non-komerční. Zamítnuto.
- **FLUX.1-schnell** — Apache 2.0, technicky použitelné, ale zamítnuto s celou rodinou (rozhodnutí: jeden engine family, ne dva).
- **SD 3.5** — Stability Community License, komerční použití zdarma pod ~$1M ročního obratu. **Nezamítnuto, jen odloženo.** Kandidát na druhý tier v Fázi 3, pokud SDXL kvalitativně nestačí. Práh $1M ověřen 2026-08-06.

### 5.2 Anime presety
**Vyřešeno ověřením:** Pony V6 je ❌ (modifikace Fair AI PL výslovně zakazuje monetizovanou inferenci),
ale **Illustrious XL v2.0 je ✅ CreativeML Open RAIL-M** — přelicencováno oproti v0.1.

Anime kategorie tedy může jet na Illustrious v2.0 s `license_tier: safe`. Pozor: platí to pro **v2.0**,
ne pro v0.1 (FAIPL). Verzi checkpointu drž v presetu explicitně a nikdy ji „nevylepšuj" na jinou bez re-verifikace.

---

## 6. Engine matrix (po zúžení na SDXL)

| Profil | Checkpoint | Identity | CFG | Steps | Použití |
|---|---|---|---|---|---|
| `preview` | SDXL + Lightning LoRA | IP-Adapter | 1.5–2.5 | 6–8 | slider drag |
| `final` | SDXL fotorealistický | user LoRA + IP-Adapter | 5–7 | 25–30 | slider release |
| `final-stylized` | SDXL + style LoRA | user LoRA (nižší váha) | 5–7 | 25–30 | artistic presety |
| `inpaint` | tentýž jako base | identita ↓ 0.2–0.3 | 5–7 | 20 | maskované atributy |

**Zjednodušení oproti původnímu plánu:** jedna rodina modelů = jedna fronta, jeden rezidentní worker, žádný swap FLUX↔SDXL. To byla nejdražší operace v systému a teď je pryč. Latence i složitost dispatchu klesají.

Zůstává pattern **prompt styl podle checkpointu** (booru tagy vs. přirozený jazyk) — drž ho v preset schematu jako `prompt_dialect`.

---

## 7. Datové modely

### 7.1 Preset

```json
{
  "id": "cyber-oni",
  "version": 3,
  "category": "stylized",
  "checkpoint": "sdxl_base_1.0",
  "profile": "final-stylized",
  "prompt_dialect": "natural",
  "loras": [
    { "name": "style_cyber_neon", "weight": 0.7 },
    { "name": "$user_lora", "weight": 0.75 }
  ],
  "prompt_template": "portrait of {subject}, {attrs}, neon rim light, {quality}",
  "negative_template": "{nsfw_lock}, blurry, deformed, extra fingers",
  "identity": { "method": "ipadapter", "weight": 0.6, "start": 0.0, "end": 0.9 },
  "controlnet": [{ "type": "depth_anything_v2_small", "weight": 0.5 }],
  "sampler": { "name": "dpmpp_2m", "scheduler": "karras", "steps": 28, "cfg": 6.0 },
  "supports_attrs": ["lips_fullness", "eye_color", "hair_style", "skin_tone"],
  "nsfw_negative_lock": true,
  "face_restore": 0.0,
  "license_tier": "safe"
}
```

`$user_lora` je placeholder — resolver ho nahradí cestou z `face_v2`. Když LoRA ještě není hotová, položka vypadne a jede se instant trackem.

`license_tier` (`safe` / `verify` / `blocked`) umožní vypnout celou kategorii presetů jedním flagem, kdyby se licenční situace změnila. **Tohle si napiš hned** — je to levné teď a drahé zpětně.

`supports_attrs`: UI **skryje** slidery, které preset neumí. Lepší než je zobrazit a nechat je nic nedělat.

### 7.2 Mapovací tabulka atributů

```yaml
lips_fullness:
  layer: geometry
  range: [-1, 1]
  default: 0
  geometry:
    method: tps
    landmarks: [61, 291, 0, 17, 13, 14]   # MediaPipe lip contour
    scale: 0.35
  prompt:
    natural: { -1: "thin lips", 1: "full plump lips" }
    booru:   { -1: "thin lips", 1: "large lips" }

eye_color:
  layer: mask
  type: discrete
  options: [brown, blue, green, hazel, grey, amber, heterochromia]
  mask:
    source: mediapipe_iris        # ne segmentace
    regions: [l_iris, r_iris]
  inpaint: { denoise: 0.55, identity_weight_override: 0.2, feather: 3 }

hair_style:
  layer: mask
  type: discrete
  options: [short, buzz, dreadlocks, afro, long_straight, curly, bald, mohawk]
  mask:
    source: sam2
    prompt_points: from_landmarks(forehead_top, +0.15h)
  inpaint: { denoise: 0.85, identity_weight_override: 0.3, feather: 12, expand: 24 }
```

Vlasy potřebují **expand masky** — dready jsou objemnější než krátké vlasy a musí mít kam růst. Bez toho se ořežou na siluetu původního účesu.

### 7.3 Recipe — objekt první třídy

```
Recipe = (face_id, preset_id@version, attr_vector, seed, model_hashes, timestamp)
```

**Ukládej recipe, ne jen výsledné PNG.** Zadarmo z toho dostaneš:
- undo/redo stack (pár set bytů na krok)
- rerender po dotrénování LoRA (§4.2) — **kritické pro tenhle design**
- re-render ve vyšším rozlišení
- regeneraci po update modelu nebo změně licence checkpointu
- sdílení "vzhledu" mezi uživateli (deep link)

### 7.4 Diferenciální rendering

Když uživatel hne **jedním** sliderem:
- vrstva `mask` → inpaint jen dotčené masky nad předchozím výsledkem, **stejný seed**
- vrstva `geometry` → nutný full re-render (warp mění vstup) → preview režim s debounce
- vrstva `prompt` → full re-render, stejný seed

Rozdíl mezi "změním rty a zůstane všechno ostatní" a "změním rty a je to jiný obrázek" je rozdíl mezi nástrojem a hračkou.

---

## 8. Backend

Navazuje na existující stack (AiStack, gen-queue, `shop.ol1n.com` pattern).

### 8.1 Go API (`avatar-api`, chi)
- auth, kredity, rate limit
- recipe store, preset store (hot-reload, respektuje `license_tier`)
- dispatch do gen-queue, SSE progress do Flutteru
- signed URL pro výsledky

### 8.2 gen-queue
Jedna rodina modelů → jednodušší než původní návrh:
- `q:render` — jeden rezidentní ComfyUI worker, SDXL stále v paměti
- `q:preview` — Lightning, vysoká priorita, krátké joby
- `q:train` — **nová**, LoRA tréninky, low priority, dlouhé joby, izolovaná GPU alokace

`q:train` musí být oddělená, jinak ti pětiminutový trénink zablokuje interaktivní rendery.

### 8.3 Persistence
- Postgres: `users`, `faces`, `recipes` (JSONB), `presets` (JSONB), `renders`, `train_jobs`
- NAS: zdrojové fotky (TTL), **user LoRA soubory**, výstupy (CDN cache přes Cloudflare)
- LoRA storage rozpočet: 30 MB × N uživatelů — počítej to do kapacity od začátku

### 8.4 Latency tiering

| Režim | Rozlišení | Kroky | Cíl | Trigger |
|---|---|---|---|---|
| Preview | 768 | 6 (Lightning) | 1–2 s | slider drag (debounce 250 ms) |
| Final | 1024 | 28 | 10–20 s | slider release / tlačítko |
| LoRA train | — | ~1000 | 2–5 min | jednou po uploadu, na pozadí |
| Export | 2048 (upscale) | — | 30 s | explicitní akce |

### 8.5 Deployment
`avatar.ol1n.com` přes Cloudflare Tunnel, stejný Caddy/Access pattern jako zbytek infrastruktury.

---

## 9. Flutter UX

### 9.1 Onboarding
1. Upload 3–5 fotek, quality gate on-device
2. Okamžitě: 6 presetů instant trackem (paralelně, preview kvalita) → **wow moment a hlavní screenshot do storu**
3. Na pozadí: LoRA job, progress v UI nenápadně ("dolaďuji tvůj avatar…")
4. Po dokončení: rerender + notifikace

Krok 3 nesmí blokovat nic. Když trénink selže, appka funguje dál na instant tracku — jen o něco hůř.

### 9.2 Editor
- horní půlka: výsledek, pinch-zoom, A/B toggle na originál (držením)
- spodní půlka: taby **Oči / Rty / Nos / Vlasy / Pleť / Tvar**
- slidery pro spojité, horizontální picker pro diskrétní
- atributy, které preset neumí → skryté, ne disabled
- historie: horizontální strip miniatur = stack recipes

### 9.3 Export
Čtverec (avatar), kruh (profilovka), 16:9 banner, průhledné PNG (BiRefNet), sticker pack.

---

## 10. Katalog presetů

**Realistic** (`license_tier: safe`): studio headshot, cinematic film still, LinkedIn korporát, retro polaroid, black & white portrait.
**Stylized 2.5D**: semi-real illustration, stylized 3D render, clay render.
**Artistic**: komiks/ink, olej, akvarel, low-poly, pixel art.
**Fantasy/Genre**: cyberpunk neon, elf, ork, dwarf, tattoo flash.
**Anime** (`license_tier: verify`, Fáze 3): shoujo, 90s cel, modern digital.

Balíčky po 5–8 presetech jako sezónní IAP. Base sada zdarma.

---

## 11. Fáze implementace

### Fáze 0 — Vertikální řez
- [ ] Go API skeleton: upload → analyze → render → result, SSE progress
- [ ] ComfyUI workflow: SDXL + IP-Adapter + depth ControlNet, 1 preset
- [ ] Flutter: upload screen + result screen, žádný editor
- **DoD:** foto z telefonu → za < 20 s stylizovaný portrét, poznatelně tentýž člověk. **Bez jediné non-komerční závislosti.**

### Fáze 1 — LoRA track
- [ ] `q:train` fronta + SDXL LoRA training job (kohya nebo vlastní)
- [ ] `face_v1` → `face_v2` upgrade path, `$user_lora` resolver
- [ ] Rerender z recipes po dotrénování
- [ ] Tuning: rank, kroky, LR, počet fotek — hledej minimum, které drží podobnost
- **DoD:** LoRA hotová do 5 min, podobnost měřitelně lepší než instant track, fallback při selhání funguje

### Fáze 2 — Tweak engine
- [ ] MediaPipe masky (rty, iris, obočí, obrys) + SAM 2 na vlasy
- [ ] TPS warp modul (rty jako první atribut)
- [ ] Atributová tabulka jako YAML, loader v Go
- [ ] 2 slidery v UI: **rty** (geometry) + **vlasy** (mask)
- **DoD:** slider mění **jen** svou oblast, zbytek obrázku bit-identický mimo feather zónu

### Fáze 3 — Presety, recipes, katalog
- [ ] Preset schema + `license_tier` + loader, 15+ presetů
- [ ] Recipe persistence, undo/redo
- [ ] Preview/final tiering, debounce
- [ ] Všech 6 skupin atributů
- [ ] Rozhodnutí o anime kategorii a SD 3.5 tieru
- [ ] Tuning pass: 30 reálných obličejů, různé etnicity, různé světlo
- **DoD:** uživatel projde galerii, edituje, vrátí se o 5 kroků zpět, exportuje. Podobnost > 80 % na testovací sadě

### Fáze 4 — Compliance a release
- [ ] Consent gate, age detection, NSFW output klasifikátor
- [ ] GDPR: souhlas, retence, export, mazání (**včetně smazání LoRA**)
- [ ] C2PA metadata / watermark
- [ ] IAP (StoreKit 2 / Play Billing), kredity
- [ ] Licenční audit celého stacku před submitem
- **DoD:** prošlo review na obou platformách

---

## 12. Rizika

| Riziko | Mitigace |
|---|---|
| **Podobnost bez InstantID nebude stačit** | hlavní technické riziko celého plánu. Změř to ve Fázi 1, ne později. Pokud LoRA nestačí, další eskalace je vlastní ID adaptér trénovaný na permisivním datasetu — drahé |
| Juggernaut / Pony licence pro hosting | **ověřeno: oba ❌.** Na DGX jsou stažené a používají se jinde — Avatar je nesmí referencovat. Řeší `license_tier` + allowlist checkpointů v resolveru, ne jen dobrá vůle |
| LoRA trénink zablokuje interaktivní rendery | oddělená `q:train` fronta, hard limit souběžných tréninků |
| Storage LoRA souborů roste lineárně s uživateli | rank 8, fp16, TTL na neaktivní účty, recipe umožní přetrénovat na vyžádání |
| Identita se ztratí u silných tweaků | clamp na součet \|Δatributů\|, varování v UI, ne tvrdý blok |
| GDPR čl. 9 — biometrický údaj | explicitní souhlas, TTL na zdrojové fotky. **Pozor: user LoRA je odvozený biometrický údaj** — musí jít smazat spolu s účtem |
| AI Act transparence | označení syntetického obsahu, C2PA; termíny ověřit |
| Deepfake zneužití | consent gate "jen vlastní obličej", age detection → block, žádné celebrity v marketingu. Per-user LoRA tohle riziko **zvyšuje** — hlídej upload cizích tváří |
| Bias v identity pipeline | testovací sada napříč etnicitami už ve Fázi 1, ne až před releasem |

---

## 13. Non-goals (v1)

- video / animovaný avatar
- full-body generace
- vlastní trénink base modelů
- sociální feed / sdílení uvnitř appky
- FLUX v jakékoliv podobě (rozhodnuto, §5.1)

*Per-user LoRA byla přesunuta z non-goals do jádra (§4.2).*

---

*Handoff pro Claude Code: začni Fází 0. Preset schema, atributovou tabulku a `license_tier` vytvoř jako data soubory hned na začátku (i s jednou položkou) — všechno ostatní se na ně navěsí. Před psaním kódu projdi verify-list v §5 a doplň zjištěné licence přímo do tabulky. Komentáře v kódu česky, identifikátory anglicky.*

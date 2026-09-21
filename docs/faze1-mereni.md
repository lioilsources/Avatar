# Fáze 1 — měření podobnosti

Plán označuje „podobnost bez InstantID nebude stačit" za **hlavní technické
riziko celého návrhu** (§12) a chce ho změřit ve Fázi 1, ne později.

**Závěr: infrastruktura na měření stojí a funguje, ale první měření je
neprůkazné a vím proč. DoD „podobnost měřitelně lepší než instant track"
zatím NENÍ prokázané.** Níže je, co se naměřilo, proč to nestačí, a co
je potřeba k platnému výsledku.

---

## Metrika

Správný nástroj je ArcFace embedding. Jenže ArcFace váhy chodí přes
InsightFace a ty jsou non-komerční — přesně to, kvůli čemu celý plán
nepoužívá InstantID. Mít takovou závislost v repu jen kvůli internímu
měření by porušovalo vlastní pravidla projektu.

Použité náhrady:

| model | licence | co měří |
|---|---|---|
| DINOv2 base | Apache 2.0 | instance-level podobnost (jemná vizuální struktura) |
| CLIP ViT-L/14 | MIT | sémantická podobnost (styl, typ, celkový dojem) |

**Ani jedno není face recognition.** Absolutní hodnoty proto neříkají
„podobnost je 87 %". Smysl dává jen relativní rozdíl mezi variantami,
a i ten jen s podlahou (cizí obličeje) a stropem (reference navzájem).

---

## Ořez rozhoduje o tom, co se vlastně měří

První pokus ořezával „horní střední část obrázku". Výsledek byl nesmyslný:

| ořez | DINOv2 instant / LoRA | CLIP instant / LoRA |
|---|---|---|
| žádný (celý obrázek) | 0,758 / 0,755 | 0,932 / 0,935 |
| heuristický (střed) | 0,650 / 0,615 | 0,856 / 0,936 |
| **MediaPipe detekce** | **0,815 / 0,762** | **0,854 / 0,936** |

Bez ořezu vyšla podobnost kandidátů **výš než podobnost referencí mezi
sebou** — protože všechny vygenerované obrázky sdílí jedno pozadí
a jeden styl, zatímco referenční sada má pozadí schválně různá. Metrika
tedy měřila pozadí, ne obličej.

Proto vznikl `scripts/face_crop.py` s MediaPipe Face Detection (Apache 2.0).
Detekce našla obličej na všech 12 obrázcích. Je to zároveň první kus
Fáze 2 — plán MediaPipe stejně potřebuje na landmarky a masky (§3.2).

---

## Naměřeno (ořez přes MediaPipe)

Referenční sada: 4 fotky jedné syntetické osoby.
Kandidáti: 3 rendery na variantu, stejné seedy (2001–2003), stejný preset.

| | DINOv2 | CLIP |
|---|---|---|
| podlaha (cizí obličeje) | 0,386 | 0,643 |
| **instant track** (IP-Adapter) | **0,815** | **0,854** |
| **LoRA track** (IP-Adapter + user LoRA) | **0,762** | **0,936** |
| strop (reference navzájem) | 0,823 | 0,918 |

Na škále podlaha → strop:

| | instant | LoRA | rozdíl |
|---|---|---|---|
| DINOv2 | 98 % | 86 % | **−12 b.** |
| CLIP | 77 % | 107 % | **+30 b.** |

**Metriky si odporují, a to konzistentně napříč všemi třemi variantami
ořezu.** Není to šum v ořezu.

---

## Tříčestné srovnání a co je na něm vidět

Doplněno o variantu **LoRA samotná, bez IP-Adapteru**, aby šlo oddělit
příspěvky obou mechanismů:

| varianta | DINOv2 | CLIP |
|---|---|---|
| instant — jen IP-Adapter | 0,815 (98 %) | 0,854 (77 %) |
| jen user LoRA | 0,754 (84 %) | 0,873 (84 %) |
| **IP-Adapter + user LoRA** | 0,762 (86 %) | **0,936 (107 %)** |

**Vizuální kontrola odhalila konkrétní vadu, kterou čísla samotná neukázala:
LoRA samotná uletí s barvou vlasů.** Reference má tmavě hnědé vlasy, LoRA-only
rendery je mají světlé až blond. Struktura obličeje sedí, barva vlasů ne.

To vysvětluje penalizaci v DINOv2 — ta je na takovou změnu citlivá — a je to
pravděpodobná příčina rozporu mezi metrikami.

Proč k tomu nejspíš došlo: trénink použil jediný popisek `a photo of {trigger}`
bez variace a **netrénoval textový enkodér**. Trigger token tak má slabou vazbu
a model si nemá jak oddělit „tahle osoba" od „takhle vypadaly ty čtyři fotky".

**Praktický závěr: obě metody jsou komplementární, ne alternativní.**
IP-Adapter drží barvy a celkový vzhled, LoRA přidává strukturu obličeje.
Kombinace vypadá vizuálně nejlíp a má nejvyšší CLIP skóre. Plán tohle
předpokládal (§4: „plán používá obě, protože se doplňují") — teď to má
i empirickou oporu, byť slabou.

**Kandidáti na opravu drift barvy vlasů** (netestováno):
- trénovat i textový enkodér, nebo aspoň variovat popisky mezi fotkami
- nižší LR nebo míň kroků (přeučení na styl trénovací sady)
- rank 8 místo 16 — menší kapacita může paradoxně pomoct generalizaci
- caption s explicitní barvou vlasů, aby si ji model nemusel „vymýšlet"

---

## Proč je tohle měření neplatné

Dvě věci, obě moje chyba v návrhu experimentu:

### 1. Referenční sada vznikla jednou z porovnávaných metod

„Fotky uživatele" jsem vyrobil z jednoho výchozího portrétu pomocí
**IP-Adapteru na váze 0,95**. Instant track pak používá **tentýž
IP-Adapter na tomtéž zdrojovém obrázku**.

Instant track a referenční sada jsou tedy sourozenci ze stejného
mechanismu. LoRA se učila až z výstupů toho mechanismu, takže je
o generaci dál. Jakákoliv metrika citlivá na nízkoúrovňovou vizuální
strukturu (= DINOv2) musí instant track zvýhodnit.

**To není zjištění o LoRA, to je artefakt setupu.**

### 2. Jedna identita, tři vzorky

Jedna syntetická osoba a tři rendery na variantu nestačí na žádný závěr
o rozdílu 10–30 procentních bodů. Rozptyl mezi seedy nebyl změřen.

### Vedlejší podezření: přeučení

CLIP dal LoRA variantě **107 %**, tedy víc než mají reference mezi sebou.
Při 1000 krocích, ranku 16 a jen 4 fotkách je přeučení pravděpodobné —
LoRA možná reprodukuje trénovací obrázky včetně jejich stylu, ne jen
identitu. To by se projevilo přesně takhle: vysoké CLIP, nižší DINOv2.

---

## Co je potřeba k platnému měření

1. **Referenční sada nezávislá na obou metodách.** Buď skutečné fotky
   skutečného člověka (víc různých pořízení), nebo syntetická identita
   vyrobená třetím způsobem. Bez toho je srovnání předem nakloněné.
2. ~~Tři varianty, ne dvě~~ — **doplněno**, viz výše.
3. **Víc vzorků.** 3 rendery na variantu je málo na jakýkoliv závěr.
4. **Víc identit.** Jedna osoba není testovací sada. Plán chce 30 obličejů
   napříč etnicitami už ve Fázi 1, ne až před releasem (§12, řádek o biasu).
5. **Kontrola přeučení:** render s LoRA na úplně jiný prompt (jiné oblečení,
   jiné prostředí). Když LoRA vrátí trénovací obrázek, je přeučená.

---

## Co z Fáze 1 prokázané JE

- trenér funguje a výstup ComfyUI načte **bez jediného** `lora key not loaded`
- soubor má konzistentní strukturu: 560 × (`lora_down`, `lora_up`, `alpha`),
  všechny klíče s prefixem `lora_unet_`
- `face_v1` → `face_v2` upgrade a rerender receptu fungují
- fallback při selhání tréninku funguje (testy v `internal/train/`)
- kombinace IP-Adapter + LoRA vypadá vizuálně nejlíp ze všech tří variant
- LoRA samotná má **reprodukovatelnou vadu**: drift barvy vlasů (viz výše).
  Tohle je konkrétní, opravitelný nález, ne dojem

---

## Jak měření spustit

```bash
cd api && make deploy-scripts eval-setup

# ořez na obličej
ssh spark 'cd ~/avatar && venv-face/bin/python face_crop.py \
  --images eval/*.png --out-dir eval/cropped --model blaze_face_short_range.tflite'

# podobnost
ssh spark 'cd ~/avatar && ~/Code/ComfyUI/.venv/bin/python eval_similarity.py \
  --reference eval/cropped/train_*.png \
  --candidates "instant=..." "lora=..." \
  --unrelated eval/cropped/unrelated_*.png --no-crop'
```

`--no-crop` je správně: obrázky už ořezal `face_crop.py`.

# Licenční audit — 2026-08-06

Odpověď na verify-list z `AVATAR_PLAN.md` §5. Strojově čitelná podoba je
v `data/licenses.yaml`; ta je zároveň vynucená v kódu (`internal/preset/license.go`).

**Nejsem právník.** Tohle je záznam toho, co stojí na model kartách a v LICENSE
souborech k datu ověření, ne právní stanovisko. Před releasem přeověřit
(Fáze 4 to má jako položku).

---

## Co změnilo zadání

### 1. Depth Anything V2 Base je CC-BY-NC-4.0, ne Apache

Plán tvrdil „small/base Apache 2.0, **large je CC-BY-NC**". Ve skutečnosti je
Apache 2.0 **jen Small**; Base, Large i Giant jsou CC-BY-NC-4.0.

Ověřeno přímo na model kartě `depth-anything/Depth-Anything-V2-Base`
(license tag `cc-by-nc-4.0`).

**Dopad:** preset v §7.1 plánu měl `depth_anything_v2_base` → opraveno na `_small`.
V ComfyUI je to `depth_anything_v2_vits.pth`. Pozor: node `DepthAnythingV2Preprocessor`
**defaultuje na `vitl`** (Large, CC-BY-NC), takže se to musí nastavit explicitně —
proto to hlídá test `TestDepthPreprocessorUsesSmallCheckpoint`.

### 2. GFPGAN není licenčně čistý

Plán měl „GFPGAN je Apache 2.0 (ověř i závislosti)". Závislosti nejsou ten problém —
problém je přímo v jeho vlastním LICENSE souboru, který z Apache 2.0 vyjímá:

- **StyleGAN2** — NVIDIA License, výslovně: *„The Work… only may be used or intended
  for use non-commercially"*
- **DFDNet** — CC BY-NC-SA 4.0

StyleGAN2 prior není volitelná závislost, je to jádro generátoru GFPGANu.

**Dopad:** face restore je v v0 vypnutý úplně. Plán to stejně preferoval
(„zkus verzi bez face restore vůbec"), teď je to licenční nutnost.
`face_restore > 0` sráží preset na tier `verify`, takže se v produkci nenabídne.

### 3. Illustrious XL v2.0 je čistý — anime se odblokovalo

Plán počítal s tím, že Pony i Illustrious mají otevřenou licenční otázku a anime
je proto až Fáze 3 s tierem `verify`.

- **Pony Diffusion V6 XL** — ❌ potvrzeno. Modifikovaná Fair AI PL 1.0-SD výslovně
  zakazuje inference na webech/appkách s jakoukoliv monetizací. Blanket souhlas
  má jen CivitAI a HuggingFace.
- **Illustrious XL v2.0** — ✅ **přelicencováno na CreativeML Open RAIL-M.**
  v0.1 je pořád FAIPL.

**Dopad:** anime kategorie může jet na Illustrious v2.0 jako `safe`.
V registru je verze v klíči (`illustrious_xl_v2.0`) právě proto, aby ji nikdo
„nevylepšil" na jinou bez re-verifikace.

---

## Plná tabulka

| Model | Licence | Závěr |
|---|---|---|
| SDXL base 1.0 | CreativeML Open RAIL++-M | ✅ bezpečné jádro |
| SDXL-Lightning LoRA | openrail++ | ✅ |
| DMD2 SDXL LoRA | MIT | ✅ |
| Juggernaut XL v9 / Lightning | RunDiffusion vlastní | ❌ zákaz nasazení za placenou API službou |
| RealVisXL V5.0 | openrail++ | ✅ na kartě bez dodatků |
| Pony Diffusion V6 XL | modifikovaná Fair AI PL | ❌ zákaz monetizované inference |
| Illustrious XL **v2.0** | CreativeML Open RAIL-M | ✅ (v0.1 ❌ FAIPL) |
| IP-Adapter base + **plus-face** SDXL | Apache 2.0 | ✅ žádný InsightFace |
| IP-Adapter **FaceID** | CC-BY-NC | ❌ |
| PhotoMaker V1 | Apache 2.0 | ✅ |
| PhotoMaker V2 | Apache 2.0 kód + soulad s InsightFace | ❌ |
| xinsir ControlNet union SDXL promax | Apache 2.0 | ✅ depth+softedge+inpaint v jednom |
| xinsir ControlNet openpose SDXL | Apache 2.0 | ✅ |
| InstantID ControlNet | pipeline stojí na InsightFace | ❌ |
| Depth Anything V2 **Small** | Apache 2.0 | ✅ |
| Depth Anything V2 Base/Large/Giant | **CC-BY-NC-4.0** | ❌ |
| CLIP-ViT-H-14 (laion) | MIT | ✅ |
| SAM 2 | Apache 2.0 | ✅ |
| MediaPipe | Apache 2.0 | ✅ |
| BiRefNet | MIT | ✅ |
| GFPGAN | Apache 2.0 s výjimkami | ❌ StyleGAN2 non-komerční |
| CodeFormer | NTU S-Lab | ❌ |
| SD 3.5 | Stability AI Community License | ✅ zdarma pod $1M obratu |

---

## Proč je registr vynucený v kódu

Na DGX Sparku fyzicky leží Juggernaut i Pony a používají se tam pro jiné projekty.
Kdyby byl `license_tier` jen štítek v presetu, stačil by jeden překlep v datovém
souboru a produkce by běžela na modelu, který se za peníze provozovat nesmí.

Proto:

1. `data/licenses.yaml` vede **i zakázané modely**, explicitně jako `blocked` —
   aby při psaní presetu bylo vidět, že to není opomenutí.
2. Resolver odmítne neznámý klíč i blocked klíč. Preset se pak vůbec nenačte.
3. Efektivní tier presetu je **nejpřísnější** ze všech jeho modelů a jeho vlastní
   deklarace. Preset si nemůže sám sobě povolit blocked checkpoint tím, že napíše
   `license_tier: safe`.
4. Neznámá hodnota tieru se chová jako nejpřísnější — překlep nesmí něco povolit.

Hlídají to testy v `api/internal/preset/license_test.go`, které načítají skutečný
`licenses.yaml`, ne fixturu.

---

## Zdroje

- [RunDiffusion/Juggernaut-XL-v9](https://huggingface.co/RunDiffusion/Juggernaut-XL-v9)
- [Pony Diffusion V6 XL](https://civitai.com/models/257749/pony-diffusion-v6-xl)
- [OnomaAIResearch/Illustrious-XL-v2.0](https://huggingface.co/OnomaAIResearch/Illustrious-XL-v2.0)
- [h94/IP-Adapter](https://huggingface.co/h94/IP-Adapter)
- [TencentARC/PhotoMaker (README_pmv2)](https://github.com/TencentARC/PhotoMaker/blob/main/README_pmv2.md)
- [xinsir/controlnet-union-sdxl-1.0](https://huggingface.co/xinsir/controlnet-union-sdxl-1.0)
- [depth-anything/Depth-Anything-V2-Base](https://huggingface.co/depth-anything/Depth-Anything-V2-Base)
- [depth-anything/Depth-Anything-V2-Small](https://huggingface.co/depth-anything/Depth-Anything-V2-Small)
- [GFPGAN LICENSE](https://github.com/TencentARC/GFPGAN/blob/master/LICENSE)
- [SG161222/RealVisXL_V5.0](https://huggingface.co/SG161222/RealVisXL_V5.0)
- [Stability AI Community License](https://stability.ai/license)

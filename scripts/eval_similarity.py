#!/usr/bin/env python3
"""Měření podobnosti mezi referenčními fotkami a vygenerovanými avatary.

Odpovídá na otázku z §12 plánu: "Podobnost bez InstantID nebude stačit" je
označené jako hlavní technické riziko a plán chce, aby se změřilo ve Fázi 1.

DŮLEŽITÁ VÝHRADA K METRICE
--------------------------
Správný nástroj na tohle je ArcFace embedding (kosinová vzdálenost mezi
obličejovými vektory). Jenže ArcFace váhy chodí přes InsightFace, a ty jsou
non-komerční — tedy přesně to, kvůli čemu celý plán nepoužívá InstantID.
Použít je i jen na interní měření by znamenalo mít v repu závislost, která
tam podle vlastních pravidel být nesmí.

Používáme proto dvě licenčně čisté náhrady:

  DINOv2 (Apache 2.0)  — self-supervised ViT. Na instance-level podobnost
                         (tenhle konkrétní objekt vs. jiný objekt téže třídy)
                         je výrazně lepší než CLIP. Tohle je hlavní číslo.
  CLIP   (MIT)         — sémantická podobnost. Doplňkové číslo; chytá spíš
                         "je to portrét člověka v podobném stylu".

Ani jedno NENÍ face recognition. Absolutní hodnoty proto neříkají "podobnost
je 87 %". Co říkají, je **relativní rozdíl mezi dvěma variantami téhož
pipeline** — a přesně to Fáze 1 potřebuje: je LoRA track měřitelně lepší než
instant track?

Baseline nesouvisejících obličejů se počítá taky, aby bylo vidět, kde leží
podlaha metriky. Bez ní je číslo 0.7 neinterpretovatelné.
"""

from __future__ import annotations

import argparse
import itertools
import json
import sys
from pathlib import Path

import torch
import torch.nn.functional as F
from PIL import Image


def log(msg: str, **kv: object) -> None:
    print(json.dumps({"msg": msg, **kv}, ensure_ascii=False), flush=True)


class Embedder:
    """Obalí model tak, aby vracel normalizovaný vektor pro obrázek."""

    def __init__(self, kind: str, device: torch.device):
        self.kind = kind
        self.device = device

        if kind == "dinov2":
            from transformers import AutoImageProcessor, AutoModel

            name = "facebook/dinov2-base"  # Apache 2.0
            self.proc = AutoImageProcessor.from_pretrained(name)
            self.model = AutoModel.from_pretrained(name).to(device).eval()

        elif kind == "clip":
            from transformers import CLIPModel, CLIPImageProcessor

            name = "openai/clip-vit-large-patch14"  # MIT
            self.proc = CLIPImageProcessor.from_pretrained(name)
            self.model = CLIPModel.from_pretrained(name).to(device).eval()

        else:
            raise ValueError(f"neznámý model {kind}")

    @torch.no_grad()
    def embed(self, img: Image.Image) -> torch.Tensor:
        inputs = self.proc(images=img.convert("RGB"), return_tensors="pt")
        inputs = {k: v.to(self.device) for k, v in inputs.items()}

        if self.kind == "dinov2":
            out = self.model(**inputs).last_hidden_state[:, 0]  # CLS token
        else:
            # get_image_features mění návratový typ mezi verzemi transformers
            # (5.x vrací obalový objekt, starší rovnou tensor). Voláme proto
            # vision tower a projekci ručně — to je stabilní napříč verzemi.
            pooled = self.model.vision_model(**inputs).pooler_output
            out = self.model.visual_projection(pooled)

        return F.normalize(out.float(), dim=-1).squeeze(0)


def center_face_crop(img: Image.Image, frac: float = 0.55) -> Image.Image:
    """Hrubý výřez horní střední části — tam u portrétu sedí obličej.

    Bez výřezu by metriku dominovalo pozadí a oblečení, což je přesně to,
    co měřit nechceme. Není to detekce obličeje; ta přijde ve Fázi 2
    a tenhle heuristický ořez pak nahradí.
    """
    w, h = img.size
    side = int(min(w, h) * frac)
    left = (w - side) // 2
    top = int(h * 0.08)  # obličej bývá nad středem, ne uprostřed
    return img.crop((left, top, left + side, min(h, top + side)))


def load(paths: list[Path], crop: bool) -> list[Image.Image]:
    out = []
    for p in paths:
        img = Image.open(p).convert("RGB")
        out.append(center_face_crop(img) if crop else img)
    return out


def mean_cross_similarity(a: list[torch.Tensor], b: list[torch.Tensor]) -> float:
    """Průměrná kosinová podobnost každý-s-každým mezi dvěma sadami."""
    if not a or not b:
        return float("nan")
    sims = [float(torch.dot(x, y)) for x in a for y in b]
    return sum(sims) / len(sims)


def mean_within_similarity(v: list[torch.Tensor]) -> float:
    """Průměrná podobnost uvnitř jedné sady — strop, kterého lze dosáhnout."""
    if len(v) < 2:
        return float("nan")
    sims = [float(torch.dot(x, y)) for x, y in itertools.combinations(v, 2)]
    return sum(sims) / len(sims)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--reference", nargs="+", required=True,
                    help="referenční fotky uživatele (trénovací sada)")
    ap.add_argument("--candidates", nargs="+", required=True,
                    help="dvojice popisek=cesta,cesta,... např. instant=a.png,b.png")
    ap.add_argument("--unrelated", nargs="*", default=[],
                    help="nesouvisející obličeje — podlaha metriky")
    ap.add_argument("--no-crop", action="store_true", help="neořezávat na obličej")
    ap.add_argument("--json-out", default="", help="kam zapsat výsledek")
    args = ap.parse_args()

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    crop = not args.no_crop

    refs = load([Path(p) for p in args.reference], crop)
    unrelated = load([Path(p) for p in args.unrelated], crop) if args.unrelated else []

    groups: dict[str, list[Image.Image]] = {}
    for spec in args.candidates:
        if "=" not in spec:
            log("chyba", detail=f"očekáváno popisek=cesty, přišlo {spec!r}")
            return 1
        label, paths = spec.split("=", 1)
        groups[label] = load([Path(p) for p in paths.split(",") if p], crop)

    report: dict[str, object] = {"device": str(device), "crop": crop,
                                 "n_reference": len(refs)}

    for kind in ("dinov2", "clip"):
        emb = Embedder(kind, device)
        ref_v = [emb.embed(i) for i in refs]

        res: dict[str, object] = {
            # Strop: jak podobné jsou si navzájem samotné referenční fotky.
            "reference_self": round(mean_within_similarity(ref_v), 4),
        }
        if unrelated:
            un_v = [emb.embed(i) for i in unrelated]
            # Podlaha: nesouvisející obličeje. Bez tohohle čísla nejde
            # říct, jestli je 0.65 dobré nebo špatné.
            res["unrelated_floor"] = round(mean_cross_similarity(ref_v, un_v), 4)

        for label, imgs in groups.items():
            cand_v = [emb.embed(i) for i in imgs]
            res[label] = round(mean_cross_similarity(ref_v, cand_v), 4)

        report[kind] = res
        del emb
        if device.type == "cuda":
            torch.cuda.empty_cache()

    log("výsledek", **report)

    if args.json_out:
        Path(args.json_out).write_text(json.dumps(report, indent=2, ensure_ascii=False))

    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001
        log("chyba", detail=f"{type(exc).__name__}: {exc}")
        raise

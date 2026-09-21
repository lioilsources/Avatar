#!/usr/bin/env python3
"""Ořez obrázků kolem obličeje pomocí MediaPipe Face Detection.

Vzniklo jako oprava měření podobnosti ve Fázi 1: heuristický ořez „horní
střední část obrázku" nedokázal identitu izolovat. Metrika pak měřila
pozadí a oblečení, ne obličej, a vycházela nesmyslně (podobnost kandidátů
vyšší než podobnost referencí mezi sebou).

Zároveň je to první kus Fáze 2 — MediaPipe je Apache 2.0 a plán s ním
počítá pro landmarky a masky (§3.2).

Běží ve vlastním venv, ne v tom od ComfyUI:
    /home/ol1n/avatar/venv-face/bin/python face_crop.py --help
Instalovat mediapipe vedle torchu do ComfyUI venv by zbytečně riskovalo
rozbití funkčního prostředí kvůli konfliktu numpy/protobuf.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import mediapipe as mp
import numpy as np
from PIL import Image, ImageOps


def log(msg: str, **kv: object) -> None:
    print(json.dumps({"msg": msg, **kv}, ensure_ascii=False), flush=True)


def detect_face_box(img: Image.Image, detector) -> tuple[int, int, int, int] | None:
    """Vrátí (left, top, right, bottom) největšího nalezeného obličeje."""
    arr = np.asarray(img.convert("RGB"), dtype=np.uint8)
    mp_img = mp.Image(image_format=mp.ImageFormat.SRGB, data=arr)

    result = detector.detect(mp_img)
    if not result.detections:
        return None

    # Největší detekce — u portrétu je to hlavní obličej, ne někdo v pozadí.
    best = max(result.detections, key=lambda d: d.bounding_box.width * d.bounding_box.height)
    bb = best.bounding_box
    return bb.origin_x, bb.origin_y, bb.origin_x + bb.width, bb.origin_y + bb.height


def crop_around(img: Image.Image, box: tuple[int, int, int, int], margin: float) -> Image.Image:
    """Čtvercový výřez kolem obličeje s okrajem.

    Čtverec, protože vstup embedderu je čtvercový — obdélníkový výřez by se
    stejně deformoval při resize a zkreslil by proporce obličeje.
    """
    left, top, right, bottom = box
    cx, cy = (left + right) / 2, (top + bottom) / 2
    side = max(right - left, bottom - top) * (1 + margin)

    half = side / 2
    l = int(max(0, cx - half))
    t = int(max(0, cy - half))
    r = int(min(img.width, cx + half))
    b = int(min(img.height, cy + half))
    return img.crop((l, t, r, b))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--images", nargs="+", required=True)
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--model", required=True, help="blaze_face_short_range.tflite")
    ap.add_argument("--margin", type=float, default=0.35,
                    help="okraj kolem detekce (0.35 = o 35 % větší strana)")
    ap.add_argument("--size", type=int, default=512)
    args = ap.parse_args()

    base_opts = mp.tasks.BaseOptions(model_asset_path=args.model)
    opts = mp.tasks.vision.FaceDetectorOptions(base_options=base_opts, min_detection_confidence=0.5)

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    ok, missed = 0, []
    with mp.tasks.vision.FaceDetector.create_from_options(opts) as detector:
        for p in map(Path, args.images):
            img = ImageOps.exif_transpose(Image.open(p)).convert("RGB")
            box = detect_face_box(img, detector)

            if box is None:
                # Nedetekovaný obličej se přeskočí, ne nahradí odhadem —
                # tichý fallback by do měření vnesl přesně ten šum, kvůli
                # kterému tenhle skript vznikl.
                missed.append(p.name)
                continue

            crop = crop_around(img, box, args.margin)
            crop = crop.resize((args.size, args.size), Image.LANCZOS)
            crop.save(out_dir / p.name)
            ok += 1

    log("hotovo", cropped=ok, missed=missed, out_dir=str(out_dir))
    return 1 if missed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001
        log("chyba", detail=f"{type(exc).__name__}: {exc}")
        raise

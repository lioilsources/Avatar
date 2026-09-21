#!/usr/bin/env python3
"""Trénink per-user SDXL LoRA (§4.2 plánu).

Běží na DGX Sparku ve venv ComfyUI:
    /home/ol1n/Code/ComfyUI/.venv/bin/python train_lora_sdxl.py --help

Proč vlastní trenér a ne kohya: plán připouští obojí. Ve venv ComfyUI už
jsou diffusers + peft ve verzích, které na aarch64/Blackwell prokazatelně
běží. Instalovat vedle toho kohya sd-scripts s vlastním stromem závislostí
je větší riziko než dvě stě řádků, které děláme přesně to, co potřebujeme.

Licenčně: trénuje se na vlastních datech uživatele nad SDXL base
(CreativeML Open RAIL++-M). Žádná non-komerční závislost.

Klíčové zrychlení: latenty z VAE i textové embeddingy se předpočítají
jednou dopředu. Každý krok je pak jen forward/backward přes UNet, což je
rozdíl mezi tréninkem v minutách a v desítkách minut.
"""

from __future__ import annotations

import argparse
import json
import math
import random
import sys
import time
from pathlib import Path

import numpy as np
import torch
import torch.nn.functional as F
from PIL import Image, ImageOps
from safetensors.torch import save_file


def log(msg: str, **kv: object) -> None:
    """Strukturovaný log na stdout — Go worker ho parsuje po řádcích."""
    rec = {"msg": msg, **kv}
    print(json.dumps(rec, ensure_ascii=False), flush=True)


# --------------------------------------------------------------------- data


def load_images(paths: list[Path], size: int) -> list[Image.Image]:
    """Načte fotky, srovná EXIF rotaci a ořízne na čtverec size×size.

    Střed ořezu je střed fotky. Je to hrubé, ale ve Fázi 1 ještě nemáme
    detekci obličeje — jakmile bude (Fáze 2), ořez se má dělat kolem ní.
    """
    out = []
    for p in paths:
        img = Image.open(p)
        img = ImageOps.exif_transpose(img).convert("RGB")

        short = min(img.width, img.height)
        left = (img.width - short) // 2
        top = (img.height - short) // 2
        img = img.crop((left, top, left + short, top + short))
        out.append(img.resize((size, size), Image.LANCZOS))
    return out


def to_tensor(img: Image.Image) -> torch.Tensor:
    """PIL → tensor v rozsahu [-1, 1], jak ho čeká VAE."""
    arr = np.asarray(img, dtype=np.uint8)
    t = torch.from_numpy(arr).permute(2, 0, 1).float() / 127.5 - 1.0
    return t


# ------------------------------------------------------------------ převod


def peft_to_kohya(
    lora_state: dict[str, torch.Tensor], rank: int, alpha: float
) -> dict[str, torch.Tensor]:
    """Převede peft/diffusers klíče na kohya formát, který čte ComfyUI.

    ComfyUI staví mapu `lora_unet_<diffusers cesta s podtržítky>` (viz
    comfy/lora.py model_lora_keys_unet), takže stačí nahradit tečky
    podtržítky a přejmenovat lora_A/lora_B na lora_down/lora_up.

    Alpha se ukládá ke každému modulu — bez ní ComfyUI použije rank
    jako alpha, což by změnilo efektivní sílu adaptéru.
    """
    out: dict[str, torch.Tensor] = {}

    for key, val in lora_state.items():
        # peft klíč: base_model.model.<cesta>.lora_A.default.weight
        k = key
        for prefix in ("base_model.model.", "unet."):
            if k.startswith(prefix):
                k = k[len(prefix) :]
        k = k.replace(".default", "")

        if ".lora_A.weight" in k:
            mod, suffix = k.split(".lora_A.weight")[0], "lora_down.weight"
        elif ".lora_B.weight" in k:
            mod, suffix = k.split(".lora_B.weight")[0], "lora_up.weight"
        else:
            continue

        name = "lora_unet_" + mod.replace(".", "_")
        out[f"{name}.{suffix}"] = val.to(torch.float16).contiguous()
        # Každá alpha musí být vlastní tensor — safetensors odmítá uložit
        # soubor, kde víc klíčů sdílí tutéž paměť.
        out[f"{name}.alpha"] = torch.tensor(alpha, dtype=torch.float32)

    return out


# ------------------------------------------------------------------ trénink


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--images", nargs="+", required=True, help="cesty ke vstupním fotkám")
    ap.add_argument("--output", required=True, help="cílový .safetensors")
    ap.add_argument("--checkpoint", required=True, help="SDXL base .safetensors")
    ap.add_argument("--trigger", default="ohwx person", help="trigger token do promptu")
    ap.add_argument("--class-word", default="person", help="třída pro popisky")
    ap.add_argument("--rank", type=int, default=16)
    ap.add_argument("--alpha", type=float, default=0.0, help="0 = rovno ranku")
    ap.add_argument("--steps", type=int, default=1000)
    ap.add_argument("--lr", type=float, default=1e-4)
    ap.add_argument("--resolution", type=int, default=768)
    ap.add_argument("--batch-size", type=int, default=1)
    ap.add_argument("--seed", type=int, default=42)
    args = ap.parse_args()

    torch.manual_seed(args.seed)
    random.seed(args.seed)

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    dtype = torch.bfloat16

    paths = [Path(p) for p in args.images]
    missing = [p for p in paths if not p.exists()]
    if missing:
        log("chyba", detail=f"chybí vstupní soubory: {missing}")
        return 1

    alpha = args.alpha if args.alpha > 0 else float(args.rank)
    t_start = time.time()

    log("start", images=len(paths), rank=args.rank, steps=args.steps,
        resolution=args.resolution, device=str(device))

    # --- načtení modelu ---------------------------------------------------
    from diffusers import StableDiffusionXLPipeline, DDPMScheduler

    pipe = StableDiffusionXLPipeline.from_single_file(
        args.checkpoint, torch_dtype=dtype, add_watermarker=False
    )
    noise_sched = DDPMScheduler.from_config(pipe.scheduler.config)
    log("model načten", after_s=round(time.time() - t_start, 1))

    unet = pipe.unet.to(device)
    vae = pipe.vae.to(device, dtype=torch.float32)  # fp16 VAE u SDXL dává NaN
    # Enkodéry musí na GPU taky — encode_prompt sice bere device pro vstupy,
    # ale váhy si nepřesune, takže by to spadlo na "tensors on different devices".
    pipe.text_encoder.to(device)
    pipe.text_encoder_2.to(device)
    unet.requires_grad_(False)
    vae.requires_grad_(False)

    # --- předpočet latentů a embeddingů -----------------------------------
    # Tohle je celý trik na rychlost: VAE a textové enkodéry se pustí jednou,
    # ne v každém kroku. U 3–5 fotek je to zlomek sekundy a ušetří to
    # většinu času tréninku.
    images = load_images(paths, args.resolution)
    prompt = f"a photo of {args.trigger}"

    with torch.no_grad():
        px = torch.stack([to_tensor(im) for im in images]).to(device, torch.float32)
        latents = vae.encode(px).latent_dist.sample() * vae.config.scaling_factor
        latents = latents.to(dtype)

        prompt_embeds, _, pooled, _ = pipe.encode_prompt(
            prompt=prompt, device=device, num_images_per_prompt=1,
            do_classifier_free_guidance=False,
        )

    log("předpočet hotov", latents=list(latents.shape), after_s=round(time.time() - t_start, 1))

    # Textové enkodéry ani VAE už nejsou potřeba — pryč z paměti.
    del vae, pipe.text_encoder, pipe.text_encoder_2, pipe.vae
    torch.cuda.empty_cache()

    # --- LoRA na attention projekce UNetu ---------------------------------
    from peft import LoraConfig, get_peft_model

    cfg = LoraConfig(
        r=args.rank,
        lora_alpha=alpha,
        # Jen attention projekce. Trénovat i konvoluce zvětší soubor
        # několikanásobně a na podobnost obličeje to nepřidá.
        target_modules=["to_q", "to_k", "to_v", "to_out.0"],
        lora_dropout=0.0,
        bias="none",
    )
    unet = get_peft_model(unet, cfg)

    params = [p for p in unet.parameters() if p.requires_grad]
    n_params = sum(p.numel() for p in params)
    log("lora připojena", trainable_params=n_params)

    # LoRA parametry musí být fp32 — v bf16 se u nich ztrácí přesnost
    # gradientu natolik, že trénink stagnuje.
    for p in params:
        p.data = p.data.float()

    opt = torch.optim.AdamW(params, lr=args.lr, weight_decay=1e-4, betas=(0.9, 0.999))
    sched = torch.optim.lr_scheduler.OneCycleLR(
        opt, max_lr=args.lr, total_steps=args.steps, pct_start=0.1, anneal_strategy="cos"
    )

    add_time_ids = torch.tensor(
        [[args.resolution, args.resolution, 0, 0, args.resolution, args.resolution]],
        dtype=dtype, device=device,
    )

    n = latents.shape[0]
    bs = min(args.batch_size, n)
    unet.train()

    t_train = time.time()
    loss_acc, loss_n = 0.0, 0

    for step in range(args.steps):
        idx = torch.tensor(random.choices(range(n), k=bs), device=device)
        lat = latents[idx]

        noise = torch.randn_like(lat)
        timesteps = torch.randint(
            0, noise_sched.config.num_train_timesteps, (bs,), device=device
        ).long()
        noisy = noise_sched.add_noise(lat, noise, timesteps)

        pred = unet(
            noisy, timesteps,
            encoder_hidden_states=prompt_embeds.expand(bs, -1, -1),
            added_cond_kwargs={
                "text_embeds": pooled.expand(bs, -1),
                "time_ids": add_time_ids.expand(bs, -1),
            },
        ).sample

        loss = F.mse_loss(pred.float(), noise.float())

        loss.backward()
        torch.nn.utils.clip_grad_norm_(params, 1.0)
        opt.step()
        sched.step()
        opt.zero_grad(set_to_none=True)

        loss_acc += loss.item()
        loss_n += 1

        # Progress po 5 % — Go worker to přeposílá do SSE.
        every = max(1, args.steps // 20)
        if (step + 1) % every == 0 or step == args.steps - 1:
            log("progress",
                step=step + 1, total=args.steps,
                percent=round((step + 1) / args.steps * 100, 1),
                loss=round(loss_acc / max(1, loss_n), 4),
                elapsed_s=round(time.time() - t_train, 1))
            loss_acc, loss_n = 0.0, 0

    train_s = time.time() - t_train

    # --- uložení ----------------------------------------------------------
    from peft import get_peft_model_state_dict

    state = get_peft_model_state_dict(unet)
    kohya = peft_to_kohya(state, args.rank, alpha)

    out_path = Path(args.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    save_file(kohya, str(out_path), metadata={
        "avatar_trigger": args.trigger,
        "avatar_rank": str(args.rank),
        "avatar_alpha": str(alpha),
        "avatar_steps": str(args.steps),
        "avatar_resolution": str(args.resolution),
        "ss_network_module": "networks.lora",
    })

    size_mb = out_path.stat().st_size / 1024 / 1024
    log("hotovo",
        output=str(out_path),
        size_mb=round(size_mb, 1),
        tensors=len(kohya),
        train_s=round(train_s, 1),
        total_s=round(time.time() - t_start, 1),
        trigger=args.trigger)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001 — chce to jeden řádek pro Go worker
        log("chyba", detail=f"{type(exc).__name__}: {exc}")
        raise

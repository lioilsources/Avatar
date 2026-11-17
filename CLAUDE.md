# Avatar (Face Avatar Studio)

<!-- Max 80 řádků. Co se nevejde, jde do docs/ a sem jeden odkaz. Veřejné repo: žádné LAN IP, domovské cesty, ID. -->

## Účel
Appka na portréty z vlastních fotek: Flutter (Android + iOS) + Go API, render přes ComfyUI na SPARKu (SDXL, IP-Adapter, depth ControlNet, per-user LoRA).

## Build a test
```
cd api && make test      # go test ./...
cd api && make lint      # go vet + gofmt
cd api && make build
cd app && flutter analyze
```
Lokální běh: `cd api && make tunnel` + `make run` (:8092), appka `flutter run --dart-define=AVATAR_API_URL=…`. Konfigurace přes `AVATAR_*` proměnné, tabulka v README.

## Mapa
- `data/` licenční registr, atributy, presety, vygenerované ComfyUI grafy
- `api/` Go backend (chi): preset store, comfy klient, trénink LoRA, recepty
- `scripts/` Python skripty pro GPU stroj (trénink LoRA, ořez obličeje, měření podobnosti)
- `app/` Flutter klient (quality gate, upload, výsledek)
- `docs/` výsledky fází a licenční audit

## Invarianty
- Licenční čistota je tvrdý constraint: modely mimo `data/licenses.yaml` nebo s tierem `blocked` se nesmí použít; hlídá to `go test ./internal/preset/`.
- Mazání identity maže i odvozená data včetně LoRA (GDPR).

## Odchylky
- Odchylka od lang.md: komentáře v kódu jsou česky.

## Kam dál
- `README.md` stav, rozjetí, API, naměřené časy
- `AVATAR_PLAN.md` plán a rozhodnutí
- `docs/licence-audit.md`, `docs/faze1-*.md`

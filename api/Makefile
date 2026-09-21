.PHONY: run build test lint workflows tunnel deploy-scripts clean

# Vývoj proti ComfyUI na DGX Sparku přes SSH tunel:
#   make tunnel   (v jednom terminálu)
#   make run      (v druhém)

AVATAR_DATA_DIR    ?= ../data
AVATAR_STORAGE_DIR ?= ./storage
AVATAR_COMFY_URL   ?= http://127.0.0.1:18188
SPARK              ?= spark

run:
	AVATAR_DATA_DIR=$(AVATAR_DATA_DIR) \
	AVATAR_STORAGE_DIR=$(AVATAR_STORAGE_DIR) \
	AVATAR_COMFY_URL=$(AVATAR_COMFY_URL) \
	AVATAR_TRAIN_SSH_HOST=$(SPARK) \
	go run ./cmd/avatar-api

# Tunel na ComfyUI, který běží host-native na sparku (:8188).
tunnel:
	ssh -N -L 18188:127.0.0.1:8188 $(SPARK)

# Trénovací a vyhodnocovací skripty musí být na stroji s GPU.
deploy-scripts:
	ssh $(SPARK) 'mkdir -p ~/avatar/work ~/Code/ComfyUI/models/loras/avatar'
	scp ../scripts/train_lora_sdxl.py ../scripts/eval_similarity.py $(SPARK):~/avatar/

build:
	CGO_ENABLED=0 go build -o bin/avatar-api ./cmd/avatar-api

test:
	go test ./...

lint:
	go vet ./...
	gofmt -l . | grep -v '^$$' && exit 1 || true

# Vygeneruje referenční ComfyUI grafy do data/workflows/ — dají se otevřít
# přímo v ComfyUI a ladit ručně.
workflows:
	go run ./tools/dumpworkflow -data $(AVATAR_DATA_DIR) -out $(AVATAR_DATA_DIR)/workflows

clean:
	rm -rf bin storage

# --- Fáze 1: měření podobnosti -----------------------------------------
# Vyžaduje `make deploy-scripts` a natrénovanou LoRA na Sparku.
eval-setup:
	ssh $(SPARK) 'python3 -m venv ~/avatar/venv-face && ~/avatar/venv-face/bin/pip install -q mediapipe pillow'
	ssh $(SPARK) 'cd ~/avatar && test -f blaze_face_short_range.tflite || wget -q https://storage.googleapis.com/mediapipe-models/face_detector/blaze_face_short_range/float16/1/blaze_face_short_range.tflite'

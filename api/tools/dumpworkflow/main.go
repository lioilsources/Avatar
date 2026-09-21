package main

// Nástroj: vygeneruje referenční ComfyUI workflow do data/workflows/,
// aby šel graf otevřít a ladit ručně v ComfyUI, ne jen přes API.
//
//	go run ./tools/dumpworkflow -preset studio-headshot

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ol1n/avatar/api/internal/attrs"
	"github.com/ol1n/avatar/api/internal/comfy"
	"github.com/ol1n/avatar/api/internal/face"
	"github.com/ol1n/avatar/api/internal/preset"
	"github.com/ol1n/avatar/api/internal/recipe"
)

func main() {
	data := flag.String("data", "../data", "adresář s daty")
	out := flag.String("out", "../data/workflows", "kam zapsat")
	flag.Parse()

	ps, err := preset.NewStore(filepath.Join(*data, "presets"), filepath.Join(*data, "licenses.yaml"))
	must(err)
	tb, err := attrs.Load(filepath.Join(*data, "attributes.yaml"))
	must(err)
	must(os.MkdirAll(*out, 0o755))

	for _, p := range ps.List(false) {
		g, _, err := comfy.Build(comfy.BuildInput{
			Preset:   p,
			Table:    tb,
			Registry: ps.Registry(),
			Recipe: &recipe.Recipe{
				ID:    "reference",
				Seed:  42,
				Attrs: attrs.Vector{},
			},
			Face:         &face.Face{ID: "reference", RefCrop: "ref.png"},
			RefImageName: "ref.png",
		})
		must(err)

		b, err := json.MarshalIndent(g, "", "  ")
		must(err)
		path := filepath.Join(*out, p.ID+".api.json")
		must(os.WriteFile(path, append(b, '\n'), 0o644))
		fmt.Println("zapsáno", path, len(g), "uzlů")
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "chyba:", err)
		os.Exit(1)
	}
}

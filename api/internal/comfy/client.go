package comfy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Client mluví s ComfyUI. Ten běží host-native na DGX Sparku na :8188,
// mimo Docker (viz AiStack/CLAUDE.md).
type Client struct {
	baseURL  string
	http     *http.Client
	clientID string
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: timeout},
		clientID: uuid.NewString(),
	}
}

// Health ověří, že ComfyUI odpovídá.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/system_stats", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comfyui vrátilo %d", resp.StatusCode)
	}
	return nil
}

// UploadImage nahraje fotku do ComfyUI input adresáře a vrátí jméno,
// pod kterým na ni odkazuje LoadImage uzel.
func (c *Client) UploadImage(ctx context.Context, name string, data []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	fw, err := w.CreateFormFile("image", filepath.Base(name))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	// overwrite=true: stejný face_id nahrává stejné jméno opakovaně,
	// bez toho by ComfyUI vyráběl kopie s příponou (1), (2)…
	_ = w.WriteField("overwrite", "true")
	_ = w.WriteField("type", "input")
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/upload/image", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload obrázku selhal (%d): %s", resp.StatusCode, truncate(body, 300))
	}

	var out struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("upload obrázku: nečekaná odpověď: %s", truncate(body, 300))
	}
	if out.Subfolder != "" {
		return out.Subfolder + "/" + out.Name, nil
	}
	return out.Name, nil
}

// Queue pošle graf do fronty a vrátí prompt_id.
func (c *Client) Queue(ctx context.Context, g Graph) (string, error) {
	payload := map[string]any{"prompt": g, "client_id": c.clientID}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/prompt", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// ComfyUI vrací u nevalidního grafu detailní popis, který je
		// při ladění workflow to jediné užitečné — nezahazuj ho.
		return "", fmt.Errorf("comfyui odmítlo graf (%d): %s", resp.StatusCode, truncate(body, 1500))
	}

	var out struct {
		PromptID string `json:"prompt_id"`
		Number   int    `json:"number"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("comfyui: nečekaná odpověď na /prompt: %s", truncate(body, 300))
	}
	if out.PromptID == "" {
		return "", fmt.Errorf("comfyui nevrátilo prompt_id: %s", truncate(body, 300))
	}
	return out.PromptID, nil
}

// Progress je jedna zpráva o postupu renderu.
type Progress struct {
	Stage   string  // "queued", "running", "done", "error"
	Node    string  // který uzel právě běží
	Percent float64 // 0–100 v rámci aktuálního uzlu
	Message string
}

// ImageRef ukazuje na výstupní obrázek v ComfyUI.
type ImageRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// Watch sleduje websocket ComfyUI a posílá průběh do kanálu.
//
// Websocket je jediný způsob, jak dostat skutečný postup po krocích —
// polling /history řekne jen "hotovo/nehotovo", což by v SSE znamenalo
// dvacet sekund ticha a uživatele, který neví, jestli appka nespadla.
//
// Vrací reference na obrázky po dokončení. Kanál progress zavírá volající
// není — zavírá ho tahle funkce při návratu.
func (c *Client) Watch(ctx context.Context, promptID, samplerNode string, progress chan<- Progress) ([]ImageRef, error) {
	defer close(progress)

	// Postup se přepočítává na jedno monotónní číslo za celý render.
	// Bez toho by ukazatel couval: každý uzel hlásí vlastních 0–100 %,
	// takže po doběhnutí depth preprocesoru na 100 % by sampler začal
	// znovu od nuly a uživatel by si myslel, že se něco pokazilo.
	var reported float64
	emit := func(p Progress) {
		if p.Percent > 0 {
			if p.Percent < reported {
				p.Percent = reported
			} else {
				reported = p.Percent
			}
		}
		send(progress, p)
	}

	wsURL, err := c.wsURL()
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		// Websocket je nice-to-have; když nejde, spadneme na polling.
		slog.Warn("comfyui websocket nedostupný, fallback na polling historie", "err", err)
		return c.pollHistory(ctx, promptID, emit)
	}
	defer conn.Close() //nolint:errcheck

	// Zavření socketu při zrušení kontextu — jinak by Watch visel
	// v ReadMessage, dokud se ComfyUI samo neozve.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	emit(Progress{Stage: "queued", Message: "ve frontě"})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Spojení spadlo uprostřed — výsledek ale může být hotový.
			slog.Warn("comfyui websocket přerušen, dotahuji přes historii", "err", err)
			return c.fetchHistory(ctx, promptID)
		}

		var msg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "progress":
			var d struct {
				Value    float64 `json:"value"`
				Max      float64 `json:"max"`
				PromptID string  `json:"prompt_id"`
				Node     string  `json:"node"`
			}
			if json.Unmarshal(msg.Data, &d) != nil || d.Max == 0 {
				continue
			}
			if d.PromptID != "" && d.PromptID != promptID {
				continue
			}
			emit(Progress{
				Stage:   "running",
				Node:    d.Node,
				Percent: mapNodeProgress(d.Node, samplerNode, d.Value/d.Max),
			})

		case "executing":
			var d struct {
				Node     *string `json:"node"`
				PromptID string  `json:"prompt_id"`
			}
			if json.Unmarshal(msg.Data, &d) != nil {
				continue
			}
			if d.PromptID != promptID {
				continue
			}
			// node == null znamená, že tenhle prompt doběhl.
			if d.Node == nil {
				emit(Progress{Stage: "done", Percent: 100})
				return c.fetchHistory(ctx, promptID)
			}
			emit(Progress{Stage: "running", Node: *d.Node})

		case "execution_error":
			var d struct {
				PromptID     string `json:"prompt_id"`
				NodeType     string `json:"node_type"`
				ExceptionMsg string `json:"exception_message"`
			}
			if json.Unmarshal(msg.Data, &d) != nil || d.PromptID != promptID {
				continue
			}
			return nil, fmt.Errorf("render selhal v uzlu %s: %s", d.NodeType, d.ExceptionMsg)
		}
	}
}

// pollHistory je fallback, když websocket není dostupný.
func (c *Client) pollHistory(ctx context.Context, promptID string, emit func(Progress)) ([]ImageRef, error) {
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			imgs, err := c.fetchHistory(ctx, promptID)
			if err == nil && len(imgs) > 0 {
				emit(Progress{Stage: "done", Percent: 100})
				return imgs, nil
			}
			emit(Progress{Stage: "running", Message: "renderuji"})
		}
	}
}

// fetchHistory vytáhne z historie reference na vygenerované obrázky.
func (c *Client) fetchHistory(ctx context.Context, promptID string) ([]ImageRef, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/history/"+promptID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	var hist map[string]struct {
		Outputs map[string]struct {
			Images []ImageRef `json:"images"`
		} `json:"outputs"`
		Status struct {
			StatusStr string `json:"status_str"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		return nil, err
	}

	entry, ok := hist[promptID]
	if !ok {
		return nil, fmt.Errorf("prompt %s není v historii", promptID)
	}
	if entry.Status.StatusStr == "error" {
		return nil, fmt.Errorf("render skončil chybou")
	}

	var out []ImageRef
	for _, o := range entry.Outputs {
		out = append(out, o.Images...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("render nevyprodukoval žádný obrázek")
	}
	return out, nil
}

// FetchImage stáhne hotový obrázek.
func (c *Client) FetchImage(ctx context.Context, ref ImageRef) ([]byte, error) {
	q := url.Values{}
	q.Set("filename", ref.Filename)
	q.Set("subfolder", ref.Subfolder)
	q.Set("type", ref.Type)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/view?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stažení obrázku selhalo: %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// wsURL přeloží http(s) base na ws(s) endpoint s naším client_id.
func (c *Client) wsURL() (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	u.RawQuery = "clientId=" + c.clientID
	return u.String(), nil
}

// preSamplerBudget je podíl ukazatele vyhrazený pro přípravné uzly
// (depth preprocesor, CLIP vision encode). Sampling dostane zbytek.
//
// Naměřeno na GB10: příprava zabere kolem 8 s, sampling 8–13 s podle
// profilu. 20 % je hrubý, ale stabilnější odhad než snaha to počítat
// z reálných časů, které se liší podle toho, co je zrovna v paměti.
const preSamplerBudget = 0.20

// mapNodeProgress přepočítá postup jednoho uzlu na postup celého renderu.
func mapNodeProgress(node, samplerNode string, frac float64) float64 {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	if node == samplerNode {
		return (preSamplerBudget + (1-preSamplerBudget)*frac) * 100
	}
	return preSamplerBudget * frac * 100
}

// send pošle progress bez blokování — pomalý konzument nesmí zdržet render.
func send(ch chan<- Progress, p Progress) {
	select {
	case ch <- p:
	default:
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

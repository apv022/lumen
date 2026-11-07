package gate

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// brandOverrides are DSH paths the gate answers itself with Lumen assets.
var brandOverrides = map[string]string{
	"/favicon.svg":          "assets/icon.svg",
	"/favicon-dark.svg":     "assets/icon.svg",
	"/manifest.webmanifest": "assets/manifest.webmanifest",
}

// serveBrand handles icon and manifest paths. It reports whether it did.
func serveBrand(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/apple-touch-icon.png" || r.URL.Path == "/favicon.ico" {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(appIconPNG())
		return true
	}
	file, ok := brandOverrides[r.URL.Path]
	if !ok {
		return false
	}
	data, err := assetFS.ReadFile(file)
	if err != nil {
		return false
	}
	switch {
	case strings.HasSuffix(file, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	case strings.HasSuffix(file, ".webmanifest"):
		w.Header().Set("Content-Type", "application/manifest+json")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
	return true
}

var (
	titleRE   = regexp.MustCompile(`(?is)<title>.*?</title>`)
	headEndRE = regexp.MustCompile(`(?i)</head>`)
)

// injection is added to every DSH HTML page.
const injection = `<link rel="stylesheet" href="/lumen/assets/theme.css">` +
	`<meta name="theme-color" content="#ff9a5a">` +
	`<script src="/lumen/assets/rebrand.js" defer></script>`

// rebrandHTML renames the document and injects the Lumen theme.
func rebrandHTML(doc []byte) []byte {
	doc = titleRE.ReplaceAll(doc, []byte("<title>Lumen</title>"))
	if loc := headEndRE.FindIndex(doc); loc != nil {
		var out bytes.Buffer
		out.Grow(len(doc) + len(injection))
		out.Write(doc[:loc[0]])
		out.WriteString(injection)
		out.Write(doc[loc[0]:])
		return out.Bytes()
	}
	return append([]byte(injection), doc...)
}

// rewriteHTMLResponse applies rebrandHTML to an uncompressed HTML response.
func rewriteHTMLResponse(resp *http.Response) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	body = rebrandHTML(body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("ETag")
	return nil
}

var (
	iconOnce sync.Once
	iconPNG  []byte
)

// appIconPNG draws the Lumen eye (a vertical oval with a circle in the
// center, on cream) as a 180x180 PNG for home-screen icons. Drawn in code so
// the repo needs no binary assets or image tools.
func appIconPNG() []byte {
	iconOnce.Do(func() {
		const size, ss = 180, 4 // 4x supersampling for smooth edges
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		top := color.NRGBA{0xff, 0xc5, 0x6b, 0xff}
		mid := color.NRGBA{0xff, 0x9a, 0x5a, 0xff}
		bot := color.NRGBA{0xff, 0x7a, 0x7a, 0xff}
		cream := color.NRGBA{0xff, 0xf7, 0xee, 0xff}
		bg := color.NRGBA{0xff, 0xf3, 0xe6, 0xff}
		cx, cy := float64(size)/2, float64(size)/2
		rx, ry, rc := size*0.25, size*0.40, size*0.135
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				var r, g, b, n float64
				for sy := 0; sy < ss; sy++ {
					for sx := 0; sx < ss; sx++ {
						px := float64(x) + (float64(sx)+.5)/ss
						py := float64(y) + (float64(sy)+.5)/ss
						c := bg
						dx, dy := (px-cx)/rx, (py-cy)/ry
						if dx*dx+dy*dy <= 1 {
							t := (py - (cy - ry)) / (2 * ry)
							if t < .55 {
								c = lerp(top, mid, t/.55)
							} else {
								c = lerp(mid, bot, (t-.55)/.45)
							}
							if (px-cx)*(px-cx)+(py-cy)*(py-cy) <= rc*rc {
								c = cream
							}
						}
						r += float64(c.R)
						g += float64(c.G)
						b += float64(c.B)
						n++
					}
				}
				img.SetNRGBA(x, y, color.NRGBA{uint8(r / n), uint8(g / n), uint8(b / n), 0xff})
			}
		}
		var buf bytes.Buffer
		png.Encode(&buf, img)
		iconPNG = buf.Bytes()
	})
	return iconPNG
}

func lerp(a, b color.NRGBA, t float64) color.NRGBA {
	t = max(0, min(1, t))
	m := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.NRGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 0xff}
}

package matchviewer

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed static
var staticFiles embed.FS

// MinimapAsset is a minimap image loaded from an assets directory.
type MinimapAsset struct {
	data        []byte
	contentType string
}

// FindMinimap looks for <dir>/minimap/<id>.<ext> in each candidate assets
// directory and returns the first image found.
func FindMinimap(dirs []string, id string) (*MinimapAsset, string) {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, ext := range []string{"webp", "png", "jpg"} {
			path := filepath.Join(dir, "minimap", id+"."+ext)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			ct := "image/" + ext
			if ext == "jpg" {
				ct = "image/jpeg"
			}
			return &MinimapAsset{data: data, contentType: ct}, path
		}
	}
	return nil, ""
}

func (a *MinimapAsset) dataURI() string {
	return "data:" + a.contentType + ";base64," + base64.StdEncoding.EncodeToString(a.data)
}

type server struct {
	mux     *http.ServeMux
	payload []byte
	minimap *MinimapAsset
	refs    *ReferenceStore
}

// newServer serves the viewer, the match document and the minimap image. The
// document is encoded once; the viewer fetches it at api/match.
func NewHandler(data *MatchData, minimap *MinimapAsset, refs *ReferenceStore) (*server, error) {
	if minimap != nil {
		data.Match.Map.Image = "assets/minimap"
	}
	if refs == nil {
		refs = &ReferenceStore{}
		if data.Reference != nil {
			refs.Set(data.Reference)
		}
	}
	// The served document carries no embedded reference; api/reference does.
	embedded := data.Reference
	data.Reference = nil
	payload, err := json.Marshal(data)
	data.Reference = embedded
	if err != nil {
		return nil, err
	}

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}

	s := &server{mux: http.NewServeMux(), payload: payload, minimap: minimap, refs: refs}
	s.mux.Handle("/", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("GET /api/match", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.payload)
	})
	// The reference data is fetched in the background after startup; until it
	// is ready the viewer is told to poll again.
	s.mux.HandleFunc("GET /api/reference", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ref, ready := s.refs.Get()
		if !ready {
			w.Write([]byte(`{"pending":true}`))
			return
		}
		if ref == nil {
			w.Write([]byte(`{"disabled":true}`))
			return
		}
		json.NewEncoder(w).Encode(ref)
	})
	s.mux.HandleFunc("GET /assets/minimap", func(w http.ResponseWriter, r *http.Request) {
		if s.minimap == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", s.minimap.contentType)
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(s.minimap.data)
	})
	return s, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ExportHTML renders a self-contained viewer page: the stylesheet, script and
// match document are inlined, and the minimap (if any) is embedded as a data
// URI. Hero and item icons are still loaded from Valve's CDN.
func ExportHTML(data *MatchData, minimap *MinimapAsset) ([]byte, error) {
	read := func(name string) (string, error) {
		b, err := staticFiles.ReadFile("static/" + name)
		return string(b), err
	}
	page, err := read("index.html")
	if err != nil {
		return nil, err
	}
	css, err := read("style.css")
	if err != nil {
		return nil, err
	}
	js, err := read("app.js")
	if err != nil {
		return nil, err
	}

	if minimap != nil {
		data.Match.Map.Image = minimap.dataURI()
	} else {
		data.Match.Map.Image = ""
	}
	// json.Marshal escapes <, > and & so the document is safe inside a script
	// element even if a chat line contains "</script>".
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	const styleTag = `<link rel="stylesheet" href="style.css">`
	const scriptTag = `<script src="app.js"></script>`
	if !strings.Contains(page, styleTag) || !strings.Contains(page, scriptTag) {
		return nil, fmt.Errorf("index.html is missing the expected asset tags")
	}
	var out bytes.Buffer
	page = strings.Replace(page, styleTag, "<style>\n"+css+"\n</style>", 1)
	page = strings.Replace(page, scriptTag,
		"<script>window.MATCH = "+string(payload)+";</script>\n<script>\n"+js+"\n</script>", 1)
	out.WriteString(page)
	return out.Bytes(), nil
}

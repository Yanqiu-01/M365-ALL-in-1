package web

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultWallpaperDir = `D:\壁纸\壁纸合集`

// Skip files this large in the random picker so Cloudflare/Caddy
// does not sit on a 30MB JPEG through the tunnel.
const wallpaperMaxBytes = 8 << 20

var wallpaperTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
}

type wallpaperIndex struct {
	mu      sync.Mutex
	root    string
	files   []string
	scanned time.Time
}

var wallpapers = &wallpaperIndex{}

func wallpaperDir() string {
	if v := strings.TrimSpace(os.Getenv("M365_WALLPAPER_DIR")); v != "" {
		return v
	}
	return defaultWallpaperDir
}

func (idx *wallpaperIndex) list() []string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	root := wallpaperDir()
	if root != idx.root || time.Since(idx.scanned) > 5*time.Minute || len(idx.files) == 0 {
		idx.root = root
		idx.files = scanWallpapers(root)
		idx.scanned = time.Now()
		log.Printf("[wallpaper] indexed %d images under %s", len(idx.files), root)
	}
	out := make([]string, len(idx.files))
	copy(out, idx.files)
	return out
}

func scanWallpapers(root string) []string {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	var files []string
	_ = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil || fi.IsDir() {
			return nil
		}
		if fi.Size() == 0 || fi.Size() > wallpaperMaxBytes {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(fi.Name()))
		if _, ok := wallpaperTypes[ext]; !ok {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files
}

func (s *Server) wallpaperInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	files := wallpapers.list()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count": len(files),
		"dir":   wallpaperDir(),
	})
}

func (s *Server) wallpaperRandom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	files := wallpapers.list()
	if len(files) == 0 {
		http.Error(w, "no wallpapers", http.StatusNotFound)
		return
	}
	path := files[rand.Intn(len(files))]
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	ctype := wallpaperTypes[ext]
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Wallpaper-Name", filepath.Base(path))
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

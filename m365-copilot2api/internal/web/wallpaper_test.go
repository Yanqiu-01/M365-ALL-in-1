package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWallpaperRandomServesIndexedFile(t *testing.T) {
	dir := t.TempDir()
	body := []byte{0xff, 0xd8, 0xff, 0xd9} // tiny JPEG SOI/EOI
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_WALLPAPER_DIR", dir)
	wallpapers = &wallpaperIndex{}

	s := &Server{}
	rec := httptest.NewRecorder()
	s.wallpaperRandom(rec, httptest.NewRequest(http.MethodGet, "/wall/random", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("content-type=%q", got)
	}
	if rec.Body.Len() != len(body) {
		t.Fatalf("bytes=%d", rec.Body.Len())
	}
}

func TestWallpaperInfoCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_WALLPAPER_DIR", dir)
	wallpapers = &wallpaperIndex{}

	s := &Server{}
	rec := httptest.NewRecorder()
	s.wallpaperInfo(rec, httptest.NewRequest(http.MethodGet, "/wall/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 {
		t.Fatalf("count=%d", out.Count)
	}
}

func TestWallpaperRandomEmptyDir(t *testing.T) {
	t.Setenv("M365_WALLPAPER_DIR", t.TempDir())
	wallpapers = &wallpaperIndex{}
	s := &Server{}
	rec := httptest.NewRecorder()
	s.wallpaperRandom(rec, httptest.NewRequest(http.MethodGet, "/wall/random", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

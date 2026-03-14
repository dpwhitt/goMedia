package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAddr      = ":8080"
	defaultMediaRoot = "./media"
)

type MediaItem struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Path          string    `json:"-"`
	URL           string    `json:"url"`
	HLSURL        string    `json:"hlsUrl"`
	Size          int64     `json:"size"`
	Modified      time.Time `json:"modified"`
	Category      string    `json:"category"`
	Ext           string    `json:"ext"`
	ThumbnailURL  string    `json:"thumbnailUrl"`
	ThumbnailPath string    `json:"-"`
	DurationSecs  int64     `json:"durationSecs"`
	DurationLabel string    `json:"durationLabel"`
	IsHLSReady    bool      `json:"isHlsReady"`
}

type Server struct {
	mediaRoot    string
	hlsRoot      string
	hlsEnabled   bool
	items        map[string]MediaItem
	list         []MediaItem
	streamSecret []byte
	mu           sync.Mutex
	hlsLocks     map[string]*sync.Mutex
}

func main() {
	_ = mime.AddExtensionType(".mp4", "video/mp4")
	_ = mime.AddExtensionType(".m4v", "video/mp4")
	_ = mime.AddExtensionType(".mov", "video/quicktime")
	_ = mime.AddExtensionType(".webm", "video/webm")
	_ = mime.AddExtensionType(".mkv", "video/x-matroska")
	_ = mime.AddExtensionType(".m3u8", "application/vnd.apple.mpegurl")
	_ = mime.AddExtensionType(".ts", "video/mp2t")

	mediaRootFlag := flag.String("media-root", defaultMediaRoot, "media library root folder")
	addrFlag := flag.String("addr", defaultAddr, "http listen address")
	hlsFlag := flag.Bool("hls", true, "enable HLS segmented streaming: true or false")
	streamSecretFlag := flag.String("stream-secret", "", "optional secret used to sign direct stream URLs")
	flag.Parse()

	mediaRoot := strings.TrimSpace(*mediaRootFlag)
	if mediaRoot == "" {
		mediaRoot = defaultMediaRoot
	}
	addr := strings.TrimSpace(*addrFlag)
	if addr == "" {
		addr = defaultAddr
	}
	hlsEnabled := *hlsFlag
	secret := buildStreamSecret(strings.TrimSpace(*streamSecretFlag))
	hlsRoot := filepath.Join(mediaRoot, ".hls")

	if err := os.MkdirAll(mediaRoot, 0o755); err != nil {
		log.Fatalf("failed to create media root: %v", err)
	}
	if hlsEnabled {
		if err := os.MkdirAll(hlsRoot, 0o755); err != nil {
			log.Fatalf("failed to create hls root: %v", err)
		}
	}

	srv, err := NewServer(mediaRoot, hlsRoot, secret, hlsEnabled)
	if err != nil {
		log.Fatalf("failed to initialize server: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleHome)
	mux.HandleFunc("/watch/", srv.handleWatch)
	mux.HandleFunc("/api/media", srv.handleAPI)
	mux.HandleFunc("/stream/", srv.handleStream)
	mux.HandleFunc("/thumb/", srv.handleThumb)
	mux.HandleFunc("/hls/", srv.handleHLS)
	mux.HandleFunc("/healthz", srv.handleHealth)

	server := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("media root: %s", mediaRoot)
	log.Printf("hls enabled: %t", hlsEnabled)
	if hlsEnabled {
		log.Printf("hls root: %s", hlsRoot)
	}
	log.Printf("open http://localhost%s", addr)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func NewServer(mediaRoot, hlsRoot string, secret []byte, hlsEnabled bool) (*Server, error) {
	items, err := scanMedia(mediaRoot, hlsRoot, hlsEnabled)
	if err != nil {
		return nil, err
	}

	itemMap := make(map[string]MediaItem, len(items))
	for _, item := range items {
		itemMap[item.ID] = item
	}

	return &Server{
		mediaRoot:    mediaRoot,
		hlsRoot:      hlsRoot,
		hlsEnabled:   hlsEnabled,
		items:        itemMap,
		list:         items,
		streamSecret: secret,
		hlsLocks:     make(map[string]*sync.Mutex),
	}, nil
}

func scanMedia(root, hlsRoot string, hlsEnabled bool) ([]MediaItem, error) {
	supported := map[string]bool{
		".mp4":  true,
		".m4v":  true,
		".webm": true,
		".mov":  true,
		".mkv":  true,
	}

	items := make([]MediaItem, 0, 64)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if hlsEnabled && samePath(path, hlsRoot) {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !supported[ext] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = d.Name()
		}
		rel = filepath.ToSlash(rel)

		id := makeSafeID(rel)
		title := prettifyTitle(strings.TrimSuffix(filepath.Base(d.Name()), filepath.Ext(d.Name())))
		thumbPath := findThumbnail(path)
		durationSecs := probeDurationSeconds(path)
		isHLSReady := false
		if hlsEnabled {
			isHLSReady = fileExists(filepath.Join(hlsRoot, id, "index.m3u8"))
		}

		items = append(items, MediaItem{
			ID:            id,
			Title:         title,
			Path:          path,
			URL:           "/stream/" + id,
			HLSURL:        "/hls/" + id + "/index.m3u8",
			Size:          info.Size(),
			Modified:      info.ModTime(),
			Category:      detectCategory(rel),
			Ext:           ext,
			ThumbnailURL:  buildThumbnailURL(id, thumbPath),
			ThumbnailPath: thumbPath,
			DurationSecs:  durationSecs,
			DurationLabel: formatDuration(durationSecs),
			IsHLSReady:    isHLSReady,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Category != items[j].Category {
			if items[i].Category == "Recently Added" {
				return true
			}
			if items[j].Category == "Recently Added" {
				return false
			}
			return items[i].Category < items[j].Category
		}
		return strings.ToLower(items[i].Title) < strings.ToLower(items[j].Title)
	})

	return items, nil
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, homeHTML)
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	groups := make(map[string][]MediaItem)
	for _, item := range s.list {
		groups[item.Category] = append(groups[item.Category], item)
	}

	categories := make([]string, 0, len(groups))
	for category := range groups {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	moveToFront(categories, "Recently Added")

	payload := struct {
		Categories []string               `json:"categories"`
		Items      map[string][]MediaItem `json:"items"`
		HLSEnabled bool                   `json:"hlsEnabled"`
	}{
		Categories: categories,
		Items:      groups,
		HLSEnabled: s.hlsEnabled,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/watch/")
	item, ok := s.items[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	fallbackURL := s.signedStreamURL(id, 6*time.Hour)
	page := strings.ReplaceAll(watchHTML, "__TITLE__", htmlEscape(item.Title))
	page = strings.ReplaceAll(page, "__HOME_URL__", "/")
	page = strings.ReplaceAll(page, "__HLS_URL__", htmlEscape(item.HLSURL))
	page = strings.ReplaceAll(page, "__FALLBACK_URL__", htmlEscape(fallbackURL))
	page = strings.ReplaceAll(page, "__HLS_ENABLED__", strconv.FormatBool(s.hlsEnabled))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "same-origin")
	_, _ = io.WriteString(w, page)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/stream/")
	if !s.validateStreamRequest(r, id) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	item, ok := s.items[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(item.Path)
	if err != nil {
		http.Error(w, "failed to open media", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "failed to stat media", http.StatusInternalServerError)
		return
	}

	contentType := mime.TypeByExtension(item.Ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store, max-age=0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(item.Path), info.ModTime(), file)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/thumb/")
	item, ok := s.items[id]
	if !ok || strings.TrimSpace(item.ThumbnailPath) == "" {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(item.ThumbnailPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(item.ThumbnailPath)))
	if contentType == "" {
		contentType = "image/jpeg"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, filepath.Base(item.ThumbnailPath), info.ModTime(), file)
}

func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	if !s.hlsEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel := strings.TrimPrefix(r.URL.Path, "/hls/")
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	cleanRel := filepath.Clean(rel)
	cleanRel = filepath.ToSlash(cleanRel)
	if cleanRel == "." || strings.HasPrefix(cleanRel, "../") || strings.Contains(cleanRel, "/../") {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(cleanRel, "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if _, ok := s.items[id]; !ok {
		http.NotFound(w, r)
		return
	}

	if err := s.ensureHLS(id); err != nil {
		http.Error(w, "failed to prepare hls stream: "+err.Error(), http.StatusInternalServerError)
		return
	}

	target := filepath.Join(s.hlsRoot, filepath.FromSlash(cleanRel))
	if !isWithinBase(s.hlsRoot, target) || !fileExists(target) {
		http.NotFound(w, r)
		return
	}

	ext := strings.ToLower(filepath.Ext(target))
	contentType := mime.TypeByExtension(ext)
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, target)
}

func (s *Server) ensureHLS(id string) error {
	if !s.hlsEnabled {
		return errors.New("hls is disabled")
	}
	item, ok := s.items[id]
	if !ok {
		return errors.New("media item not found")
	}

	playlist := filepath.Join(s.hlsRoot, id, "index.m3u8")
	if fileExists(playlist) {
		s.markHLSReady(id)
		return nil
	}

	lock := s.getHLSLock(id)
	lock.Lock()
	defer lock.Unlock()

	if fileExists(playlist) {
		s.markHLSReady(id)
		return nil
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("ffmpeg not found on PATH")
	}

	outDir := filepath.Join(s.hlsRoot, id)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	cmd := exec.Command(
		ffmpegPath,
		"-y",
		"-i", item.Path,
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-c:a", "aac",
		"-ac", "2",
		"-ar", "48000",
		"-b:a", "128k",
		"-start_number", "0",
		"-hls_time", "6",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(outDir, "segment_%05d.ts"),
		"-f", "hls",
		playlist,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %s", strings.TrimSpace(string(output)))
	}

	s.markHLSReady(id)
	return nil
}

func (s *Server) getHLSLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.hlsLocks[id]
	if !ok {
		lock = &sync.Mutex{}
		s.hlsLocks[id] = lock
	}
	return lock
}

func (s *Server) markHLSReady(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return
	}
	item.IsHLSReady = true
	s.items[id] = item
	for i := range s.list {
		if s.list[i].ID == id {
			s.list[i] = item
			break
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"mediaCount": len(s.list),
		"hlsEnabled": s.hlsEnabled,
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func buildStreamSecret(configured string) []byte {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return []byte(configured)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err == nil {
		return []byte(base64.RawURLEncoding.EncodeToString(buf))
	}
	return []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
}

func (s *Server) signedStreamURL(id string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	payload := id + ":" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.streamSecret)
	_, _ = mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return "/stream/" + id + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + sig
}

func (s *Server) validateStreamRequest(r *http.Request, id string) bool {
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || exp < time.Now().Unix() {
		return false
	}
	payload := id + ":" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.streamSecret)
	_, _ = mac.Write([]byte(payload))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(r.URL.Query().Get("sig")))
}

func makeSafeID(value string) string {
	value = filepath.ToSlash(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return out
}

func detectCategory(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 1 && strings.TrimSpace(parts[0]) != "" {
		return prettifyTitle(parts[0])
	}
	return "Recently Added"
}

func moveToFront(values []string, target string) {
	index := -1
	for i, value := range values {
		if value == target {
			index = i
			break
		}
	}
	if index <= 0 {
		return
	}
	value := values[index]
	copy(values[1:index+1], values[0:index])
	values[0] = value
}

func prettifyTitle(s string) string {
	replacer := strings.NewReplacer("_", " ", ".", " ", "-", " ")
	s = replacer.Replace(s)
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return "Untitled"
	}
	words := strings.Fields(strings.ToLower(s))
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(s)
}

func findThumbnail(videoPath string) string {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
	candidates := []string{
		filepath.Join(dir, base+".jpg"),
		filepath.Join(dir, base+".jpeg"),
		filepath.Join(dir, base+".png"),
		filepath.Join(dir, base+".webp"),
		filepath.Join(dir, "poster.jpg"),
		filepath.Join(dir, "poster.png"),
		filepath.Join(dir, "folder.jpg"),
		filepath.Join(dir, "folder.png"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func buildThumbnailURL(id, thumbPath string) string {
	if strings.TrimSpace(thumbPath) == "" {
		return ""
	}
	return "/thumb/" + id
}

func probeDurationSeconds(path string) int64 {
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0
	}
	cmd := exec.Command(ffprobePath, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0
	}
	return int64(math.Round(seconds))
}

func formatDuration(totalSeconds int64) string {
	if totalSeconds <= 0 {
		return ""
	}
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return aa == bb
}

func isWithinBase(base, target string) bool {
	absBase, err1 := filepath.Abs(base)
	absTarget, err2 := filepath.Abs(target)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != ".." && !strings.HasPrefix(rel, "../")
}

const homeHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Homeflix</title>
  <style>
    :root {
      --bg: #141414;
      --panel: #181818;
      --text: #ffffff;
      --muted: #b3b3b3;
      --accent: #e50914;
      --card: #202020;
      --shadow: rgba(0,0,0,0.45);
      --line: rgba(255,255,255,0.08);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: Arial, Helvetica, sans-serif;
      background: linear-gradient(180deg, #101010 0%, #141414 18%, #141414 100%);
      color: var(--text);
    }
    header {
      position: sticky;
      top: 0;
      z-index: 10;
      background: linear-gradient(180deg, rgba(0,0,0,0.94), rgba(0,0,0,0.68), rgba(0,0,0,0));
      padding: 18px 28px 16px;
      backdrop-filter: blur(6px);
    }
    .brand {
      font-size: 30px;
      font-weight: 800;
      color: var(--accent);
      letter-spacing: 1px;
    }
    .sub {
      color: var(--muted);
      margin-top: 6px;
      font-size: 14px;
    }
    .hero {
      padding: 30px 28px 18px;
      display: grid;
      gap: 12px;
    }
    .hero h1 {
      margin: 0;
      font-size: clamp(30px, 5vw, 54px);
      line-height: 1.02;
    }
    .hero p {
      margin: 0;
      max-width: 880px;
      color: #d8d8d8;
      font-size: 16px;
      line-height: 1.55;
    }
    .toolbar {
      display: flex;
      gap: 12px;
      flex-wrap: wrap;
      align-items: center;
      margin-top: 10px;
    }
    input[type="search"] {
      width: min(430px, 100%);
      padding: 12px 14px;
      border-radius: 8px;
      border: 1px solid #353535;
      background: rgba(0,0,0,0.45);
      color: #fff;
      outline: none;
    }
    .toggle {
      display: inline-flex;
      align-items: center;
      gap: 10px;
      padding: 10px 12px;
      border: 1px solid #353535;
      border-radius: 8px;
      background: rgba(0,0,0,0.35);
      color: #fff;
      user-select: none;
    }
    .toggle input {
      width: 18px;
      height: 18px;
      accent-color: #e50914;
    }
    main {
      padding: 8px 0 40px;
    }
    .row {
      margin-bottom: 22px;
    }
    .row h2 {
      padding: 0 28px;
      margin: 0 0 14px;
      font-size: 22px;
    }
    .rail {
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(220px, 260px);
      gap: 16px;
      overflow-x: auto;
      padding: 0 28px 10px;
      scroll-behavior: smooth;
    }
    .rail::-webkit-scrollbar {
      height: 10px;
    }
    .rail::-webkit-scrollbar-thumb {
      background: #333;
      border-radius: 999px;
    }
    .card {
      text-decoration: none;
      color: inherit;
      display: block;
      background: linear-gradient(180deg, #232323, #171717);
      border-radius: 12px;
      overflow: hidden;
      box-shadow: 0 10px 24px var(--shadow);
      transition: transform 0.18s ease, box-shadow 0.18s ease;
      border: 1px solid var(--line);
    }
    .card:hover, .card:focus {
      transform: scale(1.04);
      box-shadow: 0 18px 40px rgba(0,0,0,0.6);
    }
    .thumb {
      aspect-ratio: 16 / 9;
      background:
        radial-gradient(circle at top right, rgba(229,9,20,0.35), transparent 35%),
        linear-gradient(135deg, #3d3d3d, #111 62%);
      display: grid;
      place-items: center;
      font-size: 54px;
      font-weight: 800;
      color: rgba(255,255,255,0.92);
      letter-spacing: 2px;
      user-select: none;
      overflow: hidden;
    }
    .thumb img {
      width: 100%;
      height: 100%;
      object-fit: cover;
      display: block;
    }
    .meta {
      padding: 14px;
    }
    .title {
      font-size: 17px;
      font-weight: 700;
      line-height: 1.3;
      margin-bottom: 8px;
    }
    .details {
      color: var(--muted);
      font-size: 13px;
      display: flex;
      justify-content: space-between;
      gap: 8px;
      flex-wrap: wrap;
    }
    .empty {
      padding: 24px 28px;
      color: var(--muted);
    }
  </style>
</head>
<body>
  <header>
    <div class="brand">HOMEFLIX</div>
    <div class="sub">Go media server with thumbnails, duration metadata, direct playback, and optional HLS segmented streaming.</div>
  </header>

  <section class="hero">
    <h1>Browse and stream your local media.</h1>
    <p>Start the server with <code>-hls=true</code> or <code>-hls=false</code>. The switch below reflects the current server mode.</p>
    <div class="toolbar">
      <input id="search" type="search" placeholder="Search titles..." aria-label="Search titles" />
      <label class="toggle">
        <input id="hls-toggle" type="checkbox" disabled>
        <span>HLS processing enabled</span>
      </label>
    </div>
  </section>

  <main id="app">
    <div class="empty">Loading library...</div>
  </main>

  <script>
    const app = document.getElementById('app');
    const search = document.getElementById('search');
    const hlsToggle = document.getElementById('hls-toggle');
    let state = { categories: [], items: {}, hlsEnabled: false };

    function formatSize(bytes) {
      if (!Number.isFinite(bytes) || bytes <= 0) return 'Unknown size';
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let value = bytes;
      let unit = 0;
      while (value >= 1024 && unit < units.length - 1) {
        value = value / 1024;
        unit++;
      }
      const decimals = value >= 10 || unit === 0 ? 0 : 1;
      return value.toFixed(decimals) + ' ' + units[unit];
    }

    function initials(title) {
      const chars = String(title)
        .split(/\s+/)
        .filter(Boolean)
        .slice(0, 2)
        .map(function(part) { return part.charAt(0); })
        .join('')
        .toUpperCase();
      return chars || '▶';
    }

    function escapeHtml(value) {
      return String(value)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    }

    function buildThumb(item) {
      if (item.thumbnailUrl) {
        return '<img src="' + encodeURI(item.thumbnailUrl) + '" alt="' + escapeHtml(item.title) + '" loading="lazy" draggable="false">';
      }
      return escapeHtml(initials(item.title));
    }

    function render() {
      const q = search.value.trim().toLowerCase();
      let visibleRows = 0;
      app.innerHTML = '';
      hlsToggle.checked = !!state.hlsEnabled;

      state.categories.forEach(function(category) {
        const sourceItems = state.items[category] || [];
        const filtered = sourceItems.filter(function(item) {
          return String(item.title).toLowerCase().indexOf(q) !== -1;
        });

        if (filtered.length === 0) {
          return;
        }

        visibleRows++;

        const row = document.createElement('section');
        row.className = 'row';

        const heading = document.createElement('h2');
        heading.textContent = category;
        row.appendChild(heading);

        const rail = document.createElement('div');
        rail.className = 'rail';

        filtered.forEach(function(item) {
          const card = document.createElement('a');
          card.className = 'card';
          card.href = '/watch/' + encodeURIComponent(item.id);

          const mode = state.hlsEnabled
            ? (item.isHlsReady ? 'HLS ready' : 'HLS on demand')
            : 'Direct only';

          card.innerHTML =
            '<div class="thumb">' + buildThumb(item) + '</div>' +
            '<div class="meta">' +
              '<div class="title">' + escapeHtml(item.title) + '</div>' +
              '<div class="details">' +
                '<span>' + formatSize(item.size) + '</span>' +
                '<span>' + escapeHtml(item.durationLabel || '') + '</span>' +
                '<span>' + mode + '</span>' +
              '</div>' +
            '</div>';

          rail.appendChild(card);
        });

        row.appendChild(rail);
        app.appendChild(row);
      });

      if (visibleRows === 0) {
        app.innerHTML = '<div class="empty">No matching titles found.</div>';
      }
    }

    async function loadLibrary() {
      try {
        const response = await fetch('/api/media', { headers: { 'Accept': 'application/json' } });
        if (!response.ok) throw new Error('failed to load media library');
        state = await response.json();
        render();
      } catch (err) {
        const message = err && err.message ? err.message : 'Failed to load media library.';
        app.innerHTML = '<div class="empty">' + escapeHtml(message) + '</div>';
      }
    }

    search.addEventListener('input', render);
    document.addEventListener('contextmenu', function(event) {
      if (event.target.closest('.thumb img')) {
        event.preventDefault();
      }
    });
    loadLibrary();
  </script>
</body>
</html>`

const watchHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>__TITLE__</title>
  <style>
    body {
      margin: 0;
      background: #000;
      color: #fff;
      font-family: Arial, Helvetica, sans-serif;
    }
    .topbar {
      display: flex;
      align-items: center;
      gap: 16px;
      padding: 14px 18px;
      background: linear-gradient(180deg, rgba(0,0,0,0.85), rgba(0,0,0,0.42));
      position: sticky;
      top: 0;
      z-index: 10;
    }
    .back {
      color: #fff;
      text-decoration: none;
      font-weight: 700;
      border: 1px solid rgba(255,255,255,0.2);
      border-radius: 999px;
      padding: 9px 14px;
    }
    .title {
      font-size: 20px;
      font-weight: 700;
    }
    .wrap {
      display: grid;
      place-items: center;
      min-height: calc(100vh - 68px);
      padding: 12px;
    }
    video {
      width: min(1400px, 100%);
      max-height: 84vh;
      background: #000;
      border-radius: 10px;
      outline: none;
      box-shadow: 0 20px 50px rgba(0,0,0,0.5);
    }
    .msg {
      color: #ddd;
      padding: 16px;
      max-width: 900px;
    }
  </style>
</head>
<body>
  <div class="topbar">
    <a class="back" href="__HOME_URL__">← Back</a>
    <div class="title">__TITLE__</div>
  </div>
  <div class="wrap">
    <video id="player" controls controlsList="nodownload noplaybackrate" disablePictureInPicture autoplay preload="metadata"></video>
    <div id="msg" class="msg" hidden></div>
  </div>
  <script src="https://cdn.jsdelivr.net/npm/hls.js@latest"></script>
  <script>
    (function() {
      const video = document.getElementById('player');
      const msg = document.getElementById('msg');
      const hlsEnabled = __HLS_ENABLED__;
      const hlsURL = '__HLS_URL__';
      const fallbackURL = '__FALLBACK_URL__';

      function showMessage(text) {
        msg.hidden = false;
        msg.textContent = text;
      }

      function useFallback() {
        video.src = fallbackURL;
      }

      if (!hlsEnabled) {
        useFallback();
        showMessage('HLS processing is disabled for this server run. Direct playback is being used.');
      } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
        video.src = hlsURL;
        video.addEventListener('error', function() {
          useFallback();
          showMessage('HLS playback failed, so direct playback is being used instead.');
        }, { once: true });
      } else if (window.Hls && window.Hls.isSupported()) {
        const hls = new Hls();
        hls.loadSource(hlsURL);
        hls.attachMedia(video);
        hls.on(Hls.Events.ERROR, function(_, data) {
          if (data && data.fatal) {
            useFallback();
            showMessage('HLS playback failed, so direct playback is being used instead.');
          }
        });
      } else {
        useFallback();
        showMessage('This browser does not support HLS.js, so direct playback is being used.');
      }

      document.addEventListener('contextmenu', function(event) {
        if (event.target.closest('video')) {
          event.preventDefault();
        }
      });
    })();
  </script>
</body>
</html>`

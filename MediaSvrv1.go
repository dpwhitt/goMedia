package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var supportedMedia = map[string]bool{
	".mp4":  true,
	".m4v":  true,
	".webm": true,
	".mov":  true,
	".mkv":  true,
	".mp3":  true,
	".m4a":  true,
	".wav":  true,
	".ogg":  true,
}

type Config struct {
	RootDir         string
	Addr            string
	Title           string
	ScanOnStart     bool
	ThumbWidth      int
	ThumbHeight     int
	ThumbDir        string
	ThumbOffset     string
	AllowAudio      bool
	LogRequests     bool
	RefreshInterval time.Duration
}

type MediaItem struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Path         string    `json:"-"`
	RelPath      string    `json:"relPath"`
	Dir          string    `json:"dir"`
	Ext          string    `json:"ext"`
	Size         int64     `json:"size"`
	Modified     time.Time `json:"modified"`
	DurationSec  float64   `json:"durationSec"`
	DurationText string    `json:"durationText"`
	ThumbURL     string    `json:"thumbUrl"`
	StreamURL    string    `json:"streamUrl"`
	Type         string    `json:"type"`
}

type Library struct {
	mu        sync.RWMutex
	items     []MediaItem
	byID      map[string]MediaItem
	lastScan  time.Time
	scanning  bool
	scanError string
}

type Server struct {
	cfg      Config
	library  *Library
	tmpl     *template.Template
	thumbMu  sync.Mutex
	thumbGen map[string]*sync.Once
}

type PageData struct {
	Title string
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.RootDir, "root", ".", "media library root directory")
	flag.StringVar(&cfg.Addr, "addr", ":8080", "listen address")
	flag.StringVar(&cfg.Title, "title", "My Media", "UI title")
	flag.BoolVar(&cfg.ScanOnStart, "scan-on-start", true, "scan library at startup")
	flag.IntVar(&cfg.ThumbWidth, "thumb-width", 480, "thumbnail width")
	flag.IntVar(&cfg.ThumbHeight, "thumb-height", 270, "thumbnail height")
	flag.StringVar(&cfg.ThumbDir, "thumb-dir", ".thumbs", "thumbnail cache folder under root")
	flag.StringVar(&cfg.ThumbOffset, "thumb-offset", "00:00:10", "ffmpeg capture offset")
	flag.BoolVar(&cfg.AllowAudio, "allow-audio", true, "include audio files")
	flag.BoolVar(&cfg.LogRequests, "log-requests", true, "enable request logging")
	flag.DurationVar(&cfg.RefreshInterval, "refresh", 0, "periodic rescan interval, e.g. 5m")
	flag.Parse()

	rootAbs, err := filepath.Abs(cfg.RootDir)
	must(err)
	cfg.RootDir = rootAbs
	must(os.MkdirAll(filepath.Join(cfg.RootDir, cfg.ThumbDir), 0o755))

	srv := &Server{
		cfg: cfg,
		library: &Library{
			byID: make(map[string]MediaItem),
		},
		tmpl:     template.Must(template.New("index").Parse(indexHTML)),
		thumbGen: make(map[string]*sync.Once),
	}

	if cfg.ScanOnStart {
		if err := srv.scanLibrary(context.Background()); err != nil {
			log.Printf("initial scan warning: %v", err)
		}
	}

	if cfg.RefreshInterval > 0 {
		go srv.periodicRefresh(cfg.RefreshInterval)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/library", srv.handleLibrary)
	mux.HandleFunc("/api/rescan", srv.handleRescan)
	mux.HandleFunc("/stream/", srv.handleStream)
	mux.HandleFunc("/thumb/", srv.handleThumb)

	var handler http.Handler = mux
	if cfg.LogRequests {
		handler = requestLogger(handler)
	}

	log.Printf("Media root: %s", cfg.RootDir)
	log.Printf("Listening on http://localhost%s", cfg.Addr)
	if runtime.GOOS == "windows" {
		log.Printf("Press Ctrl+C to stop")
	}
	must(http.ListenAndServe(cfg.Addr, handler))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) periodicRefresh(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := s.scanLibrary(ctx); err != nil {
			log.Printf("periodic rescan error: %v", err)
		}
		cancel()
	}
}

func (s *Server) scanLibrary(ctx context.Context) error {
	s.library.mu.Lock()
	if s.library.scanning {
		s.library.mu.Unlock()
		return nil
	}
	s.library.scanning = true
	s.library.scanError = ""
	s.library.mu.Unlock()

	defer func() {
		s.library.mu.Lock()
		s.library.scanning = false
		s.library.lastScan = time.Now()
		s.library.mu.Unlock()
	}()

	items := make([]MediaItem, 0, 256)
	byID := make(map[string]MediaItem)

	err := filepath.WalkDir(s.cfg.RootDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		name := d.Name()
		if d.IsDir() {
			if path != s.cfg.RootDir {
				if name == s.cfg.ThumbDir || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		if !supportedMedia[ext] {
			return nil
		}
		if !s.cfg.AllowAudio && isAudioExt(ext) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(s.cfg.RootDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		itemID := safeID(relSlash)
		durSec, _ := probeDuration(path)
		itemType := "video"
		if isAudioExt(ext) {
			itemType = "audio"
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}

		item := MediaItem{
			ID:           itemID,
			Name:         strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
			Path:         path,
			RelPath:      relSlash,
			Dir:          dir,
			Ext:          ext,
			Size:         info.Size(),
			Modified:     info.ModTime(),
			DurationSec:  durSec,
			DurationText: formatDuration(durSec),
			ThumbURL:     "/thumb/" + itemID,
			StreamURL:    "/stream/" + itemID,
			Type:         itemType,
		}
		items = append(items, item)
		byID[itemID] = item
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.library.mu.Lock()
		s.library.scanError = err.Error()
		s.library.mu.Unlock()
		return err
	}

	sort.Slice(items, func(i, j int) bool {
		if strings.EqualFold(items[i].Dir, items[j].Dir) {
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		}
		return strings.ToLower(items[i].Dir) < strings.ToLower(items[j].Dir)
	})

	s.library.mu.Lock()
	s.library.items = items
	s.library.byID = byID
	s.library.mu.Unlock()
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, PageData{Title: s.cfg.Title}); err != nil {
		http.Error(w, "template render failed", http.StatusInternalServerError)
	}
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	s.library.mu.RLock()
	items := append([]MediaItem(nil), s.library.items...)
	scanning := s.library.scanning
	lastScan := s.library.lastScan
	scanError := s.library.scanError
	s.library.mu.RUnlock()

	resp := map[string]interface{}{
		"title":     s.cfg.Title,
		"count":     len(items),
		"items":     items,
		"scanning":  scanning,
		"lastScan":  lastScan,
		"scanError": scanError,
		"mediaRoot": s.cfg.RootDir,
	}
	writeJSON(w, resp)
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.scanLibrary(ctx); err != nil {
			log.Printf("rescan error: %v", err)
		}
	}()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/stream/")
	item, ok := s.getItem(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path, err := s.resolveMediaPath(item.RelPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat file", http.StatusInternalServerError)
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ctype == "" {
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		ctype = http.DetectContentType(buf[:n])
		_, _ = f.Seek(0, io.SeekStart)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=60")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/thumb/")
	item, ok := s.getItem(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if item.Type == "audio" {
		serveSVGPlaceholder(w, item.Name, true)
		return
	}

	thumbPath := filepath.Join(s.cfg.RootDir, s.cfg.ThumbDir, id+".jpg")
	if _, err := os.Stat(thumbPath); err == nil {
		serveFile(w, r, thumbPath, "image/jpeg")
		return
	}

	s.thumbMu.Lock()
	once := s.thumbGen[id]
	if once == nil {
		once = &sync.Once{}
		s.thumbGen[id] = once
	}
	s.thumbMu.Unlock()

	var genErr error
	once.Do(func() {
		mediaPath, err := s.resolveMediaPath(item.RelPath)
		if err != nil {
			genErr = err
			return
		}
		genErr = generateThumbnail(mediaPath, thumbPath, s.cfg.ThumbOffset, s.cfg.ThumbWidth, s.cfg.ThumbHeight)
	})

	if genErr == nil {
		if _, err := os.Stat(thumbPath); err == nil {
			serveFile(w, r, thumbPath, "image/jpeg")
			return
		}
	}
	serveSVGPlaceholder(w, item.Name, false)
}

func (s *Server) getItem(id string) (MediaItem, bool) {
	s.library.mu.RLock()
	defer s.library.mu.RUnlock()
	item, ok := s.library.byID[id]
	return item, ok
}

func (s *Server) resolveMediaPath(rel string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(rel))
	full := filepath.Join(s.cfg.RootDir, cleanRel)
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(s.cfg.RootDir)
	if err != nil {
		return "", err
	}
	relToRoot, err := filepath.Rel(rootAbs, fullAbs)
	if err != nil {
		return "", err
	}
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes root")
	}
	return fullAbs, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func safeID(s string) string {
	h := sha1.Sum([]byte(strings.ToLower(strings.ReplaceAll(s, "\\", "/"))))
	return hex.EncodeToString(h[:])
}

func isAudioExt(ext string) bool {
	switch ext {
	case ".mp3", ".m4a", ".wav", ".ogg":
		return true
	default:
		return false
	}
}

func formatDuration(sec float64) string {
	if sec <= 0 {
		return ""
	}
	total := int(sec + 0.5)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func probeDuration(path string) (float64, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func generateThumbnail(mediaPath, thumbPath, offset string, width, height int) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(thumbPath), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	vf := fmt.Sprintf("thumbnail,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", width, height, width, height)
	cmd := exec.CommandContext(ctx,
		"ffmpeg",
		"-y",
		"-ss", offset,
		"-i", mediaPath,
		"-frames:v", "1",
		"-vf", vf,
		thumbPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg thumbnail failed: %w: %s", err, stderr.String())
	}
	return nil
}

func serveFile(w http.ResponseWriter, r *http.Request, path, contentType string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func serveSVGPlaceholder(w http.ResponseWriter, title string, audio bool) {
	label := "VIDEO"
	if audio {
		label = "AUDIO"
	}
	safeTitle := template.HTMLEscapeString(title)
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 480 270">
<defs>
  <linearGradient id="g" x1="0" x2="1" y1="0" y2="1">
    <stop offset="0%%" stop-color="#141414"/>
    <stop offset="100%%" stop-color="#2a2a2a"/>
  </linearGradient>
</defs>
<rect width="480" height="270" fill="url(#g)"/>
<rect x="18" y="18" rx="10" ry="10" width="88" height="32" fill="#e50914"/>
<text x="62" y="39" font-family="Arial, sans-serif" font-size="14" fill="#fff" text-anchor="middle">%s</text>
<text x="240" y="140" font-family="Arial, sans-serif" font-size="28" fill="#ffffff" text-anchor="middle">%s</text>
<text x="240" y="172" font-family="Arial, sans-serif" font-size="16" fill="#b3b3b3" text-anchor="middle">Thumbnail unavailable</text>
</svg>`, label, safeTitle)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(svg))
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <style>
    :root {
      --bg: #141414;
      --bg2: #181818;
      --card: #222;
      --fg: #fff;
      --muted: #b3b3b3;
      --red: #e50914;
      --red2: #f6121d;
      --line: rgba(255,255,255,0.08);
      --shadow: 0 20px 40px rgba(0,0,0,0.35);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: linear-gradient(to bottom, rgba(0,0,0,.45), rgba(0,0,0,.85)), var(--bg);
      color: var(--fg);
      font: 14px/1.4 Inter, Segoe UI, Arial, sans-serif;
    }
    .topbar {
      position: sticky;
      top: 0;
      z-index: 20;
      display: flex;
      gap: 16px;
      align-items: center;
      padding: 16px 24px;
      background: linear-gradient(to bottom, rgba(0,0,0,.8), rgba(0,0,0,.25));
      backdrop-filter: blur(8px);
      border-bottom: 1px solid var(--line);
    }
    .brand {
      font-size: 28px;
      font-weight: 800;
      color: var(--red);
      letter-spacing: .04em;
      white-space: nowrap;
    }
    .title {
      font-size: 16px;
      color: #ddd;
      white-space: nowrap;
    }
    .search {
      flex: 1;
      display: flex;
      justify-content: flex-end;
      gap: 10px;
      flex-wrap: wrap;
    }
    input, button, select {
      border: 1px solid var(--line);
      background: rgba(255,255,255,.06);
      color: var(--fg);
      border-radius: 8px;
      padding: 10px 12px;
      font: inherit;
    }
    button {
      cursor: pointer;
      background: var(--red);
      border-color: transparent;
      font-weight: 700;
    }
    button:hover { background: var(--red2); }
    .hero {
      padding: 28px 24px 8px;
    }
    .hero h1 {
      margin: 0 0 8px;
      font-size: clamp(30px, 4vw, 52px);
      line-height: 1;
    }
    .hero p {
      margin: 0;
      color: var(--muted);
      max-width: 800px;
    }
    .status {
      padding: 8px 24px 0;
      color: var(--muted);
    }
    .rows { padding: 12px 0 32px; }
    .row { margin-top: 22px; }
    .row-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0 24px 10px;
    }
    .row-header h2 {
      margin: 0;
      font-size: 22px;
    }
    .count { color: var(--muted); }
    .rail {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
      gap: 18px;
      padding: 0 24px;
    }
    .card {
      position: relative;
      background: var(--card);
      border-radius: 14px;
      overflow: hidden;
      box-shadow: var(--shadow);
      transition: transform .18s ease, box-shadow .18s ease;
      border: 1px solid rgba(255,255,255,.04);
      cursor: pointer;
    }
    .card:hover {
      transform: translateY(-4px) scale(1.02);
      box-shadow: 0 24px 50px rgba(0,0,0,.45);
    }
    .thumb-wrap {
      position: relative;
      aspect-ratio: 16 / 9;
      background: #111;
    }
    .thumb {
      width: 100%;
      height: 100%;
      object-fit: cover;
      display: block;
      background: #111;
    }
    .play {
      position: absolute;
      inset: auto auto 12px 12px;
      width: 46px;
      height: 46px;
      border-radius: 50%;
      background: rgba(0,0,0,.75);
      display: grid;
      place-items: center;
      border: 1px solid rgba(255,255,255,.18);
      font-size: 18px;
    }
    .duration {
      position: absolute;
      right: 10px;
      bottom: 10px;
      background: rgba(0,0,0,.8);
      color: white;
      font-size: 12px;
      padding: 4px 8px;
      border-radius: 999px;
    }
    .meta {
      padding: 12px;
    }
    .name {
      font-weight: 700;
      font-size: 15px;
      margin: 0 0 4px;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      min-height: 42px;
    }
    .sub {
      color: var(--muted);
      font-size: 12px;
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
    }
    .badge {
      display: inline-flex;
      align-items: center;
      padding: 2px 8px;
      border-radius: 999px;
      background: rgba(255,255,255,.08);
      color: #ddd;
    }
    .modal {
      position: fixed;
      inset: 0;
      display: none;
      place-items: center;
      background: rgba(0,0,0,.82);
      z-index: 50;
      padding: 20px;
    }
    .modal.open { display: grid; }
    .player-box {
      width: min(1200px, 96vw);
      background: #0d0d0d;
      border-radius: 18px;
      overflow: hidden;
      box-shadow: 0 30px 80px rgba(0,0,0,.6);
      border: 1px solid rgba(255,255,255,.08);
    }
    .player-head {
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 16px;
      padding: 14px 18px;
      border-bottom: 1px solid var(--line);
      background: #161616;
    }
    .player-title { font-size: 18px; font-weight: 700; }
    .player-sub { color: var(--muted); font-size: 12px; margin-top: 2px; }
    .close {
      background: transparent;
      border: 1px solid var(--line);
      color: var(--fg);
      width: 42px;
      height: 42px;
      border-radius: 999px;
      font-size: 20px;
      line-height: 1;
    }
    video, audio {
      width: 100%;
      display: block;
      background: black;
    }
    .empty {
      padding: 24px;
      color: var(--muted);
    }
    @media (max-width: 700px) {
      .topbar { padding: 14px; }
      .hero { padding: 18px 14px 8px; }
      .status, .row-header { padding-left: 14px; padding-right: 14px; }
      .rail { padding: 0 14px; grid-template-columns: repeat(auto-fill, minmax(170px, 1fr)); gap: 12px; }
      .brand { font-size: 24px; }
      .title { display: none; }
    }
  </style>
</head>
<body>
  <div class="topbar">
    <div class="brand">NETFLIX</div>
    <div class="title" id="appTitle">Loading...</div>
    <div class="search">
      <input id="search" placeholder="Search titles">
      <select id="sort">
        <option value="name">Sort: Name</option>
        <option value="newest">Sort: Newest</option>
        <option value="largest">Sort: Largest</option>
        <option value="duration">Sort: Duration</option>
      </select>
      <button id="rescanBtn" type="button">Rescan</button>
    </div>
  </div>

  <section class="hero">
    <h1 id="heroTitle">Your media library</h1>
    <p>Browse and stream your video and audio files directly in the browser. For strongest playback support, use MP4 with H.264/AAC.</p>
  </section>

  <div class="status" id="status">Loading library...</div>
  <div class="rows" id="rows"></div>

  <div class="modal" id="modal">
    <div class="player-box">
      <div class="player-head">
        <div>
          <div class="player-title" id="playerTitle"></div>
          <div class="player-sub" id="playerSub"></div>
        </div>
        <button class="close" id="closeBtn" type="button">×</button>
      </div>
      <div id="playerHost"></div>
    </div>
  </div>

  <script>
    let library = [];

    const rowsEl = document.getElementById('rows');
    const statusEl = document.getElementById('status');
    const searchEl = document.getElementById('search');
    const sortEl = document.getElementById('sort');
    const modalEl = document.getElementById('modal');
    const playerHostEl = document.getElementById('playerHost');
    const playerTitleEl = document.getElementById('playerTitle');
    const playerSubEl = document.getElementById('playerSub');
    const appTitleEl = document.getElementById('appTitle');
    const heroTitleEl = document.getElementById('heroTitle');

    document.getElementById('closeBtn').addEventListener('click', closePlayer);
    document.getElementById('rescanBtn').addEventListener('click', async () => {
      try {
        statusEl.textContent = 'Rescanning...';
        await fetch('/api/rescan', { method: 'POST' });
        setTimeout(loadLibrary, 800);
      } catch (err) {
        console.error(err);
        statusEl.textContent = 'Rescan failed.';
      }
    });
    searchEl.addEventListener('input', render);
    sortEl.addEventListener('change', render);
    modalEl.addEventListener('click', (e) => {
      if (e.target === modalEl) closePlayer();
    });
    window.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') closePlayer();
    });

    function fmtBytes(bytes) {
      if (bytes === null || bytes === undefined) return '';
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let i = 0;
      let v = bytes;
      while (v >= 1024 && i < units.length - 1) {
        v /= 1024;
        i++;
      }
      const dp = v >= 100 || i === 0 ? 0 : 1;
      return v.toFixed(dp) + ' ' + units[i];
    }

    function escapeHtml(value) {
      return String(value ?? '').replace(/[&<>\"]/g, (ch) => ({
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;'
      }[ch]));
    }

    function groupItems(items) {
      const groups = new Map();
      for (const item of items) {
        const key = item.dir && item.dir !== '.' ? item.dir : 'All Media';
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(item);
      }
      return Array.from(groups.entries());
    }

    function applySort(items) {
      const mode = sortEl.value;
      items.sort((a, b) => {
        if (mode === 'newest') return new Date(b.modified) - new Date(a.modified);
        if (mode === 'largest') return (b.size || 0) - (a.size || 0);
        if (mode === 'duration') return (b.durationSec || 0) - (a.durationSec || 0);
        return String(a.name || '').localeCompare(String(b.name || ''));
      });
      return items;
    }

    function filterItems() {
      const q = (searchEl.value || '').trim().toLowerCase();
      let items = library.slice();
      if (q) {
        items = items.filter((i) => {
          return String(i.name || '').toLowerCase().includes(q) ||
            String(i.relPath || '').toLowerCase().includes(q) ||
            String(i.dir || '').toLowerCase().includes(q);
        });
      }
      return applySort(items);
    }

    function render() {
      const items = filterItems();
      if (!items.length) {
        rowsEl.innerHTML = '<div class="empty">No media matched the current filter.</div>';
        return;
      }

      const groups = groupItems(items);
      rowsEl.innerHTML = groups.map(([name, arr]) => {
        return '\n<section class="row">\n' +
          '  <div class="row-header">\n' +
          '    <h2>' + escapeHtml(name) + '</h2>\n' +
          '    <div class="count">' + arr.length + ' item' + (arr.length === 1 ? '' : 's') + '</div>\n' +
          '  </div>\n' +
          '  <div class="rail">\n' +
          arr.map((item) => {
            return '    <article class="card" data-id="' + escapeHtml(item.id) + '">\n' +
              '      <div class="thumb-wrap">\n' +
              '        <img class="thumb" src="' + escapeHtml(item.thumbUrl) + '" alt="' + escapeHtml(item.name) + '" loading="lazy">\n' +
              '        <div class="play">▶</div>\n' +
              (item.durationText ? '        <div class="duration">' + escapeHtml(item.durationText) + '</div>\n' : '') +
              '      </div>\n' +
              '      <div class="meta">\n' +
              '        <div class="name">' + escapeHtml(item.name) + '</div>\n' +
              '        <div class="sub">\n' +
              '          <span class="badge">' + escapeHtml(String(item.ext || '').replace('.', '').toUpperCase()) + '</span>\n' +
              '          <span>' + escapeHtml(fmtBytes(item.size)) + '</span>\n' +
              '          <span>' + escapeHtml(item.type) + '</span>\n' +
              '        </div>\n' +
              '      </div>\n' +
              '    </article>\n';
          }).join('') +
          '  </div>\n' +
          '</section>\n';
      }).join('');

      document.querySelectorAll('.card').forEach((el) => {
        el.addEventListener('click', () => {
          const item = library.find((x) => x.id === el.dataset.id);
          if (item) openPlayer(item);
        });
      });
    }

    function openPlayer(item) {
      playerTitleEl.textContent = item.name || '';
      playerSubEl.textContent = (item.relPath || '') + (item.durationText ? ' • ' + item.durationText : '');

      if (item.type === 'audio') {
        playerHostEl.innerHTML =
          '<div style="padding:20px">' +
          '  <img src="' + escapeHtml(item.thumbUrl) + '" alt="" style="width:100%;max-height:420px;object-fit:cover;border-radius:14px;margin-bottom:18px;background:#111">' +
          '  <audio controls autoplay preload="metadata">' +
          '    <source src="' + escapeHtml(item.streamUrl) + '">' +
          '    Your browser could not play this audio file.' +
          '  </audio>' +
          '</div>';
      } else {
        playerHostEl.innerHTML =
          '<video controls autoplay preload="metadata" playsinline>' +
          '  <source src="' + escapeHtml(item.streamUrl) + '">' +
          '  Your browser could not play this video file. Try MP4/H.264/AAC for strongest compatibility.' +
          '</video>';
      }
      modalEl.classList.add('open');
    }

    function closePlayer() {
      const media = playerHostEl.querySelector('video, audio');
      if (media) {
        try { media.pause(); } catch (_) {}
        media.removeAttribute('src');
        if (typeof media.load === 'function') media.load();
      }
      playerHostEl.innerHTML = '';
      modalEl.classList.remove('open');
    }

    async function loadLibrary() {
      try {
        const res = await fetch('/api/library');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        library = Array.isArray(data.items) ? data.items : [];
        appTitleEl.textContent = data.title || 'My Media';
        heroTitleEl.textContent = data.title || 'Your media library';

        const parts = [];
        parts.push(library.length + ' item' + (library.length === 1 ? '' : 's'));
        if (data.scanning) parts.push('scan in progress');
        if (data.lastScan) parts.push('last scan ' + new Date(data.lastScan).toLocaleString());
        if (data.scanError) parts.push('scan warning: ' + data.scanError);
        statusEl.textContent = parts.join(' • ');
        render();
      } catch (err) {
        console.error(err);
        statusEl.textContent = 'Failed to load library.';
        rowsEl.innerHTML = '<div class="empty">Could not load the library API.</div>';
      }
    }

    loadLibrary();
  </script>
</body>
</html>`

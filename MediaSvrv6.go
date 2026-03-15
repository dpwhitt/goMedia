package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net"
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
	LogFile        string
	Password        string
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
	Resolution   string    `json:"resolution"`
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
	loginT   *template.Template
	thumbMu  sync.Mutex
	thumbGen map[string]*sync.Once
}

type PageData struct {
	Title string
}

type LoginPageData struct {
	Title string
	Error string
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.RootDir, "root", ".", "media library root directory")
	flag.StringVar(&cfg.Addr, "addr", ":8080", "listen address")
	flag.StringVar(&cfg.Title, "title", "My Media", "UI title")
	flag.StringVar(&cfg.Password, "password", "", "optional password to access the site")
	flag.BoolVar(&cfg.ScanOnStart, "scan-on-start", true, "scan library at startup")
	flag.IntVar(&cfg.ThumbWidth, "thumb-width", 480, "thumbnail width")
	flag.IntVar(&cfg.ThumbHeight, "thumb-height", 270, "thumbnail height")
	flag.StringVar(&cfg.ThumbDir, "thumb-dir", ".thumbs", "thumbnail cache folder under root")
	flag.StringVar(&cfg.ThumbOffset, "thumb-offset", "00:00:10", "ffmpeg capture offset")
	flag.BoolVar(&cfg.AllowAudio, "allow-audio", true, "include audio files")
	flag.BoolVar(&cfg.LogRequests, "log-requests", true, "enable request logging")
	flag.StringVar(&cfg.LogFile, "log-file", "", "optional log file path for events")
	flag.DurationVar(&cfg.RefreshInterval, "refresh", 0, "periodic rescan interval, e.g. 5m")
	flag.Parse()

	// configure logging output
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("cannot open log file: %v", err)
		}
		log.SetOutput(f)
	}

	rootAbs, err := filepath.Abs(cfg.RootDir)
	must(err)
	cfg.RootDir = rootAbs

	thumbCacheDir := filepath.Join(cfg.RootDir, cfg.ThumbDir)
	must(os.MkdirAll(thumbCacheDir, 0o755))

	srv := &Server{
		cfg: cfg,
		library: &Library{
			byID: make(map[string]MediaItem),
		},
		tmpl:     template.Must(template.New("index").Parse(indexHTML)),
		loginT:   template.Must(template.New("login").Parse(loginHTML)),
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
	mux.HandleFunc("/login", srv.handleLogin)
	mux.HandleFunc("/logout", srv.handleLogout)
	mux.HandleFunc("/api/library", srv.handleLibrary)
	mux.HandleFunc("/api/rescan", srv.handleRescan)
	mux.HandleFunc("/stream/", srv.handleStream)
	mux.HandleFunc("/thumb/", srv.handleThumb)

	var handler http.Handler = mux
	if cfg.Password != "" {
		handler = sessionAuthMiddleware(cfg.Password, handler)
	}
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

func sessionCookieName() string {
	return "media_session"
}

func sessionCookieValue(password string) string {
	sum := sha1.Sum([]byte("media-session|" + password))
	return hex.EncodeToString(sum[:])
}

func isAuthenticated(r *http.Request, password string) bool {
	if password == "" {
		return true
	}
	cookie, err := r.Cookie(sessionCookieName())
	if err != nil {
		return false
	}
	expected := sessionCookieValue(password)
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(expected)) == 1
}

func setSessionCookie(w http.ResponseWriter, password string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(),
		Value:    sessionCookieValue(password),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func sessionAuthMiddleware(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		if isAuthenticated(r, password) {
			next.ServeHTTP(w, r)
			return
		}
		clearSessionCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ip := clientIP(r)
		hostname := lookupHostname(ip)

		next.ServeHTTP(w, r)

		log.Printf(
			"client_ip=%s hostname=%s method=%s path=%s query=%q ua=%q referer=%q duration=%s",
			ip,
			hostname,
			r.Method,
			r.URL.Path,
			r.URL.RawQuery,
			r.UserAgent(),
			r.Referer(),
			time.Since(start).Round(time.Millisecond),
		)
	})
}

func clientIP(r *http.Request) string {
	forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	realIP := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if realIP != "" {
		return realIP
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}

	return r.RemoteAddr
}

func lookupHostname(ip string) string {
	if ip == "" {
		return "unknown"
	}
	if net.ParseIP(ip) == nil {
		return "unknown"
	}
	names, err := net.LookupAddr(ip)
	if err != nil || len(names) == 0 {
		return "unknown"
	}
	return strings.TrimSuffix(names[0], ".")
}

func (s *Server) periodicRefresh(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
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
			if path != s.cfg.RootDir && (name == s.cfg.ThumbDir || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
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
		resolution := probeResolution(path)
		itemType := "video"
		if isAudioExt(ext) {
			itemType = "audio"
		}

		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}

		item := MediaItem{
			Resolution:   resolution,
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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Password == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		if isAuthenticated(r, s.cfg.Password) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.loginT.Execute(w, LoginPageData{Title: s.cfg.Title})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	password := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.Password)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.loginT.Execute(w, LoginPageData{Title: s.cfg.Title, Error: "Wrong password"})
		return
	}
	setSessionCookie(w, s.cfg.Password)
		http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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

	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "cannot stat file", http.StatusInternalServerError)
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ctype == "" {
		buf := make([]byte, 512)
		n, _ := file.Read(buf)
		ctype = http.DetectContentType(buf[:n])
		_, _ = file.Seek(0, io.SeekStart)
	}

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
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
	// Use "\\" to properly represent a single backslash in the string
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

	cmd := exec.CommandContext(
		ctx,
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

func probeResolution(path string) string {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		"ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	res := strings.TrimSpace(string(out))
	if res == "" || res == "x" || res == "0x0" {
		return ""
	}
	return res
}

func generateThumbnail(mediaPath, thumbPath, offset string, width, height int) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return err
	}

	_ = os.MkdirAll(filepath.Dir(thumbPath), 0o755)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	vf := fmt.Sprintf("thumbnail,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", width, height, width, height)
	cmd := exec.CommandContext(
		ctx,
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
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "private, no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
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
	w.Header().Set("Cache-Control", "private, no-store, no-cache, must-revalidate")
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
      margin: 0 auto;
      max-width: 1400px;
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
    .spacer { flex: 1; }
    .logout {
      color: #fff;
      text-decoration: none;
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 10px 12px;
      background: rgba(255,255,255,.06);
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
      user-select: none;
      -webkit-user-select: none;
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
      pointer-events: none;
    }
    .play {
      position: absolute;
      left: 12px;
      bottom: 12px;
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
    .resolution {
      position: absolute;
      right: 10px;
      top: 10px;
      background: rgba(0,0,0,.8);
      color: white;
      font-size: 12px;
      padding: 4px 8px;
      border-radius: 999px;
    }
    .meta { padding: 12px; }
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
      controlslist: nodownload noplaybackrate;
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
    <div class="spacer"></div>
    <a class="logout" href="/logout">Logout</a>
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
    <p>Browse and stream your video and audio files directly in the browser. Download options are hidden, caching is disabled, and direct file links are not exposed in the UI.</p>
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

    document.addEventListener('contextmenu', function (e) {
      const media = e.target.closest('video, audio, img');
      if (media) e.preventDefault();
    });

    document.getElementById('closeBtn').addEventListener('click', closePlayer);
    document.getElementById('rescanBtn').addEventListener('click', async function () {
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
    modalEl.addEventListener('click', function (e) {
      if (e.target === modalEl) {
        closePlayer();
      }
    });
    window.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') {
        closePlayer();
      }
    });

    function fmtBytes(bytes) {
      if (bytes === null || bytes === undefined) return '';
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let i = 0;
      let v = bytes;
      while (v >= 1024 && i < units.length - 1) {
        v /= 1024;
        i += 1;
      }
      const dp = v >= 100 || i === 0 ? 0 : 1;
      return v.toFixed(dp) + ' ' + units[i];
    }

    function escapeHtml(value) {
      return String(value == null ? '' : value)
        .split('&').join('&amp;')
        .split('<').join('&lt;')
        .split('>').join('&gt;')
        .split('"').join('&quot;');
    }

    function groupItems(items) {
      const groups = new Map();
      for (const item of items) {
        const key = item.dir && item.dir !== '.' ? item.dir : 'All Media';
        if (!groups.has(key)) {
          groups.set(key, []);
        }
        groups.get(key).push(item);
      }
      return Array.from(groups.entries());
    }

    function applySort(items) {
      const mode = sortEl.value;
      items.sort(function (a, b) {
        if (mode === 'newest') return new Date(b.modified) - new Date(a.modified);
        if (mode === 'largest') return (b.size || 0) - (a.size || 0);
        if (mode === 'duration') return (b.durationSec || 0) - (a.durationSec || 0);
        return String(a.name || '').localeCompare(String(b.name || ''));
      });
      return items;
    }

    function filterItems() {
      const q = String(searchEl.value || '').trim().toLowerCase();
      let items = library.slice();
      if (q) {
        items = items.filter(function (i) {
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
      let html = '';
      groups.forEach(function (entry) {
        const name = entry[0];
        const arr = entry[1];
        html += '<section class="row">';
        html += '<div class="row-header">';
        html += '<h2>' + escapeHtml(name) + '</h2>';
        html += '<div class="count">' + arr.length + ' item' + (arr.length === 1 ? '' : 's') + '</div>';
        html += '</div>';
        html += '<div class="rail">';

        arr.forEach(function (item) {
          html += '<article class="card" data-id="' + escapeHtml(item.id) + '">';
          html += '<div class="thumb-wrap">';
          html += '<img class="thumb" src="' + escapeHtml(item.thumbUrl) + '" alt="' + escapeHtml(item.name) + '" loading="lazy">';
          html += '<div class="play">▶</div>';
          if (item.durationText) {
            html += '<div class="duration">' + escapeHtml(item.durationText) + '</div>';
          }
          if (item.resolution) {
            html += '<div class="resolution">' + escapeHtml(item.resolution) + '</div>';
          }
          html += '</div>';
          html += '<div class="meta">';
          html += '<div class="name">' + escapeHtml(item.name) + '</div>';
          html += '<div class="sub">';
          html += '<span class="badge">' + escapeHtml(String(item.ext || '').replace('.', '').toUpperCase()) + '</span>';
          html += '<span>' + escapeHtml(fmtBytes(item.size)) + '</span>';
          html += '<span>' + escapeHtml(item.type) + '</span>';
          html += '</div>';
          html += '</div>';
          html += '</article>';
        });

        html += '</div>';
        html += '</section>';
      });

      rowsEl.innerHTML = html;

      document.querySelectorAll('.card').forEach(function (el) {
        el.addEventListener('click', function () {
          const item = library.find(function (x) { return x.id === el.dataset.id; });
          if (item) {
            openPlayer(item);
          }
        });
      });
    }

    function openPlayer(item) {
      playerTitleEl.textContent = item.name || '';
      playerSubEl.textContent = (item.relPath || '') + (item.durationText ? ' • ' + item.durationText : '');

      if (item.type === 'audio') {
        playerHostEl.innerHTML =
          '<div style="padding:20px">' +
          '<img src="' + escapeHtml(item.thumbUrl) + '" alt="" style="width:100%;max-height:420px;object-fit:cover;border-radius:14px;margin-bottom:18px;background:#111">' +
          '<audio controls autoplay preload="metadata" controlsList="nodownload noplaybackrate">' +
          '<source src="' + escapeHtml(item.streamUrl) + '">' +
          'Your browser could not play this audio file.' +
          '</audio>' +
          '</div>';
      } else {
        playerHostEl.innerHTML =
          '<video controls autoplay preload="metadata" playsinline controlsList="nodownload noplaybackrate" disablePictureInPicture>' +
          '<source src="' + escapeHtml(item.streamUrl) + '">' +
          'Your browser could not play this video file. Try MP4/H.264/AAC for strongest compatibility.' +
          '</video>';
      }
      modalEl.classList.add('open');
    }

    function closePlayer() {
      const media = playerHostEl.querySelector('video, audio');
      if (media) {
        try { media.pause(); } catch (e) {}
        media.removeAttribute('src');
        if (typeof media.load === 'function') {
          media.load();
        }
      }
      playerHostEl.innerHTML = '';
      modalEl.classList.remove('open');
    }

    async function loadLibrary() {
      try {
        const res = await fetch('/api/library');
        if (!res.ok) {
          throw new Error('HTTP ' + res.status);
        }
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

const loginHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} - Login</title>
  <style>
    :root {
      --bg: #141414;
      --panel: #1c1c1c;
      --fg: #fff;
      --muted: #b3b3b3;
      --red: #e50914;
      --red2: #f6121d;
      --line: rgba(255,255,255,0.08);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      display: grid;
      place-items: center;
      background: radial-gradient(circle at top, rgba(229,9,20,.18), transparent 30%), #141414;
      color: var(--fg);
      font: 16px/1.4 Inter, Segoe UI, Arial, sans-serif;
      padding: 24px;
    }
    .panel {
      width: min(420px, 100%);
      background: rgba(28,28,28,.96);
      border: 1px solid var(--line);
      border-radius: 18px;
      padding: 28px;
      box-shadow: 0 30px 80px rgba(0,0,0,.5);
    }
    .brand {
      color: var(--red);
      font-size: 34px;
      font-weight: 800;
      letter-spacing: .05em;
      margin-bottom: 12px;
    }
    h1 {
      margin: 0 0 8px;
      font-size: 28px;
    }
    p {
      margin: 0 0 18px;
      color: var(--muted);
    }
    .error {
      margin: 0 0 14px;
      color: #ff8f8f;
      background: rgba(229,9,20,.12);
      border: 1px solid rgba(229,9,20,.35);
      border-radius: 10px;
      padding: 10px 12px;
    }
    input {
      width: 100%;
      border: 1px solid var(--line);
      background: rgba(255,255,255,.06);
      color: var(--fg);
      border-radius: 10px;
      padding: 14px 16px;
      font: inherit;
      margin-bottom: 14px;
    }
    button {
      width: 100%;
      border: 0;
      border-radius: 10px;
      padding: 14px 16px;
      font: inherit;
      font-weight: 700;
      background: var(--red);
      color: #fff;
      cursor: pointer;
    }
    button:hover { background: var(--red2); }
  </style>
</head>
<body>
  <form class="panel" method="post" action="/login">
    <div class="brand">NETFLIX</div>
    <h1>Sign in</h1>
    <p>Enter the server password to continue.</p>
    {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
    <input type="password" name="password" placeholder="Password" autofocus required>
    <button type="submit">Enter</button>
  </form>
</body>
</html>`

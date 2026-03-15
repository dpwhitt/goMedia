# Go Media Server (Netflix-Style Browser UI)

A lightweight **Golang media server** that scans a folder of video/audio files and streams them to a web browser using a **Netflix-style interface**.

It runs as a single Go program and serves a responsive web UI that allows browsing, searching, and streaming media directly in the browser.

---

# Features

* Netflix-style card interface
* Browser streaming with HTTP range support
* Automatic library scanning
* Thumbnail generation (via ffmpeg)
* Duration extraction (via ffprobe)
* Folder grouping
* Search and sorting
* Video + audio support
* Modal player
* Responsive UI (desktop / tablet / mobile)
* Safe path handling (prevents directory traversal)
* Optional automatic rescanning

---

# Screenshot

The interface includes:

* Media rows grouped by folder
* Poster thumbnails
* Duration badges
* Click-to-play modal player
* Search and sorting tools

---

# Requirements

Minimum:

```
Go 1.20+
```

Optional but recommended:

```
ffmpeg
ffprobe
```

These enable:

* video duration detection
* automatic thumbnail generation

Install on most systems with:

Linux

```
sudo apt install ffmpeg
```

Mac

```
brew install ffmpeg
```

Windows

Download from

```
https://ffmpeg.org/download.html
```

and add to PATH.

---

# Installation

Clone or copy the file:

```
MediaSvr.go
```

Run:

```
go run MediaSvr.go -root "/path/to/media"
```

Example (Windows)

```
go run MediaSvr.go -root "D:\Movies"
```

Example (Linux)

```
go run MediaSvr.go -root "/mnt/media"
```

Then open in browser:

```
http://localhost:8080
```

---

# Command Line Options

| Flag             | Description                | Default    |
| ---------------- | -------------------------- | ---------- |
| `-root`          | Media library directory    | `.`        |
| `-addr`          | Server address             | `:8080`    |
| `-title`         | UI title                   | `My Media` |
| `-scan-on-start` | Scan library at startup    | `true`     |
| `-refresh`       | Auto rescan interval       | `0`        |
| `-allow-audio`   | Include audio files        | `true`     |
| `-thumb-width`   | Thumbnail width            | `480`      |
| `-thumb-height`  | Thumbnail height           | `270`      |
| `-thumb-offset`  | Thumbnail capture position | `00:00:10` |
| `-thumb-dir`     | Thumbnail cache folder     | `.thumbs`  |
| `-log-requests`  | Enable HTTP logging        | `true`     |

Example:

```
go run MediaSvr.go \
-root "/media/movies" \
-title "Home Cinema" \
-refresh 10m
```

---

# Supported Media Formats

Video

```
mp4
m4v
webm
mov
mkv
```

Audio

```
mp3
m4a
wav
ogg
```

**Best browser compatibility**

```
MP4 (H.264 video + AAC audio)
```

Some browsers cannot play MKV or certain codecs.

---

# Directory Layout Example

```
Movies/
  Action/
      John Wick.mp4
      Matrix.mp4

  SciFi/
      Interstellar.mp4
      Arrival.mp4

  Music/
      Track1.mp3
```

Each folder becomes a **row** in the UI.

---

# Thumbnails

If **ffmpeg is installed**, thumbnails are generated automatically and cached:

```
.thumb/
```

Example:

```
.thumb/6e91c7c9.jpg
```

If ffmpeg is not available a placeholder image is used.

---

# Streaming

The server streams media using:

```
HTTP Range Requests
```

This allows:

* seeking
* buffering
* large file playback
* direct browser streaming

No external streaming server required.

---

# Security

Basic protections included:

* root directory sandbox
* safe file ID hashing
* path traversal prevention

However this server is designed for **home use**.

Do not expose directly to the internet without a reverse proxy.

Recommended:

```
Nginx
Caddy
Traefik
```

---

# Performance

Typical usage:

* thousands of media files
* multiple simultaneous streams
* minimal CPU usage

Most CPU load comes from:

```
ffmpeg thumbnail generation
```

which happens only once per file.

---

# Running as a Service

Linux systemd example:

```
[Unit]
Description=Go Media Server

[Service]
ExecStart=/usr/local/bin/MediaSvr -root /media
Restart=always

[Install]
WantedBy=multi-user.target
```

---

# Known Limitations

* Browser codec support varies
* MKV may not play everywhere
* No user authentication
* No adaptive streaming (HLS)

---

# Planned Improvements

Possible future upgrades:

* HLS streaming fallback
* transcoding for unsupported formats
* user accounts
* watch history
* resume playback
* continue watching row
* poster artwork support
* TV series detection

---

# License

Free for personal use.

---

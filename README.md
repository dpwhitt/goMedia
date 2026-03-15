# Go Media Server

A lightweight **Golang media streaming server** that serves video and audio to a web browser with a **Netflix‑style interface**.

Features include password protection, thumbnail generation, streaming with range support, request logging, and a modern responsive UI.

---

# Features

### Streaming

* Browser streaming of video and audio
* HTTP range support for smooth playback
* Works with:

```
mp4
mkv
mov
webm
mp3
m4a
wav
ogg
```

Best browser compatibility:

```
MP4 (H264 + AAC)
```

---

# User Interface

Netflix‑style UI including:

* thumbnail previews
* media duration
* video resolution badge
* modal video player
* search
* sorting
* grouped folders

The layout is constrained to a **fixed maximum width** for better viewing on ultrawide displays.

---

# Security

## Login Page

The server provides a **custom login page** instead of a browser authentication popup.

Session authentication uses a secure cookie.

Run with:

```
-password mysecret
```

Without the flag the server runs **without authentication**.

---

# Download Blocking

The server discourages downloading using:

* `controlsList="nodownload"`
* disabled right‑click on media
* `Content-Disposition: inline`
* no-cache headers

Note:

> If a browser can watch a video it technically receives the bytes, so downloads cannot be prevented completely. These protections stop casual downloading.

---

# Logging

The server logs client requests including:

* client IP
* hostname
* method
* path
* query
* user agent
* referer
* request duration

Example log entry:

```
client_ip=192.168.1.24 hostname=desktop.local method=GET path=/stream/ab83f1 duration=45ms
```

Logging can be disabled:

```
-log-requests false
```

---

# Log File

Logs can be written to a file using:

```
-log-file mediaserver.log
```

Example run:

```
go run MediaSvr.go \
-root "D:\\Media" \
-password mysecret \
-log-file server.log
```

If not specified, logs are printed to the console.

---

# Thumbnails

Thumbnails are automatically generated using **ffmpeg**.

Requirements:

```
ffmpeg
ffprobe
```

Thumbnail features:

* automatic generation
* cached in `.thumbs` folder
* resolution badge overlay
* duration overlay

---

# Installation

### Install Go

```
https://go.dev/dl/
```

### Install FFmpeg

Windows:

```
winget install ffmpeg
```

Linux:

```
sudo apt install ffmpeg
```

Mac:

```
brew install ffmpeg
```

---

# Running the Server

Example:

```
go run MediaSvr.go -root "D:\\Movies"
```

Open browser:

```
http://localhost:8080
```

---

# Command Line Options

| Flag             | Description                |
| ---------------- | -------------------------- |
| `-root`          | Media library root folder  |
| `-addr`          | HTTP listen address        |
| `-password`      | Enable login page          |
| `-log-file`      | Write logs to file         |
| `-log-requests`  | Enable request logging     |
| `-scan-on-start` | Scan library at startup    |
| `-refresh`       | Periodic library rescan    |
| `-thumb-width`   | Thumbnail width            |
| `-thumb-height`  | Thumbnail height           |
| `-thumb-offset`  | Time offset for thumbnails |
| `-allow-audio`   | Include audio files        |

Example full configuration:

```
go run MediaSvr.go \
-root "D:\\Media" \
-password netflix123 \
-log-file mediaserver.log \
-log-requests true \
-refresh 5m
```

---

# Folder Layout Example

```
Media
 ├── Movies
 │   ├── Interstellar.mp4
 │   └── BladeRunner2049.mkv
 ├── TV
 │   └── Series
 │       └── Episode1.mp4
 └── Music
     └── Album
         └── Track01.mp3
```

The server groups media based on folder structure.

---

# Performance Notes

For large libraries:

* first scan may take time
* thumbnails generate on first view
* cached thumbnails are reused

Recommended:

```
-refresh 5m
```

---

# Limitations

* MKV playback depends on browser codec support
* Downloads cannot be completely prevented
* ffmpeg required for thumbnails

---

# Future Improvements

Possible upgrades:

* HLS streaming
* hardware transcoding
* resume playback
* user accounts
* watch history
* media metadata scraping

---

# License

Open source example project.

Use freely and modify as needed.

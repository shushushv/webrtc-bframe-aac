# webrtc-bframe-aac

Stream H.264 B-frames and AAC audio over WebRTC — server-side PTS reordering (Go/Pion) + client-side WebCodecs decoding via EncodedInsertableStreams.

## Quick Start

```bash
go run ./server/
```

Open http://localhost:8080 and click **Subscribe**.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-video` | `sample/video.h264` | H.264 (Annex-B) file to stream |
| `-audio` | `sample/audio.aac` | AAC (ADTS) file to stream |
| `-v` | `0` | Log verbosity: 0=info, 1=debug, 2=verbose (per-frame) |

When `-v` is set the printed URL includes `?v=<level>` so the browser console matches.

See [`sample/README.md`](sample/README.md) for FFmpeg commands to generate compatible files.

## Project Structure

```
.
├── server/
│   ├── main.go       # WHEP handler, WebRTC setup
│   ├── video.go      # H.264 NAL parsing, PTS assignment, RTP streaming
│   └── audio.go      # ADTS parsing, LATM packing, RTP streaming
├── web/
│   ├── index.html    # Signalling + receiver setup
│   ├── video-player.js  # WebCodecsVideoPlayer (B-frame reorder buffer)
│   └── audio-player.js  # WebCodecsAudioPlayer (Opus trick + AudioDecoder)
└── sample/
    ├── video.h264
    ├── audio.aac
    └── README.md     # FFmpeg commands
```

## Articles

- [English](article-en.md)
- [中文](article-zh.md)

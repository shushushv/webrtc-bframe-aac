# Playing H.264 B-frames and AAC Audio over WebRTC — Without Patching the Browser

> **TL;DR:** Standard WebRTC stacks set RTP timestamps from DTS, which breaks B-frame video playback.  We fix this on the server by computing PTS manually and stamping every RTP packet ourselves.  On the client we intercept encoded frames with `EncodedInsertableStreams`, reorder them with a small sort buffer, and decode them with `VideoDecoder`.  AAC audio gets smuggled in by rewriting the Offer SDP — replacing Opus with `MP4A-LATM` — so the browser never refuses the transceiver.  A complete, self-contained demo (Go server + plain JS client) is on [GitHub](https://github.com/shushushv/webrtc-bframe-aac).

---

## The Problem

H.264 supports **B-frames** (bidirectional prediction frames).  A B-frame is decoded _after_ the frames it references (both past and future P/I frames), but it is _displayed_ before them.  This creates a split between two timestamps:

| Timestamp | Meaning | Who uses it |
|-----------|---------|-------------|
| **DTS** (Decode Timestamp) | Order in which frames must be decoded | Container/demuxer, RTP stack |
| **PTS** (Presentation Timestamp) | Order in which frames should be displayed | Renderer / video element |

A raw `.h264` annex-B file stores frames in **DTS order** (I, P₁, B₁, B₂, P₂, …).  Standard WebRTC libraries read that stream and simply tick the RTP timestamp forward by one frame duration per packet — effectively treating RTP timestamp as DTS.

When the browser receives those packets it feeds them to its internal decoder in arrival order, using the RTP timestamp as the render time.  With B-frames the render time is wrong: P₁ arrives and its timestamp says "display me now", but B₁ and B₂ that should appear _before_ P₁ arrive later.  The result is flickering and out-of-order rendering.

### Why this is hard to fix "the normal way"

The usual fix is to compute PTS from the H.264 bitstream and embed it in an RTSP or MPEG-TS container that carries both timestamps.  WebRTC has no equivalent mechanism — the RTP packet carries a single 32-bit timestamp field, and Pion (like most libraries) writes DTS there.

We need to take full control of that timestamp field.

---

## Part 1 — Fixing B-frames on the Server

### Choosing the Right Track Type

Pion offers two track types for sending media:

- `TrackLocalStaticSample` — accepts raw samples, runs a packetizer internally, **auto-increments the timestamp**.  Convenient, but the timestamp is always DTS-based.
- `TrackLocalStaticRTP` — accepts pre-built `rtp.Packet` structs.  We own the timestamp field entirely.

We use `TrackLocalStaticRTP` and compute PTS ourselves.

```go
videoTrack, err := webrtc.NewTrackLocalStaticRTP(
    webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
    "video", "pion",
)
```

### Computing PTS from a DTS-Ordered NAL Stream

An H.264 encoder with `-bf 2` produces groups like:

```
DTS order (file order):  I   P₁  B₁  B₂  P₂  B₃  B₄  P₃ …
PTS order (display):     I   B₁  B₂  P₁  B₃  B₄  P₂  …
```

The rule for a group **[P, B₁…Bₙ]** (P appears in the file, followed by n B-frames, then the next anchor):

```
P.pts  = lastAnchorPTS + (n+1) × frameDuration
Bᵢ.pts = lastAnchorPTS + i    × frameDuration   (i = 1…n)
```

This looks straightforward, but there are two gotchas:

1. **Units**: Use 90 kHz clock ticks (not milliseconds).  `frameDuration = 90000 / fps = 3600` for 25 fps.
2. **Non-picture NALs**: SPS, PPS, SEI, and filler units are interspersed with picture NALs.  They must inherit the PTS of the picture NAL they precede.

Here is the core of `assignPTS` from the demo:

```go
const (
    videoClockRate     = 90000
    videoFrameRate     = 25
    frameDurationTicks = videoClockRate / videoFrameRate // 3600
)

func assignPTS(frames []FrameInfo) []FrameInfo {
    // Find first IDR and set everything before it to PTS=0
    // …

    lastAnchorPTS := uint32(0)
    cur := firstIDR + 1

    for cur < len(frames) {
        if !isPicture(frames[cur].nal.UnitType) {
            frames[cur].pts = lastAnchorPTS  // inherit from anchor
            cur++
            continue
        }

        // Find next anchor (non-B picture frame)
        nextAnchor := findNextAnchor(frames, cur+1)
        numB := countBFramesBetween(frames, cur+1, nextAnchor)

        anchorPTS := lastAnchorPTS + uint32(numB+1)*frameDurationTicks
        frames[cur].pts = anchorPTS  // P frame gets its future PTS

        bIdx := 0
        for i := cur + 1; i < nextAnchor; i++ {
            if isPicture(frames[i].nal.UnitType) {
                bIdx++
                frames[i].pts = lastAnchorPTS + uint32(bIdx)*frameDurationTicks
            }
        }

        lastAnchorPTS = anchorPTS
        cur = nextAnchor
    }
    return frames
}
```

### Detecting B-frames: Parsing `slice_type`

We use `github.com/Eyevinn/mp4ff/avc` to parse the NAL slice header and read `slice_type`:

```go
func isBFrame(nalData []byte) bool {
    nalType := nalData[0] & 0x1F
    if nalType != 1 && nalType != 5 { return false }
    sliceType, err := avc.GetSliceTypeFromNALU(nalData)
    if err != nil { return false }
    return sliceType == avc.SLICE_B
}
```

### Packetizing with Correct PTS

For each frame we use `codecs.H264Payloader` to split large NAL units into FU-A RTP fragments, then stamp every packet with the pre-computed PTS:

```go
payloader := &codecs.H264Payloader{}
var seqNum uint16

for _, frame := range frames {
    pts := loopOffset + frame.pts  // loopOffset keeps timestamps monotonic across loops

    payloads := payloader.Payload(1200, frame.nal.Data)
    for i, payload := range payloads {
        pkt := &rtp.Packet{
            Header: rtp.Header{
                Version:        2,
                Marker:         i == len(payloads)-1,
                PayloadType:    96,
                SequenceNumber: seqNum,
                Timestamp:      pts,   // ← PTS, not DTS
                SSRC:           0x12345678,
            },
            Payload: payload,
        }
        seqNum++
        videoTrack.WriteRTP(pkt)
    }
}
```

Note `loopOffset`: when the file loops, we add the total duration of the previous pass so the timestamp is always monotonically increasing.  Without this the browser's jitter buffer sees the timestamp jump backwards and drops most frames.

---

## Part 2 — Decoding B-frames on the Client

### Why the Native WebRTC Pipeline Fails

Even with correct PTS in the RTP timestamp, the browser's built-in H.264 decoder still renders frames in arrival order.  The `<video>` element has no concept of "this RTP timestamp is PTS, not DTS — hold it until its B-frames arrive".

We need to bypass the native pipeline entirely.

### EncodedInsertableStreams + WebCodecs

`EncodedInsertableStreams` (sometimes called "Insertable Streams for Media") lets us intercept encoded frames _before_ the browser decodes them.  We create a `TransformStream` that sits between the RTP decoder and the native decoder — and we redirect the data to our own `VideoDecoder` instead.

```js
function setupVideoReceiver(receiver) {
    const { readable, writable } = receiver.createEncodedStreams();
    readable
        .pipeThrough(new TransformStream({
            transform(encodedFrame, controller) {
                videoPlayer.decodeFrame(encodedFrame);
                controller.enqueue(encodedFrame);  // keep the stream alive
            },
        }))
        .pipeTo(writable);
}
```

### Reading PTS from rtpTimestamp

`RTCEncodedVideoFrame.getMetadata()` exposes `rtpTimestamp` — the exact value we wrote on the server.  We convert from 90 kHz ticks to microseconds (WebCodecs uses µs):

```js
decodeFrame(encodedFrame) {
    const rtpTs = encodedFrame.getMetadata().rtpTimestamp;
    const timestampUs = (rtpTs / 90000) * 1e6;

    const chunk = new EncodedVideoChunk({
        type: encodedFrame.type === 'key' ? 'key' : 'delta',
        timestamp: timestampUs,
        data: encodedFrame.data,
    });
    this._decoder.decode(chunk);
}
```

### The Frame Reorder Buffer

`VideoDecoder` decodes frames in submission order and calls `output` in the same order.  B-frames arrive in DTS order (P₁, B₁, B₂) but must be displayed in PTS order (B₁, B₂, P₁).  We maintain a small buffer that sorts by `timestamp` before writing to a `MediaStreamTrackGenerator`:

```js
_onFrame(frame) {
    this._frameBuffer.push(frame);
    this._frameBuffer.sort((a, b) => a.timestamp - b.timestamp);

    if (this._frameBuffer.length >= this.bufferSize) {  // default: 5 frames
        const oldest = this._frameBuffer.shift();
        this._writer.write(oldest).then(() => oldest.close());
    }
}
```

The buffer depth needs to be at least `maxBframes + 1`.  Five frames works well in practice and adds only ~200 ms of latency at 25 fps.

### Rendering via MediaStreamTrackGenerator

`MediaStreamTrackGenerator` creates a `MediaStreamTrack` that we can write decoded `VideoFrame` objects into.  We attach it to a `<video>` element:

```js
const generator = new MediaStreamTrackGenerator({ kind: 'video' });
const stream = new MediaStream([generator]);
videoElement.srcObject = stream;
this._writer = generator.writable.getWriter();
```

The full pipeline:

```
RTP packets
    └─► EncodedInsertableStreams (intercept)
            └─► VideoDecoder (WebCodecs)
                    └─► 5-frame sort buffer
                            └─► MediaStreamTrackGenerator
                                    └─► <video> element
```

---

## Part 3 — AAC Audio: The Opus Trick

### The Browser Codec Wall

The WebRTC specification only mandates Opus for audio.  When you call `pc.addTransceiver('audio', { direction: 'recvonly' })` the browser creates an SDP offer containing only `opus/48000/2`.  There is no API to substitute a different codec — the browser controls the offer.

AAC is not in the mandatory codec list.  If you register a custom `MP4A-LATM` codec in Pion and return an answer referencing it, Chrome will reject the connection with _"unable to populate media section, RTPSender created with no codecs"_ or silently produce no audio.

### The Fix: Rewrite the SDP Before Sending

The browser's SDP offer is just a string.  We rewrite it _after_ `createOffer()` but _before_ posting it to the server:

```js
function rewriteSDPAudioToAAC(sdp, sampleRate) {
    // Find the PT for Opus (e.g. "a=rtpmap:111 opus/48000/2")
    const match = sdp.match(/a=rtpmap:(\d+) opus\/48000\/2/i);
    if (!match) return sdp;

    const pt = match[1];
    return sdp
        .replace(
            new RegExp(`a=rtpmap:${pt} opus/48000/2`, 'i'),
            `a=rtpmap:${pt} MP4A-LATM/${sampleRate}/2`,
        )
        .replace(
            new RegExp(`a=fmtp:${pt} [^\r\n]+`),
            `a=fmtp:${pt} profile-level-id=1;object=2;cpresent=0`,
        );
}
```

The browser has already created the transceiver (it thinks it's Opus).  After the rewrite the server sees `MP4A-LATM` and negotiates AAC.  The browser receives AAC-encoded RTP — and, because we intercept the stream with `EncodedInsertableStreams` before it reaches the native Opus decoder, we can decode it ourselves with `AudioDecoder`.

### Packaging AAC as MP4A-LATM (RFC 3016)

`MP4A-LATM` is the RTP payload format defined in RFC 3016.  With `cpresent=0` (config not present in the stream — it is in the SDP instead), each RTP packet is:

```
PayloadLengthInfo | PayloadMux
```

`PayloadLengthInfo` uses a variable-length scheme: emit `0xFF` bytes while the remaining length ≥ 255, then emit the remainder.

```go
func packLATM(frame []byte) []byte {
    remaining := len(frame)
    var lenBytes []byte
    for remaining >= 255 {
        lenBytes = append(lenBytes, 0xFF)
        remaining -= 255
    }
    lenBytes = append(lenBytes, byte(remaining))

    out := make([]byte, len(lenBytes)+len(frame))
    copy(out, lenBytes)
    copy(out[len(lenBytes):], frame)
    return out
}
```

The server strips the 7-byte ADTS header from each frame before calling `packLATM`, so the payload is raw AAC.

### The SDP config Field: StreamMuxConfig

The `fmtp` line for `MP4A-LATM` must carry a `config=` hex string encoding a **StreamMuxConfig** as defined in ISO 14496-3.  For a single-layer AAC stream it is 44 bits (we zero-pad to 48):

```
1  audioMuxVersion           = 0
1  allStreamsSameTimeFraming = 1
6  numSubFrames              = 0
4  numProgram                = 0
3  numLayer                  = 0
── AudioSpecificConfig (16 bits total) ──
5  audioObjectType           = profile (e.g. 2 for LC)
4  samplingFrequencyIndex    = index into rate table
4  channelConfiguration      = number of channels
1  frameLengthFlag           = 0
1  dependsOnCoreCoder        = 0
1  extensionFlag             = 0
── Back to StreamMuxConfig ──
3  frameLengthType           = 0 (variable length)
8  latmBufferFullness        = 0xFF
1  otherDataPresent          = 0
1  crcCheckPresent           = 0
4  (zero padding)
```

We compute this dynamically from the ADTS header of the source file:

```go
func (c aacConfig) streamMuxConfig() []byte {
    srIdx := indexFor(c.sampleRate)  // lookup in adtsSampleRateTable

    var bits uint64
    bits = (bits<<1) | 0                   // audioMuxVersion
    bits = (bits<<1) | 1                   // allStreamsSameTimeFraming
    bits = (bits<<6) | 0                   // numSubFrames
    bits = (bits<<4) | 0                   // numProgram
    bits = (bits<<3) | 0                   // numLayer
    bits = (bits<<5) | uint64(c.profile)   // audioObjectType
    bits = (bits<<4) | uint64(srIdx)       // samplingFrequencyIndex
    bits = (bits<<4) | uint64(c.channels)  // channelConfiguration
    bits = (bits<<1) | 0                   // frameLengthFlag
    bits = (bits<<1) | 0                   // dependsOnCoreCoder
    bits = (bits<<1) | 0                   // extensionFlag
    bits = (bits<<3) | 0                   // frameLengthType
    bits = (bits<<8) | 0xFF               // latmBufferFullness
    bits = (bits<<1) | 0                   // otherDataPresent
    bits = (bits<<1) | 0                   // crcCheckPresent
    bits = bits << 4                       // zero-pad to 48 bits

    out := make([]byte, 6)
    for i := 5; i >= 0; i-- {
        out[i] = byte(bits & 0xFF)
        bits >>= 8
    }
    return out
}
```

The resulting hex goes into the SDP answer:

```
a=fmtp:97 profile-level-id=1;object=2;cpresent=0;config=400024103fc0
```

### Decoding on the Client: AudioDecoder

On the client, after stripping the LATM length prefix, we feed the raw AAC frame to `AudioDecoder`:

```js
decodeFrame(encodedFrame) {
    const aacData = parseLATM(encodedFrame.data);
    this.decoder.decode(new EncodedAudioChunk({
        type: 'key',  // every AAC frame is independently decodable
        timestamp: (encodedFrame.getMetadata().rtpTimestamp / this.sampleRate) * 1e6,
        data: aacData,
    }));
}

function parseLATM(buffer) {
    const view = new DataView(buffer);
    let offset = 0, payloadLength = 0, byte;
    do {
        byte = view.getUint8(offset++);
        payloadLength += byte;
    } while (byte === 0xFF);
    return buffer.slice(offset, offset + payloadLength);
}
```

Decoded `AudioData` objects are scheduled for gapless playback using `AudioContext`:

```js
_onAudioData(audioData) {
    const buffer = this.audioCtx.createBuffer(
        audioData.numberOfChannels,
        audioData.numberOfFrames,
        audioData.sampleRate,
    );
    for (let i = 0; i < audioData.numberOfChannels; i++) {
        audioData.copyTo(buffer.getChannelData(i), { planeIndex: i });
    }
    audioData.close();

    const source = this.audioCtx.createBufferSource();
    source.buffer = buffer;
    source.connect(this.audioCtx.destination);

    const now = this.audioCtx.currentTime;
    if (this.nextPlayTime < now) this.nextPlayTime = now + 0.1;  // jitter buffer
    source.start(this.nextPlayTime);
    this.nextPlayTime += buffer.duration;
}
```

---

## Architecture Diagram

```
┌────────────────────── Go Server (Pion) ─────────────────────────┐
│                                                                  │
│  video.h264 ──► readAllNALs() ──► assignPTS() ──► H264Payloader │
│                                                    │             │
│  audio.aac  ──► readADTSFile() ──► packLATM()     │             │
│                      │                │            │             │
│                      └────────────────┴──► TrackLocalStaticRTP  │
│                                                    │             │
└────────────────────────────────────────────────────┼────────────┘
                            WebRTC/SRTP               │
┌───────────────────── Browser (Chrome) ──────────────┼────────────┐
│                                                     ▼            │
│  EncodedInsertableStreams ◄─────────────── RTP demux             │
│       │                │                                         │
│       ▼                ▼                                         │
│  VideoDecoder      AudioDecoder (mp4a.40.2)                      │
│       │                │                                         │
│  5-frame sort buf  AudioContext (gapless)                        │
│       │                                                          │
│  MediaStreamTrackGenerator ──► <video>                           │
└──────────────────────────────────────────────────────────────────┘
```

---

## Running the Demo

### Prerequisites

- Go 1.23+
- Chrome 94+ (or Safari 17.4+)
- An H.264 file with B-frames and an AAC file (sample files included)

```bash
git clone https://github.com/shushushv/webrtc-bframe-aac
cd webrtc-bframe-aac
go run ./server/
```

Open `http://localhost:8080`, click **Subscribe**.

To use your own files:

```bash
# Generate H.264 with B-frames
ffmpeg -i input.mp4 -an -c:v libx264 -bf 2 -g 60 -r 25 sample/video.h264

# Extract AAC
ffmpeg -i input.mp4 -vn -c:a aac -b:a 128k sample/audio.aac

go run ./server/ -video sample/video.h264 -audio sample/audio.aac
```

---

## Browser Compatibility

| Feature | Chrome | Firefox | Safari |
|---------|--------|---------|--------|
| `EncodedInsertableStreams` | 86+ | 117+ | 15.4+ |
| `VideoDecoder` | 94+ | 130+ | 16.1+ |
| `MediaStreamTrackGenerator` | 94+ | — | 17.4+ |
| `AudioDecoder` (AAC) | 94+ | 130+ | 16.1+ |
| **Full demo** | **94+** | **—** | **17.4+** |

Firefox does not yet implement `MediaStreamTrackGenerator`, so the video path is Chrome/Safari only.  The audio path (`AudioDecoder` + `AudioContext`) works in Firefox too, but requires a different rendering approach.

---

## Limitations and Next Steps

- **Latency**: The 5-frame sort buffer adds ~200 ms at 25 fps.  Streams with fewer B-frames (or no B-frames at all) can use a smaller buffer.
- **Lip-sync**: Video and audio are started independently.  A production implementation should synchronize them via their respective RTP timestamps.
- **RTCP**: The server drains RTCP packets but ignores them.  PLI/FIR (keyframe requests) are not handled; in a real deployment you would respond to these.
- **WHEP**: The signalling is a minimal WHEP-like HTTP exchange.  A real WHEP implementation follows the full RFC 9725 spec including `Link` headers and PATCH.
- **SDP rewrite fragility**: The Opus trick depends on the browser putting Opus in the SDP and using the exact format we match.  It has been stable across Chrome and Safari for several years, but it is not guaranteed by any spec.

---

## Conclusion

Playing H.264 with B-frames over WebRTC requires breaking out of the standard sample-based track API and taking ownership of two things: the RTP timestamp (use PTS, not DTS) and the decoding pipeline (use WebCodecs, not the browser's built-in decoder).  Adding AAC audio on top requires one more workaround — the SDP rewrite — but the result is a fully functional playback pipeline that handles both B-frames and non-standard audio codecs without any browser patches or custom builds.

The complete source code, including the Go server and the plain-JS client, is available at [github.com/shushushv/webrtc-bframe-aac](https://github.com/shushushv/webrtc-bframe-aac).

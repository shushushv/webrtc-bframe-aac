# 在 WebRTC 中播放 H.264 B 帧与 AAC 音频——无需修改浏览器

> **摘要：** 标准 WebRTC 栈以 DTS 为 RTP 时间戳，这会导致含 B 帧的视频播放乱序闪烁。本文的解法是在服务端手动计算 PTS，将每个 RTP 包的时间戳设置为 PTS 而非 DTS；在客户端通过 `EncodedInsertableStreams` 拦截编码帧，用一个小型排序缓冲区将 B 帧归位，再由 `VideoDecoder` 解码渲染。AAC 音频则通过 SDP Offer 改写（把 Opus 替换成 `MP4A-LATM`）绕过浏览器的 Codec 限制，由 `AudioDecoder` 在客户端解码。完整 Demo（Go 服务端 + 纯 JS 客户端）已开源：[GitHub](https://github.com/shushushv/webrtc-bframe-aac)。

---

## 问题背景

H.264 支持 **B 帧**（双向预测帧）。B 帧在解码时依赖前后两个参考帧（过去和未来的 P/I 帧），但它的显示时间早于它所依赖的前向参考帧。这就产生了两种时间戳：

| 时间戳 | 含义 | 使用者 |
|--------|------|--------|
| **DTS**（解码时间戳）| 帧的解码顺序 | 容器/解封装器、RTP 栈 |
| **PTS**（显示时间戳）| 帧的显示顺序 | 渲染器 / video 元素 |

一个原始 `.h264` Annex-B 文件以 **DTS 顺序** 存储帧（I、P₁、B₁、B₂、P₂……）。标准 WebRTC 库（如 Pion）读取这个码流，每发一个包就把 RTP 时间戳递增一个帧间隔——本质上是把 RTP 时间戳当成了 DTS。

浏览器收到这些包后，按到达顺序送入解码器，并以 RTP 时间戳作为渲染时间。对含 B 帧的视频来说，渲染时间是错的：P₁ 先到，时间戳说"现在渲染我"，但本应在 P₁ 之前显示的 B₁、B₂ 却在 P₁ 之后才到。结果就是画面闪烁、乱序。

### 为什么常规方案行不通

常规做法是把 PTS 算好后放进 RTSP 或 MPEG-TS 容器（这两种容器同时携带 DTS 和 PTS）。WebRTC 没有等效机制——RTP 包头只有一个 32 位时间戳字段，Pion 等库默认写 DTS。

要解决这个问题，我们必须完全接管那个时间戳字段。

---

## 第一部分：服务端修复 B 帧

### 选择正确的 Track 类型

Pion 提供两种发送媒体的 Track 类型：

- `TrackLocalStaticSample`：接受原始样本，内部自动打包，**自动递增时间戳**。方便，但时间戳始终是 DTS。
- `TrackLocalStaticRTP`：接受手动构造的 `rtp.Packet`，**时间戳完全由调用方控制**。

我们选 `TrackLocalStaticRTP`，自己计算 PTS。

```go
videoTrack, err := webrtc.NewTrackLocalStaticRTP(
    webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
    "video", "pion",
)
```

### 从 DTS 顺序的 NAL 流计算 PTS

编码器以 `-bf 2` 参数输出的帧序列如下：

```
DTS 顺序（文件顺序）：I   P₁  B₁  B₂  P₂  B₃  B₄  P₃ …
PTS 顺序（显示顺序）：I   B₁  B₂  P₁  B₃  B₄  P₂  …
```

对于每组 **[P, B₁…Bₙ]**（P 在文件中先出现，后跟 n 个 B 帧，再跟下一个锚帧）的公式：

```
P.pts  = lastAnchorPTS + (n+1) × frameDuration
Bᵢ.pts = lastAnchorPTS + i    × frameDuration   （i = 1…n）
```

有两个容易踩的坑：

1. **单位**：用 90 kHz 时钟节拍（而不是毫秒）。`frameDuration = 90000 / fps = 3600`（25 fps 时）。
2. **非图像 NAL**：SPS、PPS、SEI、填充单元会夹在图像 NAL 之间，它们需要继承所在组的锚帧 PTS。

`assignPTS` 的核心逻辑如下：

```go
const (
    videoClockRate     = 90000
    videoFrameRate     = 25
    frameDurationTicks = videoClockRate / videoFrameRate // 3600
)

func assignPTS(frames []FrameInfo) []FrameInfo {
    // 找到第一个 IDR，其之前的帧 PTS=0
    // …

    lastAnchorPTS := uint32(0)
    cur := firstIDR + 1

    for cur < len(frames) {
        if !isPicture(frames[cur].nal.UnitType) {
            frames[cur].pts = lastAnchorPTS  // 继承锚帧 PTS
            cur++
            continue
        }

        // 找到下一个锚帧（非 B 图像帧）
        nextAnchor := findNextAnchor(frames, cur+1)
        numB := countBFramesBetween(frames, cur+1, nextAnchor)

        anchorPTS := lastAnchorPTS + uint32(numB+1)*frameDurationTicks
        frames[cur].pts = anchorPTS  // P 帧获得"未来"的 PTS

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

### 检测 B 帧：解析 slice_type

利用 `github.com/Eyevinn/mp4ff/avc` 解析 NAL slice header，读取 `slice_type`：

```go
func isBFrame(nalData []byte) bool {
    nalType := nalData[0] & 0x1F
    if nalType != 1 && nalType != 5 { return false }
    sliceType, err := avc.GetSliceTypeFromNALU(nalData)
    if err != nil { return false }
    return sliceType == avc.SLICE_B
}
```

### 打包并写入正确的 PTS

用 `codecs.H264Payloader` 将大 NAL 单元拆分为 FU-A RTP 分片，再把预先计算好的 PTS 写入每个包：

```go
payloader := &codecs.H264Payloader{}
var seqNum uint16

for _, frame := range frames {
    pts := loopOffset + frame.pts  // loopOffset 保证循环时时间戳单调递增

    payloads := payloader.Payload(1200, frame.nal.Data)
    for i, payload := range payloads {
        pkt := &rtp.Packet{
            Header: rtp.Header{
                Version:        2,
                Marker:         i == len(payloads)-1,
                PayloadType:    96,
                SequenceNumber: seqNum,
                Timestamp:      pts,   // ← PTS，而非 DTS
                SSRC:           0x12345678,
            },
            Payload: payload,
        }
        seqNum++
        videoTrack.WriteRTP(pkt)
    }
}
```

`loopOffset` 的作用：文件循环时累加上一轮的总时长，确保 RTP 时间戳始终单调递增。否则时间戳归零，浏览器的抖动缓冲区会丢弃大量帧。

---

## 第二部分：客户端解码 B 帧

### 为什么原生 WebRTC 管道不够用

即使服务端在 RTP 时间戳里写了正确的 PTS，浏览器内置的 H.264 解码器仍然按到达顺序渲染帧。`<video>` 元素根本不知道"这个 RTP 时间戳是 PTS，需要等它的 B 帧到齐再渲染"。

我们必须完全绕开原生管道。

### EncodedInsertableStreams + WebCodecs

`EncodedInsertableStreams`（可插入流）允许我们在浏览器解码前拦截编码帧。我们插入一个 `TransformStream`，把数据重定向到自己的 `VideoDecoder`，而不是原生解码器。

```js
function setupVideoReceiver(receiver) {
    const { readable, writable } = receiver.createEncodedStreams();
    readable
        .pipeThrough(new TransformStream({
            transform(encodedFrame, controller) {
                videoPlayer.decodeFrame(encodedFrame);
                controller.enqueue(encodedFrame);  // 保持流管道存活
            },
        }))
        .pipeTo(writable);
}
```

### 从 rtpTimestamp 读取 PTS

`RTCEncodedVideoFrame.getMetadata()` 暴露了 `rtpTimestamp`——正是我们在服务端写入的值。转换单位：90 kHz → 微秒（WebCodecs 使用微秒）：

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

### 帧排序缓冲区

`VideoDecoder` 按提交顺序解码，`output` 回调也以同样顺序触发。B 帧以 DTS 顺序到达（P₁、B₁、B₂），但需以 PTS 顺序显示（B₁、B₂、P₁）。我们维护一个小缓冲区，按 `timestamp` 排序后再写入 `MediaStreamTrackGenerator`：

```js
_onFrame(frame) {
    this._frameBuffer.push(frame);
    this._frameBuffer.sort((a, b) => a.timestamp - b.timestamp);

    if (this._frameBuffer.length >= this.bufferSize) {  // 默认 5 帧
        const oldest = this._frameBuffer.shift();
        this._writer.write(oldest).then(() => oldest.close());
    }
}
```

缓冲区深度至少需要 `maxBframes + 1`。实测 5 帧效果良好，在 25 fps 下额外引入约 200 ms 延迟。

### 通过 MediaStreamTrackGenerator 渲染

`MediaStreamTrackGenerator` 创建一个可写入解码后 `VideoFrame` 的 `MediaStreamTrack`，挂到 `<video>` 元素上播放：

```js
const generator = new MediaStreamTrackGenerator({ kind: 'video' });
const stream = new MediaStream([generator]);
videoElement.srcObject = stream;
this._writer = generator.writable.getWriter();
```

完整数据管道：

```
RTP 包
    └─► EncodedInsertableStreams（拦截）
            └─► VideoDecoder（WebCodecs）
                    └─► 5 帧排序缓冲区
                            └─► MediaStreamTrackGenerator
                                    └─► <video> 元素
```

---

## 第三部分：AAC 音频——Opus 偷梁换柱

### 浏览器的 Codec 限制

WebRTC 规范只强制要求 Opus 音频。当你调用 `pc.addTransceiver('audio', { direction: 'recvonly' })` 时，浏览器生成的 SDP Offer 只包含 `opus/48000/2`，没有任何 API 可以替换成其他 Codec——浏览器掌控着 Offer 内容。

AAC 不在强制 Codec 列表里。如果你在 Pion 里注册 `MP4A-LATM` 并在 Answer 中引用它，Chrome 会报错 "unable to populate media section, RTPSender created with no codecs" 或静默失去音频。

### 解法：发送前改写 SDP

浏览器的 SDP Offer 不过是一段字符串。我们在 `createOffer()` 之后、POST 到服务端之前改写它：

```js
function rewriteSDPAudioToAAC(sdp, sampleRate) {
    // 找到 Opus 的 payload type（例如 "a=rtpmap:111 opus/48000/2"）
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

浏览器已经创建了收发器（它以为那是 Opus）。改写后服务端看到的是 `MP4A-LATM`，于是推送 AAC。浏览器收到 AAC 编码的 RTP 包——由于我们通过 `EncodedInsertableStreams` 在原生 Opus 解码器之前拦截了数据流，可以用 `AudioDecoder` 自行解码。

### AAC 的 RTP 打包：MP4A-LATM（RFC 3016）

`MP4A-LATM` 是 RFC 3016 定义的 RTP 载荷格式。使用 `cpresent=0`（配置信息在 SDP 里，不在流中）时，每个 RTP 包的格式为：

```
PayloadLengthInfo | PayloadMux
```

`PayloadLengthInfo` 采用变长编码：当剩余长度 ≥ 255 时输出 `0xFF`，然后输出余量：

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

服务端在调用 `packLATM` 前先剥除 7 字节的 ADTS 头，因此 payload 是裸 AAC 数据。

### SDP config 字段：StreamMuxConfig

`MP4A-LATM` 的 `fmtp` 行需要携带 `config=` 十六进制字符串，编码一个 **StreamMuxConfig**（ISO 14496-3）。对于单层 AAC 流，其结构为 44 位（补零至 48 位）：

```
1  audioMuxVersion           = 0
1  allStreamsSameTimeFraming = 1
6  numSubFrames              = 0
4  numProgram                = 0
3  numLayer                  = 0
── AudioSpecificConfig（共 16 位）──
5  audioObjectType           = profile（如 AAC-LC = 2）
4  samplingFrequencyIndex    = 采样率表索引
4  channelConfiguration      = 声道数
1  frameLengthFlag           = 0
1  dependsOnCoreCoder        = 0
1  extensionFlag             = 0
── 回到 StreamMuxConfig ──
3  frameLengthType           = 0（变长帧）
8  latmBufferFullness        = 0xFF
1  otherDataPresent          = 0
1  crcCheckPresent           = 0
4  （补零）
```

从源文件的 ADTS 头动态计算：

```go
func (c aacConfig) streamMuxConfig() []byte {
    srIdx := indexFor(c.sampleRate)  // 在 adtsSampleRateTable 中查找

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
    bits = bits << 4                       // 补零至 48 位

    out := make([]byte, 6)
    for i := 5; i >= 0; i-- {
        out[i] = byte(bits & 0xFF)
        bits >>= 8
    }
    return out
}
```

最终 SDP Answer 中的 fmtp 行示例：

```
a=fmtp:97 profile-level-id=1;object=2;cpresent=0;config=400024103fc0
```

### 客户端：AudioDecoder 解码

客户端解析 LATM 长度前缀后，将裸 AAC 帧送入 `AudioDecoder`：

```js
decodeFrame(encodedFrame) {
    const aacData = parseLATM(encodedFrame.data);
    this.decoder.decode(new EncodedAudioChunk({
        type: 'key',  // 每个 AAC 帧都可以独立解码
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

解码后的 `AudioData` 通过 `AudioContext` 无缝衔接播放：

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
    if (this.nextPlayTime < now) this.nextPlayTime = now + 0.1;  // 抖动缓冲
    source.start(this.nextPlayTime);
    this.nextPlayTime += buffer.duration;
}
```

---

## 整体架构

```
┌─────────────────── Go 服务端（Pion）──────────────────────────────┐
│                                                                   │
│  video.h264 ──► readAllNALs() ──► assignPTS() ──► H264Payloader  │
│                                                     │             │
│  audio.aac  ──► readADTSFile() ──► packLATM()      │             │
│                      │                 │            │             │
│                      └─────────────────┴──► TrackLocalStaticRTP  │
│                                                     │             │
└─────────────────────────────────────────────────────┼────────────┘
                        WebRTC / SRTP                  │
┌────────────────── 浏览器（Chrome）─────────────────── ┼────────────┐
│                                                      ▼            │
│  EncodedInsertableStreams ◄──────────────── RTP 解封装            │
│       │                 │                                         │
│       ▼                 ▼                                         │
│  VideoDecoder       AudioDecoder（mp4a.40.2）                     │
│       │                 │                                         │
│  5 帧排序缓冲区     AudioContext（无缝播放）                         │
│       │                                                           │
│  MediaStreamTrackGenerator ──► <video>                            │
└───────────────────────────────────────────────────────────────────┘
```

---

## 运行 Demo

### 前置条件

- Go 1.23+
- Chrome 94+（或 Safari 17.4+）
- 含 B 帧的 H.264 文件 + AAC 文件（仓库附带示例文件）

```bash
git clone https://github.com/shushushv/webrtc-bframe-aac
cd webrtc-bframe-aac
go run ./server/
```

打开 `http://localhost:8080`，点击 **Subscribe**。

如需使用自己的文件：

```bash
# 生成含 B 帧的 H.264
ffmpeg -i input.mp4 -an -c:v libx264 -bf 2 -g 60 -r 25 sample/video.h264

# 提取 AAC
ffmpeg -i input.mp4 -vn -c:a aac -b:a 128k sample/audio.aac

go run ./server/ -video sample/video.h264 -audio sample/audio.aac
```

验证 B 帧是否存在：

```bash
ffprobe -v error -show_frames -select_streams v sample/video.h264 \
  | grep pict_type | sort | uniq -c
# 预期输出中应出现 pict_type=B
```

---

## 浏览器兼容性

| 特性 | Chrome | Firefox | Safari |
|------|--------|---------|--------|
| `EncodedInsertableStreams` | 86+ | 117+ | 15.4+ |
| `VideoDecoder` | 94+ | 130+ | 16.1+ |
| `MediaStreamTrackGenerator` | 94+ | — | 17.4+ |
| `AudioDecoder`（AAC） | 94+ | 130+ | 16.1+ |
| **完整 Demo** | **94+** | **—** | **17.4+** |

Firefox 尚未实现 `MediaStreamTrackGenerator`，因此视频路径仅支持 Chrome/Safari。音频路径（`AudioDecoder` + `AudioContext`）在 Firefox 上也可以工作，但需要不同的渲染方式。

---

## 局限与后续改进方向

- **延迟**：5 帧排序缓冲在 25 fps 下约增加 200 ms 延迟。B 帧数量较少时可缩小缓冲区。
- **音视频同步**：视频和音频独立启动，生产环境需通过 RTP 时间戳进行精确同步。
- **RTCP**：服务端丢弃了所有 RTCP 包，未处理 PLI/FIR（关键帧请求）。生产环境应响应这些消息。
- **WHEP**：信令采用了最简化的类 WHEP HTTP 交换，完整实现应遵循 RFC 9725（包含 `Link` 头和 PATCH 方法）。
- **SDP 改写的脆弱性**：Opus trick 依赖浏览器在 SDP 中使用特定格式的 Opus，多年来在 Chrome 和 Safari 上较为稳定，但没有规范保证。

---

## 小结

在 WebRTC 上正确播放含 B 帧的 H.264，需要做两件事：一是接管 RTP 时间戳（用 PTS，而非 DTS）；二是接管解码管道（用 WebCodecs，而非浏览器内置解码器）。叠加 AAC 音频需要多一步——SDP 改写——但最终能得到一个完整可用的播放方案，无需任何浏览器补丁或定制构建。

完整源码（Go 服务端 + 纯 JS 客户端）：[github.com/shushushv/webrtc-bframe-aac](https://github.com/shushushv/webrtc-bframe-aac)。

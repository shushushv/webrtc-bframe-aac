# 在 WebRTC 中播放 H.264 B 帧与 AAC 音频——无需修改浏览器

> **摘要：** 标准 WebRTC 栈用 DTS 做 RTP 时间戳，含 B 帧的视频到了浏览器就会乱序闪烁。本文的方案：服务端手动计算 PTS 并写入 RTP 时间戳字段；客户端用 `MediaStreamTrackProcessor` 拉取已解码的 `VideoFrame`，通过 `frame.metadata().rtpTimestamp` 读取服务端写入的 PTS，经 5 帧排序缓冲后写入 `MediaStreamTrackGenerator` 渲染。AAC 音频则通过一个 SDP 字符串替换技巧绕过浏览器的 Codec 限制，由 `AudioDecoder` 解码。完整 Demo（Go 服务端 + 纯 JS 客户端）：[github.com/shushushv/webrtc-bframe-aac](https://github.com/shushushv/webrtc-bframe-aac)。

---

## 为什么需要解决这个问题

各大 CDN 厂商正在将 WebRTC 引入直播链路，但推流端编码器默认输出 H.264 B 帧 + AAC，与 WebRTC 只支持 Opus、不支持 B 帧的设计存在冲突。各家厂商在文档里都明确提到了这个问题，给出的解法也如出一辙：**服务端转码**——以延迟和费用为代价：

- **[BytePlus RTM](https://docs.byteplus.com/en/docs/byteplus-media-live/RTM-Streaming-Integration#prerequisites)**：*"If the RTM stream contains B-frames or AAC audio … remove B-frames and transcode the audio to Opus."*

- **[腾讯云 LEB](https://www.tencentcloud.com/document/product/267/41030)**：*"Web 端低延迟直播不支持 B 帧的解码和播放，若推流含有 B 帧，后台会对其进行转码以去除 B 帧，这会增加延迟并产生转码费用。……不支持 AAC，若推流含有 AAC 格式音频，系统会将其转码为 Opus 格式，这会产生音频转码费用。"*

- **[AWS IVS](https://docs.aws.amazon.com/ivs/latest/RealTimeUserGuide/rt-stream-ingest.html)**：*"Starting with version 1.25.0, it automatically disables B-frames when broadcasting to an IVS stage. For real-time streaming with other RTMP encoders, developers must disable B-frames."*

- **[Wowza](https://www.wowza.com/docs/how-to-use-webrtc-with-wowza-streaming-engine)**：*"We recommend disabling B-frames for WebRTC streams."*

本文反其道而行：**服务端原样透传，让浏览器的 WebCodecs 自己处理 B 帧和 AAC。**

---

## 问题背景

H.264 支持 **B 帧**（双向预测帧）。B 帧解码时依赖前后两个参考帧，但它的显示时间早于它所依赖的后向参考帧。这就产生了两种时间戳：

| 时间戳 | 含义 | 使用者 |
|--------|------|--------|
| **DTS**（解码时间戳）| 帧的解码顺序 | 容器/解封装器、RTP 栈 |
| **PTS**（显示时间戳）| 帧的显示顺序 | 渲染器 / `<video>` 元素 |

原始 `.h264` Annex-B 文件以 DTS 顺序存储帧（I、P₁、B₁、B₂、P₂……）。标准 WebRTC 库读取这个码流，每发一个包就把 RTP 时间戳递增一个帧间隔——实际上就是把 DTS 当成了 RTP 时间戳。浏览器按到达顺序渲染，以 RTP 时间戳作为渲染时机，画面就会闪烁乱序。

![B 帧时序问题：DTS ≠ PTS](article-images/fig1-dts-pts.png)

常规做法是把 PTS 放进 RTSP 或 MPEG-TS 容器——这两种容器同时携带 DTS 和 PTS。WebRTC 没有等效机制：RTP 包头只有一个 32 位时间戳字段，Pion 等库默认写 DTS。要解决这个问题，就必须完全接管这个时间戳字段。

---

## 第一部分：服务端修复 B 帧

### 选 TrackLocalStaticRTP，自己掌控时间戳

Pion 提供两种 Track 类型。`TrackLocalStaticSample` 内部自动打包并自动递增时间戳（始终是 DTS）。`TrackLocalStaticRTP` 接受手动构造的 `rtp.Packet`，时间戳完全由调用方控制。这里选后者，在发包前从 DTS 顺序的 NAL 流计算出每帧的 PTS。

对于每组 **[P, B₁…Bₙ]**（P 帧在文件中先出现，后跟 n 个 B 帧，直到下一个锚帧），计算公式如下：

```
P.pts  = lastAnchorPTS + (n+1) × frameDuration
Bᵢ.pts = lastAnchorPTS + i    × frameDuration   （i = 1…n）
```

单位是 90 kHz 时钟节拍（25 fps 时 `frameDuration = 3600`）。非图像 NAL（SPS、PPS、SEI）继承所在组锚帧的 PTS。完整 `assignPTS` 实现见[仓库源码](https://github.com/shushushv/webrtc-bframe-aac/blob/main/server/main.go)；B 帧检测通过 `github.com/Eyevinn/mp4ff/avc` 解析 NAL slice header 中的 `slice_type` 实现。

### 一个字段，两份信息

这是整个方案的关键所在。帧按 **DTS 顺序**发送（与文件存储顺序一致——解码器必须按这个顺序接收），但 **RTP 时间戳字段写的是 PTS**。

这样一来，一个字段就隐式携带了两份信息：
- **DTS 是隐式的**——包的到达顺序就是 DTS 顺序，因为发送前从不对帧重排。
- **PTS 是显式的**——写在 RTP 时间戳字段里，客户端通过 `getMetadata().rtpTimestamp` 读取。

接收端不需要任何额外信令就能还原完整信息：按到达顺序解码（DTS 顺序），按 RTP 时间戳决定渲染时机（PTS）。不需要容器格式，不需要旁路信道，标准的 32 位 RTP 时间戳字段就够了——写进去的是 PTS 还是 DTS，仅此之差。

核心一行——把 PTS 而非 DTS 写入 RTP 时间戳字段：

```go
pkt := &rtp.Packet{
    Header: rtp.Header{
        Timestamp: pts,   // ← PTS，而非 DTS
        Marker:    i == len(payloads)-1,
        // ...
    },
    Payload: payload,
}
videoTrack.WriteRTP(pkt)
```

补充一点：文件循环时，需要把上一轮的总时长累加到 `pts`，确保 RTP 时间戳始终单调递增。一旦时间戳回退，浏览器的抖动缓冲区会丢弃大量帧。

---

## 第二部分：客户端解码 B 帧

即使服务端在 RTP 时间戳里写了正确的 PTS，浏览器内置的 H.264 解码器仍然按到达顺序渲染帧。`<video>` 元素根本不知道"这个 RTP 时间戳是 PTS，要等 B 帧到齐再渲染"。必须完全绕开原生管道。

### ~~方案一：EncodedInsertableStreams + WebCodecs VideoDecoder~~

~~`EncodedInsertableStreams` 允许在 RTP 层与浏览器解码器之间插入一个 `TransformStream`，把数据重定向到自己的 `VideoDecoder`。`RTCEncodedVideoFrame.getMetadata()` 暴露了 `rtpTimestamp`——正是服务端写入的 PTS。将帧按 `timestamp` 排序后再写入 `MediaStreamTrackGenerator`：~~

```js
// 方案一（已弃用）
_onFrame(frame) {
    this._frameBuffer.push(frame);
    this._frameBuffer.sort((a, b) => a.timestamp - b.timestamp);

    if (this._frameBuffer.length >= this.bufferSize) {  // 默认 5 帧
        const oldest = this._frameBuffer.shift();
        this._writer.write(oldest).then(() => oldest.close());
    }
}
```

~~该方案需要手动配置 `VideoDecoder`（codec 字符串、SPS/PPS 初始化），且必须在 `RTCPeerConnection` 构造时传入 `{ encodedInsertableStreams: true }`，连同音频通道一起开启。此外，`EncodedInsertableStreams` 弱网下的帧顺序不受保证（见[局限](#局限与后续改进)第一条）。~~

### 方案二：MediaStreamTrackProcessor（当前方案）

感谢 [Philip Eliasson 在 WebRTC CL 419460 中的建议](https://webrtc-review.googlesource.com/c/src/+/419460/comment/6a0a1921_eafdb295/)，这个方案更简洁：让浏览器原生 H.264 解码器正常工作，在**解码后**用 `MediaStreamTrackProcessor` 拉取 `VideoFrame`，再通过 `frame.metadata().rtpTimestamp` 读取服务端写入的 PTS，完成排序。

```js
_onFrame(frame) {
    const rtpTs = frame.metadata()?.rtpTimestamp;
    const sortKey = rtpTs ?? frame.timestamp;

    this._frameBuffer.push({ frame, sortKey });
    this._frameBuffer.sort((a, b) => a.sortKey - b.sortKey);

    if (this._frameBuffer.length >= this.bufferSize) {  // 默认 5 帧
        const oldest = this._frameBuffer.shift();
        this._writer.write(oldest.frame).then(() => oldest.frame.close());
    }
}
```

完整实现见 [`web/video-player.js`](https://github.com/shushushv/webrtc-bframe-aac/blob/main/web/video-player.js)。

### 两个方案对比

| 维度 | ~~方案一（WebCodecs）~~ | 方案二（TrackProcessor） |
|------|------------------------|--------------------------|
| 拦截位置 | 解码**前**（编码帧） | 解码**后**（解码帧） |
| 需要 `encodedInsertableStreams: true` | 视频、音频均需要 | 仅音频需要（AAC 解码） |
| 需要手动 `VideoDecoder` | 是（需 codec 字符串、SPS/PPS） | 否（原生解码器） |
| PTS 获取方式 | `encodedFrame.getMetadata().rtpTimestamp` | `videoFrame.metadata().rtpTimestamp` |
| 弱网帧顺序保证 | 否（EncodedInsertableStreams 已知问题） | 是（原生 jitter buffer 保证） |
| 浏览器支持 | Chrome 94+、Safari 17.4+ | Chrome 110+（`rtpTimestamp` 字段） |

---

## 第三部分：AAC 音频——Opus 偷梁换柱

### 浏览器的 Codec 封锁

WebRTC 规范只强制要求 Opus 音频。调用 `pc.addTransceiver('audio', { direction: 'recvonly' })` 时，浏览器生成的 SDP Offer 只包含 `opus/48000/2`，没有任何 API 可以替换——Offer 内容由浏览器说了算。

在 Pion 里注册 `MP4A-LATM` 并在 Answer 中引用它，Chrome 会悄悄断掉音频。

### 解法：发送前改写 SDP 字符串

SDP Offer 本质上就是一段字符串。在 `createOffer()` 之后、POST 到服务端之前改写它：

```js
function rewriteSDPAudioToAAC(sdp, sampleRate) {
    const match = sdp.match(/a=rtpmap:(\d+) opus\/48000\/2/i);
    if (!match) return sdp;
    const pt = match[1];
    return sdp
        .replace(new RegExp(`a=rtpmap:${pt} opus/48000/2`, 'i'),
                 `a=rtpmap:${pt} MP4A-LATM/${sampleRate}/2`)
        .replace(new RegExp(`a=fmtp:${pt} [^\r\n]+`),
                 `a=fmtp:${pt} profile-level-id=1;object=2;cpresent=0`);
}
```

浏览器已经创建了收发器（它以为那是 Opus）。改写后服务端看到的是 `MP4A-LATM`，于是推送 AAC。浏览器收到 AAC 编码的 RTP 包——我们已经通过 `EncodedInsertableStreams` 在原生 Opus 解码器之前拦截了数据流，直接用 `AudioDecoder` 自行解码即可。

![SDP 改写流程：Opus 偷梁换柱](article-images/fig2-sdp-trick.png)

### 服务端：RFC 3016 LATM 打包

服务端剥除每帧的 7 字节 ADTS 头，按 RFC 3016 LATM 格式封装（变长长度前缀 + 裸 AAC 数据）。SDP `fmtp` 行携带 `config=` 十六进制字符串，编码一个 **StreamMuxConfig**（ISO 14496-3），其中采样率、声道数和 Profile 均从源文件 ADTS 头动态计算。完整实现见 [`server/audio.go`](https://github.com/shushushv/webrtc-bframe-aac/blob/main/server/audio.go)。

最终 SDP Answer 中的 fmtp 行示例：

```
a=fmtp:97 profile-level-id=1;object=2;cpresent=0;config=400024103fc0
```

### 客户端：AudioDecoder + AudioContext

客户端解析 LATM 长度前缀后，将裸 AAC 帧送入 `AudioDecoder`（codec `mp4a.40.2`）。解码后的 `AudioData` 通过 `AudioContext` 的 `nextPlayTime` 累加器实现无缝衔接播放。完整连接代码见 [`web/audio-player.js`](https://github.com/shushushv/webrtc-bframe-aac/blob/main/web/audio-player.js)。

---

## 整体架构

![完整系统架构](article-images/fig3-pipeline.png)

---

## 运行 Demo

```bash
git clone https://github.com/shushushv/webrtc-bframe-aac
cd webrtc-bframe-aac
go run ./server/
```

打开 `http://localhost:8080`，点击 **Subscribe**。需要 Chrome 94+ 或 Safari 17.4+。

如需使用自己的文件：

```bash
ffmpeg -i input.mp4 -an -c:v libx264 -bf 2 -g 60 -r 25 sample/video.h264
ffmpeg -i input.mp4 -vn -c:a aac -b:a 128k sample/audio.aac
```

---

## 浏览器兼容性

| 特性 | Chrome | Firefox | Safari |
|------|--------|---------|--------|
| `EncodedInsertableStreams`（音频） | 86+ | 117+ | 15.4+ |
| `MediaStreamTrackProcessor` | 94+ | 135+ | — |
| `VideoFrame.metadata().rtpTimestamp` | 110+ | — | — |
| `MediaStreamTrackGenerator` | 94+ | — | 17.4+ |
| `AudioDecoder`（AAC） | 94+ | 130+ | 16.1+ |
| **完整 Demo（方案二）** | **110+** | **—** | **—** |

当前方案（TrackProcessor）依赖 `VideoFrame.metadata().rtpTimestamp`，目前仅 Chrome 110+ 支持。Firefox 和 Safari 暂不支持完整视频链路。

---

## 局限与后续改进

- ~~**尚不适合生产环境（方案一）**：`EncodedInsertableStreams` 不保证帧从 Transform 输出时的顺序。弱网丢包或乱序时，帧可能以错误顺序进入排序缓冲区，导致花屏或解码失败。这是 WebRTC 的已知问题（[issues.webrtc.org/454162516](https://issues.webrtc.org/issues/454162516)），修复补丁正是 [CL 419460](https://webrtc-review.googlesource.com/c/src/+/419460)——也是方案二灵感的来源。方案二绕过了这一问题：在原生 jitter buffer 之后拉取解码帧，顺序由浏览器保证。~~
- **未做音视频同步**：视频和音频管道独立启动，生产环境需通过 RTP 时间戳进行精确同步。
- **延迟**：5 帧排序缓冲在 25 fps 下约增加 200 ms。B 帧数量较少时可以缩小缓冲区。
- **RTCP**：服务端丢弃了所有 RTCP 包，未处理 PLI/FIR 关键帧请求。
- **SDP 改写的脆弱性**：Opus trick 依赖浏览器以特定格式写 Opus，多年来在 Chrome 和 Safari 上比较稳定，但没有规范保证。

---

## 小结

在 WebRTC 上正确播放含 B 帧的 H.264，核心是接管 RTP 时间戳（写 PTS 而非 DTS），然后在客户端用 `MediaStreamTrackProcessor` 拉取已解码帧并按 `rtpTimestamp` 重排——既复用了浏览器原生解码器，又避免了 `EncodedInsertableStreams` 的弱网顺序问题。再叠上 AAC 音频的 SDP 改写技巧，就得到一个完整可用的播放方案，不需要任何浏览器补丁或定制构建。

源码：[github.com/shushushv/webrtc-bframe-aac](https://github.com/shushushv/webrtc-bframe-aac)

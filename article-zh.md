# 在 WebRTC 中播放 H.264 B 帧与 AAC 音频——无需修改浏览器

> **摘要：** 标准 WebRTC 栈以 DTS 为 RTP 时间戳，这会导致含 B 帧的视频播放乱序闪烁。本文的解法是在服务端手动计算 PTS，将每个 RTP 包的时间戳设置为 PTS 而非 DTS；在客户端通过 `EncodedInsertableStreams` 拦截编码帧，用一个小型排序缓冲区将 B 帧归位，再由 `VideoDecoder` 解码渲染。AAC 音频则通过 SDP Offer 改写（把 Opus 替换成 `MP4A-LATM`）绕过浏览器的 Codec 限制，由 `AudioDecoder` 在客户端解码。完整 Demo（Go 服务端 + 纯 JS 客户端）：[github.com/shushushv/webrtc-bframe-aac](https://github.com/shushushv/webrtc-bframe-aac)。

---

## 为什么现在需要解决这个问题

各大 CDN 厂商正在将 WebRTC 引入直播链路，但推流端编码器默认输出 H.264 B 帧 + AAC，与 WebRTC 只支持 Opus、不支持 B 帧的设计相冲突。这一问题已被多家厂商在官方文档中直接点出，且它们的统一解法是**服务端转码**——代价是增加延迟和费用：

- **[BytePlus RTM](https://docs.byteplus.com/en/docs/byteplus-media-live/RTM-Streaming-Integration#prerequisites)**：*"If the RTM stream contains B-frames or AAC audio … remove B-frames and transcode the audio to Opus."*

- **[腾讯云 LEB](https://www.tencentcloud.com/document/product/267/41030)**：*"Web 端低延迟直播不支持 B 帧的解码和播放，若推流含有 B 帧，后台会对其进行转码以去除 B 帧，这会增加延迟并产生转码费用。……不支持 AAC，若推流含有 AAC 格式音频，系统会将其转码为 Opus 格式，这会产生音频转码费用。"*

- **[AWS IVS](https://docs.aws.amazon.com/ivs/latest/RealTimeUserGuide/rt-stream-ingest.html)**：*"Starting with version 1.25.0, it automatically disables B-frames when broadcasting to an IVS stage. For real-time streaming with other RTMP encoders, developers must disable B-frames."*

- **[Wowza](https://www.wowza.com/docs/how-to-use-webrtc-with-wowza-streaming-engine)**：*"We recommend disabling B-frames for WebRTC streams."*

本文采用相反的思路：**服务端不动源流，直接在浏览器端用 WebCodecs 处理 B 帧和 AAC。**

---

## 问题背景

H.264 支持 **B 帧**（双向预测帧）。B 帧在解码时依赖前后两个参考帧，但它的显示时间早于它所依赖的前向参考帧。这就产生了两种时间戳：

| 时间戳 | 含义 | 使用者 |
|--------|------|--------|
| **DTS**（解码时间戳）| 帧的解码顺序 | 容器/解封装器、RTP 栈 |
| **PTS**（显示时间戳）| 帧的显示顺序 | 渲染器 / `<video>` 元素 |

原始 `.h264` Annex-B 文件以 DTS 顺序存储帧（I、P₁、B₁、B₂、P₂……）。标准 WebRTC 库读取这个码流，每发一个包就把 RTP 时间戳递增一个帧间隔——本质上是把 RTP 时间戳当成了 DTS。浏览器按到达顺序渲染，以 RTP 时间戳作为渲染时间，结果就是画面闪烁、乱序。

![B 帧时序问题：DTS ≠ PTS](article-images/fig1-dts-pts.png)

常规做法是把 PTS 放进 RTSP 或 MPEG-TS 容器——这两种容器同时携带 DTS 和 PTS。WebRTC 没有等效机制：RTP 包头只有一个 32 位时间戳字段，Pion 等库默认写 DTS。要解决这个问题，我们必须完全接管那个时间戳字段。

---

## 第一部分：服务端修复 B 帧

### 选 TrackLocalStaticRTP，自己掌控时间戳

Pion 提供两种 Track 类型。`TrackLocalStaticSample` 内部自动打包并自动递增时间戳（始终是 DTS）。`TrackLocalStaticRTP` 接受手动构造的 `rtp.Packet`，时间戳完全由调用方控制。我们选后者，在发包前从 DTS 顺序的 NAL 流计算出每帧的 PTS。

对于每组 **[P, B₁…Bₙ]**（P 在文件中先出现，后跟 n 个 B 帧，再跟下一个锚帧）的公式：

```
P.pts  = lastAnchorPTS + (n+1) × frameDuration
Bᵢ.pts = lastAnchorPTS + i    × frameDuration   （i = 1…n）
```

单位是 90 kHz 时钟节拍（25 fps 时 `frameDuration = 3600`）。非图像 NAL（SPS、PPS、SEI）继承所在组锚帧的 PTS。完整 `assignPTS` 实现见[仓库源码](https://github.com/shushushv/webrtc-bframe-aac/blob/main/server/main.go)；B 帧检测通过 `github.com/Eyevinn/mp4ff/avc` 解析 NAL slice header 中的 `slice_type` 实现。

### 隐式双时间戳：一个字段携带两份信息

这是本方案的核心洞见。我们按 **DTS 顺序**发送帧（与文件存储顺序一致——解码器必须按这个顺序接收），但将 **PTS 写入 RTP 时间戳字段**。

这一改动隐式地同时携带了两个时间戳：
- **DTS 是隐式的**——包的到达顺序就是 DTS 顺序，因为我们发送前从不对帧重排。
- **PTS 是显式的**——写在 RTP 时间戳字段里，客户端通过 `getMetadata().rtpTimestamp` 读取。

接收端无需任何额外信令就能还原完整信息：按到达顺序解码（DTS 顺序），按 RTP 时间戳安排渲染（PTS）。不需要容器格式，不需要旁路信道，只用标准的 32 位 RTP 时间戳字段——用对了而已。

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

补充一点：文件循环时，需要把上一轮的总时长累加到 `pts`，保证 RTP 时间戳始终单调递增。一旦时间戳出现回退，浏览器的抖动缓冲区会丢弃大量帧。

---

## 第二部分：客户端解码 B 帧

即使服务端在 RTP 时间戳里写了正确的 PTS，浏览器内置的 H.264 解码器仍然按到达顺序渲染帧。`<video>` 元素根本不知道"这个 RTP 时间戳是 PTS，需要等它的 B 帧到齐再渲染"。我们必须完全绕开原生管道。

### EncodedInsertableStreams 在原生解码器之前拦截

`EncodedInsertableStreams` 允许我们在 RTP 层与浏览器解码器之间插入一个 `TransformStream`，把数据重定向到自己的 `VideoDecoder`。`RTCEncodedVideoFrame.getMetadata()` 暴露了 `rtpTimestamp`——正是我们在服务端写入的 PTS。将帧按 `timestamp` 排序后再写入 `MediaStreamTrackGenerator`：

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

缓冲区深度至少需要 `maxBframes + 1`。实测 5 帧效果良好，在 25 fps 下额外引入约 200 ms 延迟。解码后的 `VideoFrame` 写入挂载在 `<video>` 元素上的 `MediaStreamTrackGenerator`。完整的拦截与解码连接代码见 [`web/video-player.js`](https://github.com/shushushv/webrtc-bframe-aac/blob/main/web/video-player.js)。

---

## 第三部分：AAC 音频——Opus 偷梁换柱

### 浏览器的 Codec 封锁

WebRTC 规范只强制要求 Opus 音频。调用 `pc.addTransceiver('audio', { direction: 'recvonly' })` 时，浏览器生成的 SDP Offer 只包含 `opus/48000/2`，没有任何 API 可以替换——浏览器掌控着 Offer 内容。

在 Pion 里注册 `MP4A-LATM` 并在 Answer 中引用它，Chrome 会静默地失去音频连接。

### 解法：发送前改写 SDP 字符串

浏览器的 SDP Offer 不过是一段字符串。我们在 `createOffer()` 之后、POST 到服务端之前改写它：

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

浏览器已经创建了收发器（它以为那是 Opus）。改写后服务端看到的是 `MP4A-LATM`，于是推送 AAC。浏览器收到 AAC 编码的 RTP 包——由于我们通过 `EncodedInsertableStreams` 在原生 Opus 解码器之前拦截了数据流，可以用 `AudioDecoder` 自行解码。

![SDP 改写流程：Opus 偷梁换柱](article-images/fig2-sdp-trick.png)

### 服务端：RFC 3016 LATM 打包

服务端剥除每帧的 7 字节 ADTS 头，按 RFC 3016 LATM 格式封装（变长长度前缀 + 裸 AAC 数据）。SDP `fmtp` 行携带 `config=` 十六进制字符串，编码一个 **StreamMuxConfig**（ISO 14496-3），内含从源文件 ADTS 头动态计算的采样率、声道数和 Profile。完整实现见 [`server/audio.go`](https://github.com/shushushv/webrtc-bframe-aac/blob/main/server/audio.go)。

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
| `EncodedInsertableStreams` | 86+ | 117+ | 15.4+ |
| `VideoDecoder` | 94+ | 130+ | 16.1+ |
| `MediaStreamTrackGenerator` | 94+ | — | 17.4+ |
| `AudioDecoder`（AAC） | 94+ | 130+ | 16.1+ |
| **完整 Demo** | **94+** | **—** | **17.4+** |

Firefox 尚未实现 `MediaStreamTrackGenerator`，因此视频路径仅支持 Chrome/Safari。

---

## 局限与后续改进

- **尚不适合生产环境**：`EncodedInsertableStreams` 不保证帧从 Transform 输出时的顺序。弱网丢包或乱序时，帧可能以错误顺序到达排序缓冲区，导致花屏或解码失败。这是 WebRTC 的已知问题（[issues.webrtc.org/454162516](https://issues.webrtc.org/issues/454162516)），目前有修复补丁正在 Review（[CL 419460](https://webrtc-review.googlesource.com/c/src/+/419460)）。
- **未做音视频同步**：视频和音频管道独立启动，生产环境需通过 RTP 时间戳进行精确同步。
- **延迟**：5 帧排序缓冲在 25 fps 下约增加 200 ms。B 帧数量较少时可缩小缓冲区。
- **RTCP**：服务端丢弃了所有 RTCP 包，未处理 PLI/FIR 关键帧请求。
- **SDP 改写的脆弱性**：Opus trick 依赖浏览器以特定格式写 Opus，多年来在 Chrome 和 Safari 上较为稳定，但没有规范保证。

---

## 小结

在 WebRTC 上正确播放含 B 帧的 H.264，需要接管两件事：RTP 时间戳（用 PTS 而非 DTS）和解码管道（用 WebCodecs 而非浏览器内置解码器）。叠加 AAC 音频还需要一步 SDP 改写，但最终能得到一个完整可用的播放方案，无需任何浏览器补丁或定制构建。

源码：[github.com/shushushv/webrtc-bframe-aac](https://github.com/shushushv/webrtc-bframe-aac)

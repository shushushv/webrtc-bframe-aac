# RTCRtpTransport 调研报告

> 调研日期：2026-05-07

---

## 一、背景：Issue #227 — 从头部扩展访问说起

- **Issue**：[Accessing the data from RTP header extensions · w3c/webrtc-encoded-transform #227](https://github.com/w3c/webrtc-encoded-transform/issues/227)
- **问题**：通过 SDP 可以协商自定义 RTP 头部扩展（`a=extmap`），但 WebRTC Encoded Transform API（`RTCEncodedVideoFrame` / `RTCEncodedAudioFrame`）无法读取这些扩展里的任意数据。现有 `getMetadata()` 只暴露了少数内置扩展（`captureTime`、AV1 Dependency Descriptor 等）。
- **短期解法讨论**：在 `getMetadata()` 返回值里增加 `headerExtensions` 字段（见下文方案对比）
- **长期解法**：RTCRtpTransport — 直接在包级别读写任意头部扩展字节

### Issue #227 的实现方案对比

| 方案 | 核心思路 | 优点 | 缺点 |
|------|----------|------|------|
| 1. 扩展 `getMetadata()` | 在 `RTCEncodedVideoFrameMetadata` 加 `headerExtensions: ArrayBuffer[]` | 改动最小、向后兼容 | JS 层需自行按 RFC 解析各扩展格式 |
| 2. 按 URI 索引的结构化数据 | 已知扩展返回 parsed object，未知扩展返回 `ArrayBuffer` | 开发者体验好 | 规范需列举并维护每种扩展的解析规则 |
| 3. RTPTransport（包级别） | 直接拿到完整 RTP 包字节 | 最灵活、通用 | 独立新规范，实现复杂，短期难落地 |
| 4. `setHeaderExtensionCallback` | 在 `RTCRtpReceiver` 上按 URI 注册回调 | 按需订阅 | 破坏 Encoded Transform 的流式模型 |

**近期最可行**：方案 1（在 `getMetadata()` 里加 `headerExtensions`）  
**长期更优**：方案 3（RTPTransport），两者不互斥

---

## 二、RTCRtpTransport 规范现状

### 基本信息

| 项目 | 内容 |
|------|------|
| 规范草案 | [WebRTC RTP Transport (w3c.github.io)](https://w3c.github.io/webrtc-rtptransport/) |
| GitHub 仓库 | [w3c/webrtc-rtptransport](https://github.com/w3c/webrtc-rtptransport) |
| Explainer | [explainer.md](https://github.com/w3c/webrtc-rtptransport/blob/main/explainer.md) |
| 规范成熟度 | **Unofficial Proposal Draft**（最后更新 2025-01-21） |
| 规范说明 | "本 API 代表基于 W3C WEBRTC WG 内部进行中工作的初步提案，API 可能发生重大变化" |

### 设计定位

RTCRtpTransport **不是**原始 UDP Socket。它保留了 WebRTC 的加密（DTLS/SRTP）、拥塞控制和 DDoS 防护，同时向应用层开放包级别控制权，可以与现有 WebRTC-PC 和 Encoded Transform 共存，也可以与 WebCodecs 组合使用。

### 动机与用例

来自 [explainer.md](https://github.com/w3c/webrtc-rtptransport/blob/main/explainer.md)：

- **AR/VR 大体积 metadata**：现有 API 不支持自定义 metadata 扩展
- **AAC 等非标准 codec**：WebCodecs 支持但 WebRTC 不支持的编解码器（如 AAC）
- **自定义 packetization / FEC / RTX**：应用控制封包逻辑
- **自定义拥塞控制**：替换内置 GCC，接入自研或第三方 bandwidth estimation
- **自定义 RTCP**：LRR、RPSI、SLI 等现有 WebRTC 不支持的反馈消息
- **P2P fanout**：游戏流媒体等场景

---

## 三、当前 API 接口（规范草案 2025-01-21）

### 入口：扩展现有接口

```webidl
partial interface RTCPeerConnection {
  readonly attribute RTCRtpTransport? rtpTransport;
};
partial interface RTCRtpSender {
  attribute RTCRtpTransport? rtpTransport;
  undefined replacePacketSender();
};
partial interface RTCRtpReceiver {
  attribute RTCRtpTransport? rtpTransport;
  undefined replacePacketReceiver();
};
partial dictionary RTCConfiguration {
  boolean customPacer;  // 启用自定义 pacer
};
```

### RTCRtpTransport（顶层传输控制）

```webidl
interface RTCRtpTransport {
  readonly attribute unsigned long bandwidthEstimate;      // 浏览器估算带宽
  readonly attribute unsigned long allocatedBandwidth;     // 当前分配带宽
  attribute unsigned long customAllocatedBandwidth;        // 应用自定义分配带宽
  attribute unsigned long customMaxBandwidth;
  attribute unsigned long customPerPacketOverhead;

  sequence<RTCRtpPacket> readPacketizedRtp(unsigned long maxNumberOfPackets);
  sequence<RTCRtpSent>   readSentRtp(long maxCount);
  sequence<RTCRtpAcks>   readReceivedRtpAcks(long maxCount);

  attribute EventHandler onpacketizedrtpavailable;
  attribute EventHandler onsentrtp;
  attribute EventHandler onreceivedrtpacks;
};
```

### RTCRtpPacketSender / RTCRtpPacketReceiver

```webidl
interface RTCRtpPacketSender {
  readonly attribute DOMString? mid;
  readonly attribute DOMString? rid;
  readonly attribute unsigned long ssrc;
  readonly attribute unsigned long rtxSsrc;
  readonly attribute unsigned long allocatedBandwidth;

  sequence<RTCRtpPacket> readPacketizedRtp(unsigned long maxNumberOfPackets);
  RTCRtpSendResult sendRtp(RTCRtpPacket packet);
  RTCRtpSendResult sendRtp(RTCRtpPacketInit packetInit, optional RTCRtpSendOptions options);

  attribute EventHandler onpacketizedrtp;
};

interface RTCRtpPacketReceiver {
  readonly attribute DOMString? mid;
  readonly attribute DOMString? rid;

  sequence<unsigned long> getSsrcs();
  sequence<unsigned long> getRtxSsrcs();
  sequence<RTCRtpPacket>  readReceivedRtp(long maxNumberOfPackets);
  undefined receiveRtp(RTCRtpPacket packet);  // 注入包（自定义 jitter buffer）

  attribute EventHandler onreceivedrtp;
};
```

### RTCRtpPacket（核心数据包）

```webidl
interface RTCRtpPacket {
  constructor(RTCRtpPacketInit init);

  attribute boolean          marker;
  attribute octet            payloadType;
  attribute unsigned short   sequenceNumber;
  attribute unsigned long    timestamp;
  attribute unsigned long    ssrc;
  attribute unsigned long    paddingBytes;

  readonly attribute DOMHighResTimeStamp? receivedTime;
  readonly attribute unsigned long?       sequenceNumberRolloverCount;
  attribute AllowSharedBufferSource       payload;

  sequence<unsigned long>         getCsrcs();
  undefined                       setCsrcs(sequence<unsigned long> csrcs);

  // ← 与 Issue #227 直接相关
  sequence<RTCRtpHeaderExtension> getHeaderExtensions();
  undefined setHeaderExtensions(sequence<RTCRtpHeaderExtension> headerExtensions);
};
```

### RTCRtpHeaderExtension（任意头部扩展）

```webidl
interface RTCRtpHeaderExtension {
  constructor(RTCRtpHeaderExtensionInit init);
  readonly attribute DOMString   uri;              // 如 "urn:ietf:params:rtp-hdrext:toffset"
  readonly attribute unsigned long valueByteLength;
  undefined copyValueTo(AllowSharedBufferSource destination);  // 读取原始字节
};

dictionary RTCRtpHeaderExtensionInit {
  required DOMString               uri;
  required AllowSharedBufferSource value;  // 原始字节
};
```

### 确认与拥塞（自定义拥塞控制的核心输入）

```webidl
interface RTCRtpAcks {
  sequence<RTCRtpAck> getAcks();
  readonly attribute unsigned long long          remoteSendTimestamp;
  readonly attribute DOMHighResTimeStamp         receivedTime;
  readonly attribute RTCExplicitCongestionNotification explicitCongestionNotification;
};

interface RTCRtpAck {
  readonly attribute unsigned long long ackId;
  readonly attribute unsigned long long remoteReceiveTimestamp;
};

enum RTCExplicitCongestionNotification {
  "unset",
  "scalable-congestion-not-experienced",
  "classic-congestion-not-experienced",
  "congestion-experienced"
};

interface RTCRtpSent {
  readonly attribute DOMHighResTimeStamp time;
  readonly attribute unsigned long long? ackId;  // null = 不支持 ack
  readonly attribute unsigned long long  size;
};

enum RTCRtpUnsentReason { "overuse", "transport-unavailable" };
```

---

## 四、Chromium 实现状态

### 关键发现

| 项目 | 状态 |
|------|------|
| Chrome Platform Status | [RtpTransport WebRTC API](https://chromestatus.com/feature/5136968899100672) |
| Intent to Prototype | [blink-dev 邮件，2024-06-04，作者 Tony Herre](https://www.mail-archive.com/blink-dev@chromium.org/msg10561.html) |
| `runtime_enabled_features.json5` | 已注册，`status: "test"` |
| 实现文件（`.idl` / `.cc` / `.h`） | **不存在于公开源码树** |
| Chromium 内部 issue | [#345101934: Prototype RTPTransport implementation](https://issues.chromium.org/issues/345101934)（需登录） |
| TAG 审查 | Pending |
| Firefox / WebKit 信号 | 无 |
| Origin Trial | 无 |

### 关于 `status: "test"`

在 Chromium 的 [RuntimeEnabledFeatures](https://chromium.googlesource.com/chromium/src/+/main/third_party/blink/renderer/platform/RuntimeEnabledFeatures.md) 体系中：
- `stable` — 默认开启
- `experimental` — `--enable-experimental-web-platform-features` 可开启
- **`test`** — 只能通过 `--enable-blink-features=RTCRtpTransport` 开启

但由于 `.idl` 和实现文件尚未合入公开主干，**即使加了 flag 也无法使用**，`RTCRtpTransport` 对象不存在。实现代码大概率还在 Google 内部 branch 开发中。

### 时间线预估

| 阶段 | 预计时间 |
|------|----------|
| 内部 prototype 完成 | 不明，issue 受限 |
| 代码合入公开 Chromium 主干 | 2025 年底？ |
| Origin Trial（可公开测试） | 乐观估计 2026 年 |
| 正式 Ship | 未知 |

---

## 五、W3C 工作组活动

| 时间 | 事项 |
|------|------|
| 2024-06-04 | Chrome Intent to Prototype 提交 |
| 2024-09-25 | [TPAC 2024 RtpTransport Breakout Session](https://www.w3.org/2024/Talks/TPAC/breakouts/rtp-transport.pdf)，讨论自定义拥塞控制、WebCodecs 集成 |
| 2024-09-26 | [Joint Media/WebRTC WG meeting at TPAC](https://www.w3.org/2024/09/26-webrtc-minutes.html) |
| 2025-01-21 | 规范草案最新更新 |
| 2025-05 | webrtc-rtptransport 仓库**无新活动**（来自 [W3C 周报 2025-05-20](https://lists.w3.org/Archives/Public/public-webrtc/2025May/0017.html)） |

### 相关 GitHub Issues

- [w3c/webrtc-rtptransport #12: Arbitrary RTP Header Extensions](https://github.com/w3c/webrtc-rtptransport/issues/12) — 与 Issue #227 直接相关，讨论是否允许读写任意头部扩展字节
- [w3c/webrtc-rtptransport #14: Update examples to use standard APIs](https://github.com/w3c/webrtc-rtptransport/issues/14)
- [mozilla/standards-positions #713: WebRTC RTP header extension control](https://github.com/mozilla/standards-positions/issues/713) — Mozilla **Positive**（支持）

---

## 六、Firefox / Mozilla 立场

Mozilla 对 **WebRTC RTP header extension control**（`RTCRtpTransceiver.setOfferedRtpHeaderExtensions()`）持 **Positive** 立场，理由是"有助于减少 SDP munging"。但对 RTCRtpTransport 本身尚无官方表态。

参考：[WebRTC Extensions Spec](https://w3c.github.io/webrtc-extensions/)

---

## 七、与本项目（webrtc-bframe-aac）的关系

本项目当前使用：
- **服务端**：Pion `TrackLocalStaticRTP.WriteRTP()` 直接写 RTP 包
- **客户端**：`createEncodedStreams()` 拦截 encoded frames + `MediaStreamTrackProcessor` 拉取 decoded frames 做 B-frame 排序

如果 RTCRtpTransport 落地，客户端可以改为：

```js
// 未来的写法（RTCRtpTransport 可用后）
const receiver = pc.getReceivers().find(r => r.track.kind === 'video');
const transport = receiver.rtpTransport;

transport.onreceivedrtp = () => {
  const packets = transport.readReceivedRtp(32);
  for (const pkt of packets) {
    // 直接拿到 RTP 包，读取任意 header extension
    const exts = pkt.getHeaderExtensions();
    const pts = getPTSFromExtension(exts);
    // 自己做 B-frame reorder buffer
    reorderBuffer.push({ payload: pkt.payload, pts });
  }
};
```

当前最实际的路线：**继续用 Encoded Transform 实现，等 RTCRtpTransport 可用后迁移**。

---

## 八、参考链接汇总

### 规范 & 标准

- [WebRTC RTP Transport 草案](https://w3c.github.io/webrtc-rtptransport/)
- [w3c/webrtc-rtptransport GitHub](https://github.com/w3c/webrtc-rtptransport)
- [WebRTC Encoded Transform 规范 (W3C TR)](https://www.w3.org/TR/webrtc-encoded-transform/)
- [WebRTC Extensions 规范](https://w3c.github.io/webrtc-extensions/)
- [WebRTC 1.0 规范](https://w3c.github.io/webrtc-pc/)

### Issues & 讨论

- [Issue #227: Accessing the data from RTP header extensions](https://github.com/w3c/webrtc-encoded-transform/issues/227)
- [Issue #12: Arbitrary RTP Header Extensions (rtptransport)](https://github.com/w3c/webrtc-rtptransport/issues/12)
- [mozilla/standards-positions #713](https://github.com/mozilla/standards-positions/issues/713)

### Chromium 实现

- [Chrome Platform Status: RtpTransport WebRTC API](https://chromestatus.com/feature/5136968899100672)
- [Intent to Prototype: RTPTransport WebRTC API (blink-dev, 2024-06-04)](https://www.mail-archive.com/blink-dev@chromium.org/msg10561.html)
- [Chromium Issue #345101934: Prototype RTPTransport implementation](https://issues.chromium.org/issues/345101934)
- [runtime_enabled_features.json5 (Chromium source)](https://chromium.googlesource.com/chromium/src/+/main/third_party/blink/renderer/platform/runtime_enabled_features.json5)

### W3C 会议记录

- [TPAC 2024 RtpTransport Breakout Session PDF](https://www.w3.org/2024/Talks/TPAC/breakouts/rtp-transport.pdf)
- [Joint Media/WebRTC WG @ TPAC 2024-09-26 minutes](https://www.w3.org/2024/09/26-webrtc-minutes.html)
- [W3C public-webrtc 周报 2025-05-20](https://lists.w3.org/Archives/Public/public-webrtc/2025May/0017.html)

### MDN & 开发者文档

- [RTCEncodedVideoFrame: getMetadata() — MDN](https://developer.mozilla.org/en-US/docs/Web/API/RTCEncodedVideoFrame/getMetadata)
- [Using WebRTC Encoded Transforms — MDN](https://developer.mozilla.org/en-US/docs/Web/API/WebRTC_API/Using_Encoded_Transforms)

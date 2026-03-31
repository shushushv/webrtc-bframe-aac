// Log verbosity from ?v= URL param (mirrors server -v flag: 0=info, 1=debug, 2=verbose).
const V = parseInt(new URLSearchParams(location.search).get('v') ?? '0', 10);
const dbg = (level, ...args) => { if (V >= level) console.log(...args); };

/**
 * VideoPlayer — B-frame reordering using the browser's native H.264 decoder
 * via MediaStreamTrackProcessor / MediaStreamTrackGenerator.
 *
 * Decoded VideoFrames are pulled from the native WebRTC pipeline and reordered
 * by the RTP timestamp (= PTS set by the server) exposed via frame.metadata().
 *
 * Browser requirements:
 *   - MediaStreamTrackProcessor  (Chrome 94+)
 *   - MediaStreamTrackGenerator  (Chrome 94+)
 *   - VideoFrame.metadata()      (Chrome 106+, metadata.rtpTimestamp: Chrome 110+)
 */
class VideoPlayer {
  /**
   * @param {object} [options]
   * @param {number} [options.bufferSize=5]  Frames to buffer before rendering.
   */
  constructor(options = {}) {
    this.bufferSize = options.bufferSize || 5;
    this._writer = null;
    this._frameBuffer = [];
    this._destroyed = false;
  }

  /**
   * Creates a <video> element backed by a MediaStreamTrackGenerator.
   * Must be called before attachTrack().
   */
  init(container = document.body) {
    const video = document.createElement('video');
    video.autoplay = true;
    video.muted = true;
    video.controls = true;
    video.playsInline = true;
    video.style.width = '640px';
    container.appendChild(video);

    const generator = new MediaStreamTrackGenerator({ kind: 'video' });
    video.srcObject = new MediaStream([generator]);
    this._writer = generator.writable.getWriter();

    video.play();
    dbg(1, `[video] initialized — bufferSize=${this.bufferSize}`);
  }

  /**
   * Attaches a live video MediaStreamTrack from the RTCPeerConnection.
   * Starts pulling decoded VideoFrames and feeding them through the reorder buffer.
   *
   * @param {MediaStreamTrack} track  kind === 'video'
   */
  attachTrack(track) {
    dbg(1, '[video] attaching track, starting read loop');
    const processor = new MediaStreamTrackProcessor({ track });
    this._readLoop(processor.readable.getReader());
  }

  destroy() {
    this._destroyed = true;
    for (const { frame } of this._frameBuffer) frame.close();
    this._frameBuffer = [];
    this._writer?.close().catch(() => {});
    this._writer = null;
  }

  // ── private ──────────────────────────────────────────────────────────────

  async _readLoop(reader) {
    while (!this._destroyed) {
      let result;
      try {
        result = await reader.read();
      } catch (e) {
        if (!this._destroyed) console.warn('[video] reader error:', e);
        break;
      }
      const { value: frame, done } = result;
      if (done) break;
      this._onFrame(frame);
    }
    // Flush any buffered frames on loop exit.
    for (const { frame } of this._frameBuffer) frame.close();
    this._frameBuffer = [];
  }

  _onFrame(frame) {
    const meta = frame.metadata();
    // rtpTimestamp is the raw RTP timestamp set by the server (= PTS, 90 kHz).
    // If the browser doesn't expose it yet, fall back to frame.timestamp (µs).
    const rtpTs = meta?.rtpTimestamp;
    const sortKey = rtpTs ?? frame.timestamp;

    dbg(2, `[video] rtpTs=${rtpTs} frameTs_ms=${(frame.timestamp / 1000).toFixed(1)} sortKey=${sortKey}`);

    this._frameBuffer.push({ frame, sortKey });
    this._frameBuffer.sort((a, b) => a.sortKey - b.sortKey);

    if (this._frameBuffer.length >= this.bufferSize) {
      const oldest = this._frameBuffer.shift();
      dbg(2, `[video][render] sortKey=${oldest.sortKey}`);
      this._writer.write(oldest.frame)
        .then(() => oldest.frame.close())
        .catch(err => { console.warn('[video] write error:', err); oldest.frame.close(); });
    }
  }
}

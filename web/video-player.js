// Log verbosity from ?v= URL param (mirrors server -v flag: 0=info, 1=debug, 2=verbose).
const V = parseInt(new URLSearchParams(location.search).get('v') ?? '0', 10);
const dbg = (level, ...args) => { if (V >= level) console.log(...args); };

/**
 * WebCodecsVideoPlayer — decodes H.264 with B-frames via WebCodecs + MediaStreamTrackGenerator.
 *
 * The server sets the RTP timestamp to PTS (not DTS), so we read
 * encodedFrame.getMetadata().rtpTimestamp as the display timestamp.
 * A small frame buffer (default 5 frames) is sorted by PTS before rendering,
 * which corrects the decode-order vs display-order discrepancy introduced by B-frames.
 *
 * Browser requirements:
 *   - EncodedInsertableStreams (Chrome 86+, Firefox 117+, Safari 15.4+)
 *   - WebCodecs VideoDecoder     (Chrome 94+, Safari 16+)
 *   - MediaStreamTrackGenerator  (Chrome 94+, Safari 17.4+)
 */
class WebCodecsVideoPlayer {
  /**
   * @param {object} [options]
   * @param {number} [options.frameRate=25]
   * @param {number} [options.bufferSize=5]   Number of frames to buffer before rendering.
   * @param {string} [options.codec='avc1.42002a']  WebCodecs codec string.
   */
  constructor(options = {}) {
    this.frameRate = options.frameRate || 25;
    this.bufferSize = options.bufferSize || 5;
    this.codec = options.codec || 'avc1.42002a';
    this.frameDurationUs = (1 / this.frameRate) * 1e6; // microseconds

    this._decoder = null;
    this._writer = null;
    this._frameBuffer = [];
  }

  /**
   * Creates a <video> element, attaches a MediaStreamTrackGenerator and
   * initialises the VideoDecoder.  Must be called before decodeFrame().
   */
  init(container = document.body) {
    const video = document.createElement('video');
    video.autoplay = true;
    video.muted = true;
    video.controls = true;
    video.playsInline = true;
    video.style.width = '640px';
    container.appendChild(video);

    const stream = new MediaStream();
    video.srcObject = stream;

    const generator = new MediaStreamTrackGenerator({ kind: 'video' });
    stream.addTrack(generator);
    this._writer = generator.writable.getWriter();

    this._decoder = new VideoDecoder({
      output: (frame) => this._onFrame(frame),
      error: (e) => console.error('VideoDecoder error:', e),
    });
    this._decoder.configure({
      codec: this.codec,
      hardwareAcceleration: 'prefer-hardware',
    });
    dbg(1, `[video] VideoDecoder configured — codec=${this.codec} bufferSize=${this.bufferSize}`);

    video.play();
  }

  /**
   * Decodes one encoded WebRTC frame.
   * Call this from an EncodedInsertableStreams TransformStream.
   *
   * @param {RTCEncodedVideoFrame} encodedFrame
   */
  decodeFrame(encodedFrame) {
    // rtpTimestamp is the raw RTP timestamp set by the server (= PTS in 90 kHz units).
    const rtpTs = encodedFrame.getMetadata().rtpTimestamp;
    // WebCodecs expects microseconds.
    const timestampUs = (rtpTs / 90000) * 1e6;

    dbg(2, `[recv] type=${encodedFrame.type} rtpTs=${rtpTs} pts_ms=${(timestampUs / 1000).toFixed(1)} byteLength=${encodedFrame.data.byteLength}`);

    const chunk = new EncodedVideoChunk({
      type: encodedFrame.type === 'key' ? 'key' : 'delta',
      timestamp: timestampUs,
      duration: this.frameDurationUs,
      data: encodedFrame.data,
    });
    this._decoder.decode(chunk);
  }

  destroy() {
    for (const frame of this._frameBuffer) frame.close();
    this._frameBuffer = [];
    if (this._decoder && this._decoder.state !== 'closed') this._decoder.close();
    this._writer?.close().catch(() => {});
    this._decoder = null;
    this._writer = null;
  }

  // ── private ──────────────────────────────────────────────────────────────

  _onFrame(frame) {
    this._frameBuffer.push(frame);

    // Sort by PTS so B-frames are emitted before the P-frame they precede.
    this._frameBuffer.sort((a, b) => a.timestamp - b.timestamp);

    if (this._frameBuffer.length >= this.bufferSize) {
      const oldest = this._frameBuffer.shift();
      dbg(2, `[render] type=${oldest.type} pts_ms=${(oldest.timestamp / 1000).toFixed(1)}`);
      this._writer.write(oldest)
        .then(() => oldest.close())
        .catch((err) => {
          console.warn('frame write error:', err);
          oldest.close();
        });
    }
  }
}

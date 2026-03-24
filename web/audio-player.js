/**
 * WebCodecsAudioPlayer — decodes AAC audio via WebCodecs + AudioContext.
 *
 * The server sends AAC packaged as RFC 3016 (MP4A-LATM, cpresent=0).
 * The SDP "Opus trick" makes the browser create an audio transceiver,
 * but the actual payload is AAC — we intercept it with EncodedInsertableStreams
 * and decode it ourselves with AudioDecoder instead of the browser's Opus decoder.
 */
class WebCodecsAudioPlayer {
  constructor() {
    this.sampleRate = null;
    this.channels = null;
    this.audioCtx = null;
    this.decoder = null;
    this.nextPlayTime = 0;
  }

  /**
   * Initialise AudioDecoder and AudioContext for the given sample rate.
   * Throws if the config is not supported — caller should retry with another rate.
   *
   * @param {number} sampleRate  e.g. 44100 or 48000
   * @param {number} channels    e.g. 2
   */
  async init(sampleRate, channels) {
    const config = { codec: 'mp4a.40.2', sampleRate, numberOfChannels: channels };

    const { supported } = await AudioDecoder.isConfigSupported(config);
    if (!supported) {
      throw new Error(`AudioDecoder does not support ${JSON.stringify(config)}`);
    }

    this.sampleRate = sampleRate;
    this.channels = channels;
    this.audioCtx = new AudioContext({ sampleRate });

    this.decoder = new AudioDecoder({
      output: (data) => this._onAudioData(data),
      error: (e) => console.error('[audio] decode error:', e),
    });
    this.decoder.configure(config);

    dbg(1, `[audio] AudioDecoder ready — sampleRate=${sampleRate} channels=${channels}`);
  }

  /**
   * Decode one encoded audio frame from EncodedInsertableStreams.
   * Strips the RFC 3016 LATM length prefix to obtain the raw AAC payload.
   *
   * @param {RTCEncodedAudioFrame} encodedFrame
   */
  decodeFrame(encodedFrame) {
    const aacData = parseLATM(encodedFrame.data);
    if (!aacData || aacData.byteLength === 0) return;

    const rtpTs = encodedFrame.getMetadata().rtpTimestamp;
    const timestampUs = (rtpTs / this.sampleRate) * 1e6;

    dbg(2, `[audio recv] rtpTs=${rtpTs} pts_ms=${(timestampUs / 1000).toFixed(1)} byteLen=${aacData.byteLength}`);

    this.decoder.decode(new EncodedAudioChunk({
      type: 'key', // AAC frames are all independently decodable
      timestamp: timestampUs,
      data: aacData,
    }));
  }

  // ── private ───────────────────────────────────────────────────────────────

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

    // Schedule frames for gapless playback
    const now = this.audioCtx.currentTime;
    if (this.nextPlayTime < now) this.nextPlayTime = now + 0.1; // small jitter buffer
    source.start(this.nextPlayTime);
    this.nextPlayTime += buffer.duration;
  }
}

/**
 * Parse RFC 3016 LATM payload (cpresent=0) and return the raw AAC payload.
 *
 * PayloadLengthInfo uses a variable-length byte scheme:
 *   - Read bytes, accumulating their values, until a byte < 0xFF is read.
 *   - e.g. [0x64]       → length 100
 *   - e.g. [0xFF, 0x00] → length 255
 *   - e.g. [0xFF, 0x2D] → length 300
 * The remaining bytes are the AAC payload (PayloadMux).
 *
 * @param {ArrayBuffer} buffer
 * @returns {ArrayBuffer|null}
 */
function parseLATM(buffer) {
  if (buffer.byteLength < 1) return null;
  const view = new DataView(buffer);
  let offset = 0;
  let payloadLength = 0;
  let byte;
  do {
    if (offset >= buffer.byteLength) return null;
    byte = view.getUint8(offset++);
    payloadLength += byte;
  } while (byte === 0xFF);
  return buffer.slice(offset, offset + payloadLength);
}

/**
 * Rewrite the audio section of an SDP offer, replacing Opus with
 * MP4A-LATM at the requested sample rate.
 *
 * This is the "Opus trick": the browser can only create Opus transceivers,
 * but we rename the codec in the outgoing SDP so the server negotiates AAC.
 *
 * @param {string} sdp
 * @param {number} sampleRate
 * @returns {string}
 */
function rewriteSDPAudioToAAC(sdp, sampleRate) {
  // Find the Opus payload type from the rtpmap line.
  const match = sdp.match(/a=rtpmap:(\d+) opus\/48000\/2/i);
  if (!match) return sdp; // no Opus found — return unchanged

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

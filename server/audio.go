package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const aacSamplesPerFrame = 1024

var adtsSampleRateTable = []int{
	96000, 88200, 64000, 48000, 44100, 32000,
	24000, 22050, 16000, 12000, 11025, 8000, 7350,
}

type aacFrame struct {
	data []byte
}

type aacConfig struct {
	profile    int // 1=Main, 2=LC, 3=SSR, 4=LTP
	sampleRate int
	channels   int
}

// clockRate returns the RTP clock rate (equals the sample rate for audio).
func (c aacConfig) clockRate() uint32 { return uint32(c.sampleRate) }

// sdpFmtpLine returns the fmtp attribute value for audio/MP4A-LATM.
func (c aacConfig) sdpFmtpLine() string {
	return fmt.Sprintf(
		"profile-level-id=1;object=%d;cpresent=0;config=%s",
		c.profile,
		hex.EncodeToString(c.streamMuxConfig()),
	)
}

// streamMuxConfig encodes the ISO 14496-3 StreamMuxConfig (6 bytes) for the SDP fmtp.
//
// Bit layout (44 bits, zero-padded to 48):
//
//	1  audioMuxVersion        = 0
//	1  allStreamsSameTimeFraming = 1
//	6  numSubFrames            = 0  (1 subframe)
//	4  numProgram              = 0  (1 program)
//	3  numLayer                = 0  (1 layer)
//	16 AudioSpecificConfig
//	3  frameLengthType         = 0  (variable frame length)
//	8  latmBufferFullness      = 0xFF
//	1  otherDataPresent        = 0
//	1  crcCheckPresent         = 0
func (c aacConfig) streamMuxConfig() []byte {
	srIdx := 4 // default: 44100
	for i, sr := range adtsSampleRateTable {
		if sr == c.sampleRate {
			srIdx = i
			break
		}
	}

	var bits uint64
	bits = (bits << 1) | 0                   // audioMuxVersion = 0
	bits = (bits << 1) | 1                   // allStreamsSameTimeFraming = 1
	bits = (bits << 6) | 0                   // numSubFrames = 0
	bits = (bits << 4) | 0                   // numProgram = 0
	bits = (bits << 3) | 0                   // numLayer = 0
	bits = (bits << 5) | uint64(c.profile)   // audioObjectType (5 bits)
	bits = (bits << 4) | uint64(srIdx)       // samplingFrequencyIndex (4 bits)
	bits = (bits << 4) | uint64(c.channels)  // channelConfiguration (4 bits)
	bits = (bits << 1) | 0                   // frameLengthFlag = 0
	bits = (bits << 1) | 0                   // dependsOnCoreCoder = 0
	bits = (bits << 1) | 0                   // extensionFlag = 0
	bits = (bits << 3) | 0                   // frameLengthType = 0
	bits = (bits << 8) | 0xFF                // latmBufferFullness = 0xFF
	bits = (bits << 1) | 0                   // otherDataPresent = 0
	bits = (bits << 1) | 0                   // crcCheckPresent = 0
	bits = bits << 4                         // zero-pad to 48 bits

	out := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		out[i] = byte(bits & 0xFF)
		bits >>= 8
	}
	return out
}

// packLATM wraps a raw AAC payload in RFC 3016 LATM format (cpresent=0).
//
// Layout: PayloadLengthInfo | PayloadMux
//
// PayloadLengthInfo encodes the payload byte-length using a variable-length
// scheme: emit 0xFF bytes while remaining >= 255, then emit the remainder.
//
//	length 100  → [0x64]
//	length 255  → [0xFF, 0x00]
//	length 300  → [0xFF, 0x2D]
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

// readADTSFile parses an ADTS (.aac) file and returns all frames plus stream config.
func readADTSFile(filename string) ([]aacFrame, aacConfig, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, aacConfig{}, err
	}
	defer f.Close()

	var frames []aacFrame
	var cfg aacConfig
	first := true

	for {
		// Read the fixed 7-byte ADTS header.
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(f, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, aacConfig{}, fmt.Errorf("adts: read header: %w", err)
		}

		// Validate sync word (12 set bits).
		if hdr[0] != 0xFF || (hdr[1]&0xF0) != 0xF0 {
			return nil, aacConfig{}, fmt.Errorf("adts: invalid sync at frame %d", len(frames))
		}

		protectionAbsent := hdr[1] & 0x01
		profile := int((hdr[2]>>6)&0x03) + 1
		srIdx := int((hdr[2] >> 2) & 0x0F)
		channels := int(((hdr[2] & 0x01) << 2) | ((hdr[3] >> 6) & 0x03))
		frameLen := (int(hdr[3]&0x03) << 11) | (int(hdr[4]) << 3) | int(hdr[5]>>5)

		if first {
			if srIdx >= len(adtsSampleRateTable) {
				return nil, aacConfig{}, fmt.Errorf("adts: unknown sample rate index %d", srIdx)
			}
			cfg = aacConfig{profile: profile, sampleRate: adtsSampleRateTable[srIdx], channels: channels}
			first = false
		}

		headerLen := 7
		if protectionAbsent == 0 {
			// Skip 2-byte CRC field.
			if _, err := io.ReadFull(f, make([]byte, 2)); err != nil {
				break
			}
			headerLen = 9
		}

		payloadLen := frameLen - headerLen
		if payloadLen <= 0 {
			continue
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(f, payload); err != nil {
			break
		}
		frames = append(frames, aacFrame{data: payload})
	}

	if len(frames) == 0 {
		return nil, aacConfig{}, fmt.Errorf("adts: no frames found in %s", filename)
	}
	return frames, cfg, nil
}

// streamAudio sends AAC frames packaged as RFC 3016 MP4A-LATM over RTP
// at the natural frame rate until the connection is closed.
func streamAudio(iceConnected, connClosed context.Context, track *webrtc.TrackLocalStaticRTP) {
	<-iceConnected.Done()
	logf(logDebug, "[audio] loaded %d frames, starting stream\n", len(audioFrames))

	frameDuration := time.Duration(aacSamplesPerFrame) * time.Second / time.Duration(audioCfg.sampleRate)
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	var seqNum uint16
	var pts uint32
	frameIndex := 0

	for {
		select {
		case <-connClosed.Done():
			return
		case <-ticker.C:
			frame := audioFrames[frameIndex]

			logf(logVerbose, "[audio] #%-4d pts=%d pts_ms=%.1f byteLen=%d\n",
				frameIndex, pts,
				float64(pts)/float64(audioCfg.sampleRate)*1000,
				len(frame.data))

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Marker:         true,
					PayloadType:    97,
					SequenceNumber: seqNum,
					Timestamp:      pts,
					SSRC:           0x87654321,
				},
				Payload: packLATM(frame.data),
			}
			seqNum++
			pts += aacSamplesPerFrame

			if err := track.WriteRTP(pkt); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				logf(logDebug, "[audio] WriteRTP error: %v\n", err)
			}

			frameIndex++
			if frameIndex >= len(audioFrames) {
				frameIndex = 0
				logf(logDebug, "[audio] loop\n")
			}
		}
	}
}

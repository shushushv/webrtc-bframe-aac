package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

const (
	videoClockRate     = 90000
	videoFrameRate     = 25
	frameDurationTicks = videoClockRate / videoFrameRate // 3600 ticks per frame
	videoTimePerFrame  = time.Second / videoFrameRate
)

// FrameInfo holds a single NAL unit along with its computed PTS.
type FrameInfo struct {
	nal      *h264reader.NAL
	isBFrame bool
	isIDR    bool
	pts      uint32 // presentation timestamp in 90 kHz clock ticks
}

// streamVideo reads the H.264 file, computes PTS for each NAL unit, and streams
// them over RTP at the natural frame rate until the connection is closed.
func streamVideo(iceConnected, connClosed context.Context, track *webrtc.TrackLocalStaticRTP) {
	<-iceConnected.Done()

	frames := readAllNALs()
	if len(frames) == 0 {
		logf(logInfo, "no frames found in %s\n", videoFileName)
		return
	}
	var iCount, pCount, bCount int
	for _, f := range frames {
		if f.isIDR {
			iCount++
		} else if f.isBFrame {
			bCount++
		} else if isPicture(f.nal.UnitType) {
			pCount++
		}
	}
	logf(logDebug, "[video] loaded %d NAL units (I=%d P=%d B=%d), starting stream\n",
		len(frames), iCount, pCount, bCount)

	// Compute the total PTS span of one loop so timestamps stay monotonic.
	loopDuration := uint32(0)
	for _, f := range frames {
		if f.pts > loopDuration {
			loopDuration = f.pts
		}
	}
	loopDuration += frameDurationTicks

	payloader := &codecs.H264Payloader{}
	var seqNum uint16
	var loopOffset uint32
	frameIndex := 0
	ticker := time.NewTicker(videoTimePerFrame)
	defer ticker.Stop()

	for {
		select {
		case <-connClosed.Done():
			return
		case <-ticker.C:
			frame := frames[frameIndex]
			pts := loopOffset + frame.pts

			logf(logVerbose, "[video] #%-4d type=%-2s pts=%d pts_ms=%.1f byteLen=%d\n",
				frameIndex, frameType(frame), pts,
				float64(pts)/float64(videoClockRate)*1000,
				len(frame.nal.Data))

			payloads := payloader.Payload(1200, frame.nal.Data)
			for i, payload := range payloads {
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						Marker:         i == len(payloads)-1,
						PayloadType:    96,
						SequenceNumber: seqNum,
						Timestamp:      pts,
						SSRC:           0x12345678,
					},
					Payload: payload,
				}
				seqNum++
				if err := track.WriteRTP(pkt); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					logf(logDebug, "[video] WriteRTP error: %v\n", err)
				}
			}

			frameIndex++
			if frameIndex >= len(frames) {
				frameIndex = 0
				loopOffset += loopDuration
				logf(logDebug, "[video] loop #%d\n", loopOffset/loopDuration)
			}
		}
	}
}

// readAllNALs parses the H.264 file and returns NAL units with PTS assigned.
func readAllNALs() []FrameInfo {
	file, err := os.Open(videoFileName)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	h264, err := h264reader.NewReader(file)
	if err != nil {
		panic(err)
	}

	var frames []FrameInfo
	for {
		nal, err := h264.NextNAL()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}
		frames = append(frames, FrameInfo{
			nal:      nal,
			isIDR:    nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr,
			isBFrame: isBFrame(nal.Data),
		})
	}
	return assignPTS(frames)
}

// assignPTS computes the PTS (in 90 kHz ticks) for every frame in the slice.
//
// H.264 files store frames in DTS order.  For a -bf 2 stream the pattern is:
//
//	DTS order:  I  P1  B1  B2  P2  B3  B4  P3 ...
//	PTS order:  I  B1  B2  P1  B3  B4  P2  ...
//
// Formula for a group [P, B₁…Bₙ] (P appears first in file, then its B-frames):
//
//	P.pts  = lastAnchorPTS + (n+1) × frameDurationTicks
//	Bᵢ.pts = lastAnchorPTS + i    × frameDurationTicks   (i = 1…n)
func assignPTS(frames []FrameInfo) []FrameInfo {
	firstIDR := -1
	for i, f := range frames {
		if f.isIDR {
			firstIDR = i
			break
		}
	}
	if firstIDR == -1 {
		return frames
	}

	for i := 0; i <= firstIDR; i++ {
		frames[i].pts = 0
	}

	lastAnchorPTS := uint32(0)
	cur := firstIDR + 1

	for cur < len(frames) {
		if !isPicture(frames[cur].nal.UnitType) {
			frames[cur].pts = lastAnchorPTS
			cur++
			continue
		}

		nextAnchor := -1
		for i := cur + 1; i < len(frames); i++ {
			if isPicture(frames[i].nal.UnitType) && !frames[i].isBFrame {
				nextAnchor = i
				break
			}
		}

		if nextAnchor == -1 {
			for i := cur; i < len(frames); i++ {
				lastAnchorPTS += frameDurationTicks
				frames[i].pts = lastAnchorPTS
			}
			break
		}

		numB := 0
		for i := cur + 1; i < nextAnchor; i++ {
			if isPicture(frames[i].nal.UnitType) {
				numB++
			}
		}

		anchorPTS := lastAnchorPTS + uint32(numB+1)*frameDurationTicks
		frames[cur].pts = anchorPTS

		bIdx := 0
		for i := cur + 1; i < nextAnchor; i++ {
			if isPicture(frames[i].nal.UnitType) {
				bIdx++
				frames[i].pts = lastAnchorPTS + uint32(bIdx)*frameDurationTicks
			} else {
				frames[i].pts = lastAnchorPTS
			}
		}

		lastAnchorPTS = anchorPTS
		cur = nextAnchor
	}

	return frames
}

func isPicture(t h264reader.NalUnitType) bool {
	return t == h264reader.NalUnitTypeCodedSliceNonIdr || t == h264reader.NalUnitTypeCodedSliceIdr
}

func isBFrame(nalData []byte) bool {
	if len(nalData) == 0 {
		return false
	}
	nalType := nalData[0] & 0x1F
	if nalType != 1 && nalType != 5 {
		return false
	}
	sliceType, err := avc.GetSliceTypeFromNALU(nalData)
	if err != nil {
		return false
	}
	return sliceType == avc.SLICE_B
}

func frameType(f FrameInfo) string {
	if !isPicture(f.nal.UnitType) {
		return f.nal.UnitType.String()
	}
	if f.isIDR {
		return "I"
	}
	if f.isBFrame {
		return "B"
	}
	return "P"
}

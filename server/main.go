// WHEP server that streams H.264 (with B-frame PTS) and AAC audio over WebRTC.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// Log levels (mirrors Chrome's -v flag convention: higher = more verbose).
//
//	0 = INFO    : startup, new connection, disconnection
//	1 = DEBUG   : ICE/connection state, stream lifecycle, errors
//	2 = VERBOSE : per-frame pts / type / size
const (
	logInfo    = 0
	logDebug   = 1
	logVerbose = 2
)

var (
	videoFileName string
	audioFileName string
	logLevel      int

	rtcAPI      *webrtc.API
	audioFrames []aacFrame
	audioCfg    aacConfig
	hasAudio    bool
)

func logf(level int, format string, args ...any) {
	if logLevel >= level {
		fmt.Printf(format, args...)
	}
}

func main() {
	flag.StringVar(&videoFileName, "video", "sample/video.h264", "H.264 file to stream")
	flag.StringVar(&audioFileName, "audio", "sample/audio.aac", "AAC (ADTS) file to stream (optional)")
	flag.IntVar(&logLevel, "v", 0, "log verbosity: 0=info, 1=debug, 2=verbose")
	flag.Parse()

	if _, err := os.Stat(videoFileName); os.IsNotExist(err) {
		panic("video file not found: " + videoFileName)
	}

	if _, err := os.Stat(audioFileName); audioFileName != "" && !os.IsNotExist(err) {
		var err error
		audioFrames, audioCfg, err = readADTSFile(audioFileName)
		if err != nil {
			panic("failed to read audio file: " + err.Error())
		}
		logf(logInfo, "Loaded %d AAC frames from %s (profile=%d sampleRate=%d channels=%d)\n",
			len(audioFrames), audioFileName, audioCfg.profile, audioCfg.sampleRate, audioCfg.channels)
		hasAudio = true
	}

	rtcAPI = buildAPI()

	http.Handle("/", http.FileServer(http.Dir("web")))
	http.HandleFunc("/whep", whepHandler)

	if logLevel > 0 {
		fmt.Printf("Open http://localhost:8080?v=%d to access the player\n", logLevel)
	} else {
		fmt.Println("Open http://localhost:8080 to access the player")
	}
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

func buildAPI() *webrtc.API {
	m := &webrtc.MediaEngine{}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	if hasAudio {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    "audio/MP4A-LATM",
				ClockRate:   audioCfg.clockRate(),
				Channels:    uint16(audioCfg.channels),
				SDPFmtpLine: audioCfg.sdpFmtpLine(),
			},
			PayloadType: 97,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			panic(err)
		}
	}

	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	logf(logDebug, "Registered codecs: H264 PT=96%s\n", func() string {
		if hasAudio {
			return fmt.Sprintf(", MP4A-LATM PT=97 clockRate=%d channels=%d", audioCfg.clockRate(), audioCfg.channels)
		}
		return ""
	}())

	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))
}

func whepHandler(w http.ResponseWriter, r *http.Request) {
	logf(logInfo, "New connection from %s\n", r.RemoteAddr)

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pc, err := rtcAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion",
	)
	if err != nil {
		panic(err)
	}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		panic(err)
	}
	go drainRTCP(videoSender)

	var audioTrack *webrtc.TrackLocalStaticRTP
	if hasAudio {
		audioTrack, err = webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{
				MimeType:  "audio/MP4A-LATM",
				ClockRate: audioCfg.clockRate(),
				Channels:  uint16(audioCfg.channels),
			}, "audio", "pion",
		)
		if err != nil {
			panic(err)
		}
		audioSender, err := pc.AddTrack(audioTrack)
		if err != nil {
			panic(err)
		}
		go drainRTCP(audioSender)
	}

	iceConnected, iceConnectedCancel := context.WithCancel(context.Background())
	connClosed, connClosedCancel := context.WithCancel(context.Background())

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		logf(logDebug, "ICE state: %s\n", s)
		if s == webrtc.ICEConnectionStateConnected {
			iceConnectedCancel()
		}
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			logf(logInfo, "Connected (%s)\n", r.RemoteAddr)
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			logf(logInfo, "Disconnected (%s): %s\n", r.RemoteAddr, s)
			connClosedCancel()
			_ = pc.Close()
		default:
			logf(logDebug, "Peer connection state: %s\n", s)
		}
	})

	go streamVideo(iceConnected, connClosed, videoTrack)
	if hasAudio {
		go streamAudio(iceConnected, connClosed, audioTrack)
	}

	if err = pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: string(offer),
	}); err != nil {
		panic(err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		panic(err)
	}
	<-gatherDone

	logf(logDebug, "SDP answer sent to %s\n", r.RemoteAddr)
	w.Header().Set("Location", "/whep")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprint(w, pc.LocalDescription().SDP)
}

func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

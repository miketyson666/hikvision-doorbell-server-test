package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/acardace/hikvision-doorbell-server/internal/audio"
	"github.com/acardace/hikvision-doorbell-server/internal/hikvision"
	"github.com/acardace/hikvision-doorbell-server/internal/logger"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type WebRTCHandler struct {
	hikClient      *hikvision.Client
	peerConnection *webrtc.PeerConnection
	audioWriter    *hikvision.AudioStreamWriter
	audioReader    *hikvision.AudioStreamReader
	activeSession  *hikvision.AudioSession
	mu             sync.Mutex
}

func NewWebRTCHandler(hikClient *hikvision.Client) *WebRTCHandler {
	return &WebRTCHandler{
		hikClient: hikClient,
	}
}

// HandleOffer handles WebRTC SDP offer from client
func (h *WebRTCHandler) HandleOffer(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Parse SDP offer
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		logger.Log.Error("failed to decode SDP offer",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Invalid offer", http.StatusBadRequest)
		return
	}

	logger.Log.Info("received SDP offer",
		slog.String("component", "webrtc"),
		slog.String("type", offer.Type.String()))

	// Create WebRTC configuration for local network only
	// No ICE servers needed - this is meant for local/VPN use only
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{},
	}

	// Create a SettingEngine with fixed UDP ports
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetNetworkTypes([]webrtc.NetworkType{
		webrtc.NetworkTypeUDP4,
	})

	// Use single fixed UDP port (single user)
	if err := settingEngine.SetEphemeralUDPPortRange(50000, 50000); err != nil {
		logger.Log.Error("failed to set UDP port range",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to configure WebRTC", http.StatusInternalServerError)
		return
	}

	// Get public IP from environment variable or file for NAT traversal
	publicIP := os.Getenv("WEBRTC_PUBLIC_IP")
	if publicIP == "" {
		// Try to read from file (set by init container)
		if ipFile := os.Getenv("WEBRTC_PUBLIC_IP_FILE"); ipFile != "" {
			if data, err := os.ReadFile(ipFile); err == nil {
				publicIP = string(data)
				publicIP = strings.TrimSpace(publicIP)
			} else {
				logger.Log.Warn("could not read public IP from file",
					slog.String("component", "webrtc"),
					slog.String("file", ipFile),
					slog.String("error", err.Error()))
			}
		}
	}
	if publicIP != "" {
		logger.Log.Info("using public IP for ICE candidates",
			slog.String("component", "webrtc"),
			slog.String("ip", publicIP))
		settingEngine.SetNAT1To1IPs([]string{publicIP}, webrtc.ICECandidateTypeHost)
	} else {
		logger.Log.Warn("no public IP configured, ICE candidates may not work over NAT/VPN",
			slog.String("component", "webrtc"))
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	// Create new peer connection using the custom API
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		logger.Log.Error("failed to create peer connection",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}

	h.peerConnection = peerConnection

	// Create outgoing audio track for sending audio from doorbell to client
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: audio.CodecMimeType},
		"audio",
		"doorbell-audio",
	)
	if err != nil {
		logger.Log.Error("failed to create audio track",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to create audio track", http.StatusInternalServerError)
		return
	}

	// Add track to peer connection
	_, err = peerConnection.AddTrack(audioTrack)
	if err != nil {
		logger.Log.Error("failed to add track to peer connection",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to add track", http.StatusInternalServerError)
		return
	}

	// Handle incoming audio track (from browser/client to doorbell)
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		logger.Log.Info("received remote track",
			slog.String("component", "webrtc"),
			slog.String("kind", track.Kind().String()),
			slog.String("codec", track.Codec().MimeType))

		// Start session if not already active
		if h.activeSession == nil {
			logger.Log.Info("starting audio session", slog.String("component", "webrtc"))

			// Get available channels
			channels, err := h.hikClient.GetTwoWayAudioChannels()
			if err != nil {
				logger.Log.Error("failed to get audio channels",
					slog.String("component", "webrtc"),
					slog.String("error", err.Error()))
				return
			}

			if len(channels.Channels) == 0 {
				logger.Log.Warn("no audio channels available", slog.String("component", "webrtc"))
				return
			}

			// Find first available channel
			var channelID string
			for _, ch := range channels.Channels {
				if ch.Enabled == "false" {
					channelID = ch.ID
					break
				}
			}

			if channelID == "" {
				logger.Log.Warn("no available channels, all in use",
					slog.String("component", "webrtc"))
				return
			}

			session, err := h.hikClient.OpenAudioChannel(channelID)
			if err != nil {
				logger.Log.Error("failed to open audio channel",
					slog.String("component", "webrtc"),
					slog.String("channel_id", channelID),
					slog.String("error", err.Error()))
				return
			}
			h.activeSession = session

			// Create audio writer (for sending to doorbell)
			h.audioWriter = h.hikClient.NewAudioStreamWriter(session)
			h.audioWriter.Start()

			// Create audio reader (for receiving from doorbell)
			h.audioReader = h.hikClient.NewAudioStreamReader(session)
			h.audioReader.Start()

			// Start goroutine to read from doorbell and send via WebRTC
			// Pass audioReader as parameter to avoid race condition with cleanup()
			go func(reader *hikvision.AudioStreamReader, track *webrtc.TrackLocalStaticSample) {
				defer logger.Log.Info("stopped reading audio from doorbell", slog.String("component", "webrtc"))

				// Use io.ReadFull to read exactly audio.SampleSize bytes at a time
				buffer := make([]byte, audio.SampleSize)

				for {
					// Read exactly audio.SampleSize bytes
					n, err := io.ReadFull(reader, buffer)
					if err != nil {
						if err != io.EOF && err != io.ErrUnexpectedEOF {
							logger.Log.Error("error reading from doorbell",
								slog.String("component", "webrtc"),
								slog.String("error", err.Error()))
						}
						return
					}

					// Send to WebRTC track with precise timing
					if err := track.WriteSample(media.Sample{
						Data:     buffer[:n],
						Duration: audio.SampleDuration,
					}); err != nil {
						logger.Log.Error("error sending audio sample to client",
							slog.String("component", "webrtc"),
							slog.String("error", err.Error()))
						return
					}
				}
			}(h.audioReader, audioTrack)
		}

		// Read RTP packets and send to doorbell
		// Pass audioWriter as parameter to avoid race condition with cleanup()
		go func(writer *hikvision.AudioStreamWriter, remoteTrack *webrtc.TrackRemote) {
			defer func() {
				logger.Log.Info("track ended, cleaning up session", slog.String("component", "webrtc"))
				h.cleanup()
			}()

			for {
				rtp, _, err := remoteTrack.ReadRTP()
				if err != nil {
					if err != io.EOF {
						logger.Log.Error("error reading RTP packet",
							slog.String("component", "webrtc"),
							slog.String("error", err.Error()))
					}
					return
				}

				// Send audio payload to doorbell
				_, err = writer.Write(rtp.Payload)
				if err != nil {
					logger.Log.Error("error writing audio to doorbell",
						slog.String("component", "webrtc"),
						slog.String("error", err.Error()))
					return
				}
			}
		}(h.audioWriter, track)
	})

	// Handle connection state changes
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Log.Info("connection state changed",
			slog.String("component", "webrtc"),
			slog.String("state", state.String()))

		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			h.cleanup()
		}
	})

	// Set remote description (client's offer)
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		logger.Log.Error("failed to set remote description",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to set remote description", http.StatusInternalServerError)
		return
	}

	// Log ICE candidates for debugging
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			logger.Log.Debug("generated ICE candidate",
				slog.String("component", "webrtc"),
				slog.String("type", candidate.Typ.String()),
				slog.String("protocol", candidate.Protocol.String()),
				slog.String("address", candidate.Address),
				slog.Int("port", int(candidate.Port)))
		}
	})

	// Wait for ICE gathering to complete
	gatherComplete := make(chan struct{})
	peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		logger.Log.Info("ICE gathering state changed",
			slog.String("component", "webrtc"),
			slog.String("state", state.String()))
		if state == webrtc.ICEGatheringStateComplete {
			close(gatherComplete)
		}
	})

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		logger.Log.Error("failed to create SDP answer",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to create answer", http.StatusInternalServerError)
		return
	}

	// Set local description (this triggers ICE gathering)
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		logger.Log.Error("failed to set local description",
			slog.String("component", "webrtc"),
			slog.String("error", err.Error()))
		http.Error(w, "Failed to set local description", http.StatusInternalServerError)
		return
	}

	// Wait for ICE gathering to complete
	logger.Log.Info("waiting for ICE gathering to complete", slog.String("component", "webrtc"))
	<-gatherComplete

	// Send answer back to client (now with all ICE candidates)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(peerConnection.LocalDescription())

	logger.Log.Info("SDP answer sent successfully", slog.String("component", "webrtc"))
}

// cleanup closes the session and cleans up resources
func (h *WebRTCHandler) cleanup() {
	if h.audioWriter != nil {
		h.audioWriter.Close()
		h.audioWriter = nil
	}

	if h.audioReader != nil {
		h.audioReader.Close()
		h.audioReader = nil
	}

	if h.activeSession != nil {
		h.hikClient.CloseAudioChannel(h.activeSession.ChannelID)
		h.activeSession = nil
	}

	if h.peerConnection != nil {
		h.peerConnection.Close()
		h.peerConnection = nil
	}
}

// Close closes all WebRTC resources
func (h *WebRTCHandler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanup()
}

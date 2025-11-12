package audio

import "time"

// Audio format constants for G.711 µ-law codec
const (
	// SampleRate is the number of audio samples per second (8 kHz)
	SampleRate = 8000 // Hz

	// SampleDuration is the duration of each audio packet
	SampleDuration = 20 * time.Millisecond

	// SampleSize is the number of bytes in each audio packet
	// Calculated as: SampleRate * SampleDuration * BytesPerSample
	// 8000 Hz * 0.020 s * 1 byte/sample = 160 bytes
	SampleSize = 160 // bytes

	// CodecMimeType is the MIME type for G.711 µ-law codec
	CodecMimeType = "audio/PCMU"

	// BytesPerSample is the number of bytes per audio sample for G.711
	BytesPerSample = 1
)

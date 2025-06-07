//go:build disabled
// +build disabled

package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/joho/godotenv"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

// H264Reader wraps an io.Reader and adds H264 stream analysis
type H264Reader struct {
	reader io.ReadCloser
	name   string
	buffer bytes.Buffer
}

func (h *H264Reader) Read(p []byte) (n int, err error) {
	// Read from the underlying reader
	n, err = h.reader.Read(p)
	if n > 0 {
		// Look for start codes (0x00 0x00 0x00 0x01 or 0x00 0x00 0x01)
		start := 0
		for i := 0; i < n-4; i++ {
			if (p[i] == 0 && p[i+1] == 0 && p[i+2] == 0 && p[i+3] == 1) ||
				(p[i] == 0 && p[i+1] == 0 && p[i+2] == 1) {
				if start < i {
					fmt.Printf("[%s] Found start code at offset %d, previous chunk size: %d\n",
						h.name, i, i-start)
				}
				start = i
			}
		}
		fmt.Printf("[%s] Read %d bytes\n", h.name, n)
	}
	return n, err
}

func (h *H264Reader) Close() error {
	return h.reader.Close()
}

// DebugReader wraps an io.Reader and logs when data is read
type DebugReader struct {
	reader io.ReadCloser
	name   string
}

func (d *DebugReader) Read(p []byte) (n int, err error) {
	n, err = d.reader.Read(p)
	if n > 0 {
		fmt.Printf("[%s] Read %d bytes\n", d.name, n)
	}
	return n, err
}

func (d *DebugReader) Close() error {
	return d.reader.Close()
}

func init() {
	// Configure logger to write to stdout with timestamp
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

func main() {
	// Load .env.local file
	err := godotenv.Load(".env.local")
	if err != nil {
		log.Fatal("Error loading .env.local file")
	}

	hostURL := os.Getenv("LIVEKIT_URL")
	apiKey := os.Getenv("LIVEKIT_API_KEY")
	apiSecret := os.Getenv("LIVEKIT_API_SECRET")
	roomName := "test-room"
	identity := "go-user"

	roomCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: trackSubscribed,
		},
	}

	room, err := lksdk.ConnectToRoom(hostURL, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: identity,
	}, roomCB)
	if err != nil {
		panic(err)
	}

	// Start ffmpeg process for video encoding
	videoCmd := exec.Command("ffmpeg",
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-s", "512x512",
		"-r", "25",
		"-i", "video.i420",
		"-c:v", "h264_nvenc",
		"-preset", "fast", // Faster encoding for lower latency
		"-profile:v", "baseline",
		"-g", "30", // Keyframe every 30 frames (1.2s)
		"-keyint_min", "1", // Allow keyframe at the very first frame
		"-force_key_frames", "expr:gte(t,n_forced*2)", // Force keyframe every 2 seconds
		"-bf", "0", // Disable B-frames for safer streaming
		"-max_delay", "0",
		"-f", "h264",
		"-")

	// Start ffmpeg process for audio encoding
	audioCmd := exec.Command("ffmpeg",
		"-f", "s16le",
		"-ar", "16000",
		"-ac", "1",
		"-i", "audio.raw",
		"-c:a", "libopus",
		"-ar", "48000", // Resample to 48kHz for WebRTC
		"-page_duration", "20000",
		"-application", "voip", // Optimize for real-time communication
		"-packet_loss", "10", // Handle packet loss better
		"-f", "ogg",
		"-")

	// Create pipes for video and audio
	videoPipe, err := videoCmd.StdoutPipe()
	if err != nil {
		log.Fatal("Error creating video pipe:", err)
	}

	audioPipe, err := audioCmd.StdoutPipe()
	if err != nil {
		log.Fatal("Error creating audio pipe:", err)
	}

	// Create debug readers with buffer size tracking
	videoDebugReader := &DebugReader{reader: videoPipe, name: "Video"}
	audioDebugReader := &DebugReader{reader: audioPipe, name: "Audio"}

	// Start the ffmpeg processes
	if err := videoCmd.Start(); err != nil {
		log.Fatal("Error starting video ffmpeg:", err)
	}
	if err := audioCmd.Start(); err != nil {
		log.Fatal("Error starting audio ffmpeg:", err)
	}

	// Variables for timing
	var frameCount int
	var lastFrameTime time.Time
	var totalEncodeTime time.Duration
	var maxEncodeTime time.Duration
	var minEncodeTime time.Duration = time.Hour // Initialize with a large value
	var startTime time.Time
	var firstVideoFrame bool
	var firstAudioFrame bool
	var videoBytesRead int64
	var audioBytesRead int64
	var audioFrameCount int

	// Create video track with timing callback
	videoTrack, err := lksdk.NewLocalReaderTrack(videoDebugReader, webrtc.MimeTypeH264,
		lksdk.ReaderTrackWithFrameDuration(40*time.Millisecond), // 25fps = 40ms per frame
		lksdk.ReaderTrackWithOnWriteComplete(func() {
			now := time.Now()
			if !firstVideoFrame {
				startTime = now
				firstVideoFrame = true
				fmt.Printf("[Video] First frame received at %v (time since start: %v, bytes read: %d)\n",
					now, now.Sub(startTime), videoBytesRead)
			} else {
				encodeTime := now.Sub(lastFrameTime)
				totalEncodeTime += encodeTime
				frameCount++

				// Update min/max encode times
				if encodeTime > maxEncodeTime {
					maxEncodeTime = encodeTime
				}
				if encodeTime < minEncodeTime {
					minEncodeTime = encodeTime
				}

				// Print stats every 100 frames
				if frameCount%100 == 0 {
					avgEncodeTime := totalEncodeTime / time.Duration(frameCount)
					fmt.Printf("[Video] Frame %d - Encode time: %v (avg: %v, min: %v, max: %v, total bytes: %d)\n",
						frameCount, encodeTime, avgEncodeTime, minEncodeTime, maxEncodeTime, videoBytesRead)
				}
			}
			lastFrameTime = now
		}),
	)
	if err != nil {
		log.Fatal("Error creating video track:", err)
	}

	// Create audio track with timing callback
	audioTrack, err := lksdk.NewLocalReaderTrack(audioDebugReader, webrtc.MimeTypeOpus,
		lksdk.ReaderTrackWithFrameDuration(20*time.Millisecond),
		lksdk.ReaderTrackWithOnWriteComplete(func() {
			now := time.Now()
			if !firstAudioFrame {
				firstAudioFrame = true
				fmt.Printf("[Audio] First frame received at %v (delay from video start: %v, bytes read: %d)\n",
					now, now.Sub(startTime), audioBytesRead)
			} else {
				audioFrameCount++
				if audioFrameCount%500 == 0 {
					fmt.Printf("[Audio] Processed %d frames (time since start: %v, total bytes: %d)\n",
						audioFrameCount, now.Sub(startTime), audioBytesRead)
				}
			}
		}),
	)
	if err != nil {
		log.Fatal("Error creating audio track:", err)
	}

	// Publish video track
	if _, err = room.LocalParticipant.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{
		Name:        "video",
		VideoWidth:  512,
		VideoHeight: 512,
	}); err != nil {
		log.Fatal("Error publishing video track:", err)
	}

	// Publish audio track
	if _, err = room.LocalParticipant.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{
		Name: "audio",
	}); err != nil {
		log.Fatal("Error publishing audio track:", err)
	}

	// Wait for 60 seconds
	time.Sleep(60 * time.Second)

	// Print final stats
	if frameCount > 0 {
		avgEncodeTime := totalEncodeTime / time.Duration(frameCount)
		fmt.Printf("[Final Stats] Video - Total frames: %d, Avg encode time: %v, Min: %v, Max: %v\n",
			frameCount, avgEncodeTime, minEncodeTime, maxEncodeTime)
	}
	fmt.Printf("[Final Stats] Audio - Total frames: %d\n", audioFrameCount)

	// Clean up
	videoCmd.Process.Kill()
	audioCmd.Process.Kill()
	room.Disconnect()
}

func trackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log.Printf("Track subscribed: %s from participant %s", track.ID(), rp.Identity())
}

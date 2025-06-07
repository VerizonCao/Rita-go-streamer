//go:build disabled
// +build disabled

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/google/uuid"
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
		// fmt.Printf("[%s] Read %d bytes\n", d.name, n)
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
	if len(os.Args) < 2 {
		log.Fatal("Please provide a room name as argument")
	}
	roomName := os.Args[1]
	identity := fmt.Sprintf("Avatar-%s", uuid.New().String()[:8])

	// Load .env.local file
	err := godotenv.Load(".env.local")
	if err != nil {
		log.Fatal("Error loading .env.local file")
	}

	hostURL := os.Getenv("LIVEKIT_URL")
	apiKey := os.Getenv("LIVEKIT_API_KEY")
	apiSecret := os.Getenv("LIVEKIT_API_SECRET")

	// Create named pipes for reading raw data
	videoPipePath := "/tmp/video_pipe.yuv"
	audioPipePath := "/tmp/audio_pipe.raw"

	// Remove existing pipes if they exist
	os.Remove(videoPipePath)
	os.Remove(audioPipePath)

	// Create new pipes
	if err := syscall.Mkfifo(videoPipePath, 0666); err != nil {
		log.Fatal("Error creating video pipe:", err)
	}
	if err := syscall.Mkfifo(audioPipePath, 0666); err != nil {
		log.Fatal("Error creating audio pipe:", err)
	}
	log.Printf("Created video pipe at %s", videoPipePath)
	log.Printf("Created audio pipe at %s", audioPipePath)

	// Open named pipes for reading raw data
	rawVideoPipe, err := os.OpenFile(videoPipePath, os.O_RDONLY, 0666)
	if err != nil {
		log.Fatal("Error opening video pipe:", err)
	}
	defer rawVideoPipe.Close()

	rawAudioPipe, err := os.OpenFile(audioPipePath, os.O_RDONLY, 0666)
	if err != nil {
		log.Fatal("Error opening audio pipe:", err)
	}
	defer rawAudioPipe.Close()

	log.Printf("Pipes opened successfully, waiting for sender...")

	// Read frame dimensions from video pipe
	var frameWidth, frameHeight uint32
	err = binary.Read(rawVideoPipe, binary.LittleEndian, &frameWidth)
	if err != nil {
		log.Fatal("Error reading frame width:", err)
	}
	err = binary.Read(rawVideoPipe, binary.LittleEndian, &frameHeight)
	if err != nil {
		log.Fatal("Error reading frame height:", err)
	}
	log.Printf("Received video dimensions: %dx%d", frameWidth, frameHeight)

	// Now that we have the dimensions, connect to the room
	roomCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: trackSubscribed,
		},
	}

	room, err := lksdk.ConnectToRoom(hostURL, lksdk.ConnectInfo{
		APIKey:    apiKey,
		APISecret: apiSecret,
		RoomName:  roomName,
		ParticipantAttributes: map[string]string{
			"role": "agent-avatar",
		},
		ParticipantIdentity: identity,
		ParticipantName:     "Avatar",
	}, roomCB)
	if err != nil {
		panic(err)
	}

	// Start ffmpeg process for video encoding
	videoCmd := exec.Command("ffmpeg",
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", frameWidth, frameHeight),
		"-r", "25", // Match sender's VIDEO_FPS
		"-i", "pipe:0", // Read from stdin
		"-c:v", "h264_nvenc",
		"-preset", "p1", // Use lowest latency preset
		"-tune", "ll", // Low latency tuning
		"-profile:v", "baseline",
		"-g", "25", // Keyframe every second (25 frames)
		"-keyint_min", "1",
		"-bf", "0", // Disable B-frames
		"-max_delay", "0",
		"-bufsize", "0", // Disable buffering
		"-f", "h264",
		"-")

	// Start ffmpeg process for audio encoding
	// audioCmd := exec.Command("ffmpeg",
	// 	"-f", "s16le",
	// 	"-ar", "16000", // Match sender's sample rate
	// 	"-ac", "1",
	// 	"-i", "pipe:0", // Read from stdin
	// 	"-c:a", "libopus",
	// 	"-ar", "48000", // Resample to 48kHz for WebRTC
	// 	"-page_duration", "20000", // 20ms frames
	// 	"-max_delay", "0", // Minimize delay
	// 	"-application", "voip", // Optimize for real-time communication
	// 	"-packet_loss", "10", // Allow some packet loss for lower latency
	// 	"-frame_duration", "20", // 20ms frame duration
	// 	"-bufsize", "0", // Disable buffering
	// 	"-f", "ogg",
	// 	"-")

	audioCmd := exec.Command("ffmpeg",
		"-fflags", "nobuffer",
		"-flush_packets", "1",
		"-f", "s16le",
		"-ar", "16000",
		"-ac", "1",
		"-i", "pipe:0",
		"-c:a", "libopus",
		"-ar", "48000",
		"-page_duration", "20000",
		"-application", "voip",
		"-frame_duration", "20",
		"-bufsize", "0",
		"-f", "ogg",
		"-")

	// Create pipes for ffmpeg input
	videoCmd.Stdin = rawVideoPipe
	audioCmd.Stdin = rawAudioPipe

	// Create pipes for ffmpeg output
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
	var audioFrameCount int
	audioTrack, err := lksdk.NewLocalReaderTrack(audioDebugReader, webrtc.MimeTypeOpus,
		lksdk.ReaderTrackWithFrameDuration(20*time.Millisecond), // 50fps = 20ms per frame
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

	// Publish audio track
	if _, err = room.LocalParticipant.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{
		Name: "audio",
	}); err != nil {
		log.Fatal("Error publishing audio track:", err)
	}

	// Publish video track
	if _, err = room.LocalParticipant.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{
		Name:        "video",
		VideoWidth:  int(frameWidth),
		VideoHeight: int(frameHeight),
	}); err != nil {
		log.Fatal("Error publishing video track:", err)
	}

	// Check for remote participants and exit when none are found for 3 seconds
	noParticipantsCount := 0
	for {
		time.Sleep(1 * time.Second)
		remoteParticipants := room.GetRemoteParticipants()
		if len(remoteParticipants) == 0 {
			noParticipantsCount++
			if noParticipantsCount >= 3 {
				log.Printf("No remote participants for 3 seconds, exiting...")
				break
			}
		} else {
			noParticipantsCount = 0
		}
	}

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
	fmt.Printf("Track subscribed: %s from participant %s\n", track.ID(), rp.Identity())
}

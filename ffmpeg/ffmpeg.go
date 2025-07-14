package ffmpeg

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// EncodeParams represents parameters for video encoding
type EncodeParams struct {
	InputPath  string
	OutputPath string
	Quality    float64
	Is10Bit    bool
	FromTime   time.Duration
	Duration   time.Duration
	Width      int
	Height     int
	ExtraArgs  []string
}

// ProbeResult represents the output of ffprobe analysis
type ProbeResult struct {
	Duration    time.Duration
	Codec       string
	FPS         float64
	SizeBytes   int64
	Width       int
	Height      int
	Bitrate     int64
	Container   string
	AspectRatio float64
	SampleAR    float64
}

func (p ProbeResult) IsVertical() bool {
	return p.Width < p.Height
}

// probeOutput represents the JSON structure returned by ffprobe
type probeOutput struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

type probeStream struct {
	CodecType         string `json:"codec_type"`
	CodecName         string `json:"codec_name"`
	Width             int    `json:"width"`
	Height            int    `json:"height"`
	RFrameRate        string `json:"r_frame_rate"`
	BitRate           string `json:"bit_rate"`
	SampleAspectRatio string `json:"sample_aspect_ratio"`
}

type probeFormat struct {
	Duration string `json:"duration"`
	Size     string `json:"size"`
	BitRate  string `json:"bit_rate"`
}

// Probe analyzes a video file and returns metadata
func Probe(ctx context.Context, videoPath string) (ProbeResult, error) {
	log.Ctx(ctx).Printf("Executing ffprobe on %s", videoPath)

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_streams",
		"-show_format",
		"-print_format", "json",
		videoPath)

	output, err := cmd.Output()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("failed to run ffprobe: %w", err)
	}

	var result probeOutput
	if err := json.Unmarshal(output, &result); err != nil {
		return ProbeResult{}, fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	// Find video stream
	var videoStream *probeStream
	for _, stream := range result.Streams {
		if stream.CodecType == "video" {
			videoStream = &stream
			break
		}
	}

	if videoStream == nil {
		return ProbeResult{}, errors.New("video stream not found")
	}

	durationSec, err := strconv.ParseFloat(result.Format.Duration, 64)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("failed to parse duration: %w", err)
	}
	duration := time.Duration(durationSec) * time.Second

	fps := parseFPS(videoStream.RFrameRate)

	size, _ := strconv.ParseInt(result.Format.Size, 10, 64)

	bitrate, _ := strconv.ParseInt(videoStream.BitRate, 10, 64)
	if bitrate == 0 {
		bitrate, _ = strconv.ParseInt(result.Format.BitRate, 10, 64)
	}

	sampleAR := parseSampleAspectRatio(videoStream.SampleAspectRatio)

	aspectRatio := float64(videoStream.Width) / float64(videoStream.Height)

	container := strings.ToLower(strings.TrimPrefix(filepath.Ext(videoPath), "."))

	return ProbeResult{
		Duration:    duration,
		Codec:       videoStream.CodecName,
		FPS:         fps,
		SizeBytes:   size,
		Width:       videoStream.Width,
		Height:      videoStream.Height,
		Bitrate:     bitrate,
		Container:   container,
		AspectRatio: aspectRatio,
		SampleAR:    sampleAR,
	}, nil
}

// parseFPS parses frame rate string like "30000/1001"
func parseFPS(rFrameRate string) float64 {
	parts := strings.Split(rFrameRate, "/")
	if len(parts) != 2 {
		return 0
	}

	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)

	if err1 != nil || err2 != nil || den == 0 {
		return 0
	}

	return num / den
}

// parseSampleAspectRatio parses sample aspect ratio string like "1:1"
func parseSampleAspectRatio(sar string) float64 {
	if sar == "" {
		return 1.0
	}

	parts := strings.Split(sar, ":")
	if len(parts) != 2 {
		return 1.0
	}

	w, err1 := strconv.ParseFloat(parts[0], 64)
	h, err2 := strconv.ParseFloat(parts[1], 64)

	if err1 != nil || err2 != nil || h == 0 {
		return 1.0
	}

	return w / h
}

// EncodeProgress represents encoding progress information
type EncodeProgress struct {
	Percent     float64
	FPSAvg      float64
	ETA         time.Duration
	CurrentSize int64
}

func (e *EncodeProgress) String() string {
	return fmt.Sprintf("%3.1ffps, %3.1fMB/%3.1fMB (%.1f%%) ETA: %s",
		e.FPSAvg, e.EncodedMB(), e.EstimatedMB(), e.Percent, e.ETA)
}

// EncodedMB returns the current encoded size in MB
func (e *EncodeProgress) EncodedMB() float64 {
	return float64(e.CurrentSize) / 1048576
}

// EstimatedMB returns the estimated total size in MB
func (e *EncodeProgress) EstimatedMB() float64 {
	if e.Percent == 0 {
		return 0
	}
	mb := e.EncodedMB() / (e.Percent / 100)
	return round(mb, 1)
}

type ProgressCallback = func(progress EncodeProgress)

// Encode encodes video using FFmpeg
func Encode(ctx context.Context, params EncodeParams, onProgress ProgressCallback) error {
	args := []string{
		"ffmpeg",
		"-y",
		"-progress", "pipe:1",
		"-stats_period", "3",
		"-i", params.InputPath,
		"-c:v", "hevc_videotoolbox",
		"-q:v", fmt.Sprintf("%.0f", params.Quality),
		"-profile:v", "main",
		"-map_metadata", "0",
		"-metadata", fmt.Sprintf("title=%s", strings.TrimSuffix(filepath.Base(params.InputPath), filepath.Ext(params.InputPath))),
	}

	// Add video scaling filter if width or height are specified
	if params.Width > 0 || params.Height > 0 {
		var scaleFilter string
		if params.Width > 0 && params.Height > 0 {
			// Both dimensions specified - scale to exact size maintaining aspect ratio (fit within)
			scaleFilter = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", params.Width, params.Height)
		} else if params.Width > 0 {
			// Only width specified - scale proportionally
			scaleFilter = fmt.Sprintf("scale=%d:-2", params.Width)
		} else {
			// Only height specified - scale proportionally
			scaleFilter = fmt.Sprintf("scale=-2:%d", params.Height)
		}
		args = append(args, "-vf", scaleFilter)
	}

	args = append(args, params.OutputPath)

	if params.Is10Bit {
		// Replace profile with main10
		for i, arg := range args {
			if arg == "main" && i > 0 && args[i-1] == "-profile:v" {
				args[i] = "main10"
				break
			}
		}
	}

	if params.FromTime > 0 {
		// Insert before -i
		var newArgs []string
		for _, arg := range args {
			if arg == "-i" {
				newArgs = append(newArgs, "-ss", fmt.Sprintf("%d", int(params.FromTime.Seconds())))
			}
			newArgs = append(newArgs, arg)
		}
		args = newArgs
	}

	var totalDuration time.Duration
	if params.Duration > 0 {
		totalDuration = params.Duration
		// Insert before -i
		var newArgs []string
		for _, arg := range args {
			if arg == "-i" {
				newArgs = append(newArgs, "-t", fmt.Sprintf("%d", int(params.Duration.Seconds())))
			}
			newArgs = append(newArgs, arg)
		}
		args = newArgs
	} else {
		// probe
		probe, err := Probe(ctx, params.InputPath)
		if err != nil {
			return fmt.Errorf("failed to probe video: %w", err)
		}
		totalDuration = probe.Duration
	}

	args = append(args, params.ExtraArgs...)

	log.Ctx(ctx).Debug().Strs("args", args).Msg("starting ffmpeg encoding")

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start FFmpeg: %w", err)
	}

	// Parse progress using iterator
	if onProgress != nil {
		go func() {
			for progress := range iterProgress(stdout, totalDuration) {
				onProgress(progress)
			}
		}()
	}

	return cmd.Wait()
}

// iterProgress returns an iterator that yields EncodeProgress updates from FFmpeg output
func iterProgress(r io.Reader, totalDuration time.Duration) iter.Seq[EncodeProgress] {
	return func(yield func(EncodeProgress) bool) {
		scanner := bufio.NewScanner(r)
		var currentProgress EncodeProgress
		var startTime time.Time
		progressStarted := false

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())

			// Track when progress starts
			if strings.HasPrefix(line, "progress=continue") && !progressStarted {
				startTime = time.Now()
				progressStarted = true
			}

			// Parse FPS
			if strings.HasPrefix(line, "fps=") {
				fpsStr := strings.TrimPrefix(line, "fps=")
				if fps, err := strconv.ParseFloat(fpsStr, 64); err == nil {
					currentProgress.FPSAvg = fps
				}
			}

			// Parse total size
			if strings.HasPrefix(line, "total_size=") {
				sizeStr := strings.TrimPrefix(line, "total_size=")
				if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
					currentProgress.CurrentSize = size
				}
			}

			if strings.HasPrefix(line, "out_time_ms=") {
				// Parse time progress from FFmpeg
				timeMs := strings.TrimPrefix(line, "out_time_ms=")
				if ms, err := strconv.ParseInt(timeMs, 10, 64); err == nil {
					if totalDuration > 0 {
						currentTime := time.Duration(ms * 1000)
						percent := round(min(100.0, float64(currentTime)/float64(totalDuration)*100), 2)
						currentProgress.Percent = percent

						// Calculate ETA if we have progress and time elapsed
						if progressStarted && percent > 0 && percent < 100 {
							elapsed := time.Since(startTime)
							estimated := time.Duration(float64(elapsed) * 100 / percent)
							currentProgress.ETA = (estimated - elapsed).Truncate(time.Second)
						}

						if !yield(currentProgress) {
							return
						}
					}
				}
			}
		}
	}
}

func round(n float64, precision int) float64 {
	if precision < 0 {
		return n
	}
	pow := math.Pow(10, float64(precision))
	return math.Round(n*pow) / pow
}

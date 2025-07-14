package handbrake

import (
	"context"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// EncodeParams represents parameters for HandBrake video encoding
type EncodeParams struct {
	InputPath  string
	OutputPath string
	Quality    float64
	Is10Bit    bool
	FromTime   time.Duration
	Duration   time.Duration
	Denoise    bool
	Width      int
	Height     int
	ExtraArgs  []string
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

func (e *EncodeProgress) EstimatedMB() float64 {
	if e.Percent == 0 {
		return 0
	}
	mb := e.EncodedMB() / (e.Percent / 100)
	return round(mb, 1)
}

func round(val float64, precision int) float64 {
	factor := math.Pow(10, float64(precision))
	return math.Round(val*factor) / factor
}

type ProgressCallback = func(progress EncodeProgress)

// iterLines returns an iterator over lines from a reader, handling both \r and \n as line endings
func iterLines(reader io.Reader) iter.Seq[string] {
	return func(yield func(string) bool) {
		buf := make([]byte, 1)
		var currentLine strings.Builder

		for {
			n, err := reader.Read(buf)
			if err != nil {
				if err == io.EOF {
					// Yield any remaining content
					if currentLine.Len() > 0 {
						yield(currentLine.String())
					}
				}
				return
			}

			if n > 0 {
				char := buf[0]
				switch char {
				case '\r', '\n':
					// Line ending - yield current line and reset
					if currentLine.Len() > 0 {
						if !yield(currentLine.String()) {
							return
						}
						currentLine.Reset()
					}
					// Empty line, continue reading
				default:
					// Regular character - add to current line
					currentLine.WriteByte(char)
				}
			}
		}
	}
}

// Encode encodes video using HandBrake
func Encode(ctx context.Context, params EncodeParams, onProgress ProgressCallback) error {
	encoder := "vt_h265"
	if params.Is10Bit {
		encoder = "vt_h265_10bit"
	}

	args := []string{
		"HandbrakeCLI",
		"--format", "av_mp4",
		"--input", params.InputPath,
		"--output", params.OutputPath,
		"--optimize",
		"--encoder", encoder,
		"--quality", fmt.Sprintf("%.0f", params.Quality),
		"--vfr",
		"--aencoder", "ac3",
		"--ab", "160",
		"--non-anamorphic",
		"--verbose", "1",
	}

	if params.FromTime > 0 {
		args = append(args, "--start-at", fmt.Sprintf("duration:%0.1f", params.FromTime.Seconds()))
	}

	if params.Duration > 0 {
		args = append(args, "--stop-at", fmt.Sprintf("duration:%0.1f", params.Duration.Seconds()))
	}

	if params.Denoise {
		args = append(args, "--hqdn3d", "light")
	}

	// Add video scaling parameters if width or height are specified
	if params.Width > 0 || params.Height > 0 {
		if params.Width > 0 && params.Height > 0 {
			// Both dimensions specified - use exact dimensions
			args = append(args, "--width", strconv.Itoa(params.Width), "--height", strconv.Itoa(params.Height))
		} else if params.Width > 0 {
			// Only width specified - scale proportionally
			args = append(args, "--width", strconv.Itoa(params.Width))
		} else {
			// Only height specified - scale proportionally
			args = append(args, "--height", strconv.Itoa(params.Height))
		}
	}

	args = append(args, params.ExtraArgs...)

	log.Ctx(ctx).Debug().Strs("args", args).Msg("starting handbrake encoding")

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	log.Ctx(ctx).Debug().Msg("starting handbrake process")

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start handbrake: %w", err)
	}

	if onProgress != nil {
		go func() {
			for line := range iterLines(stdout) {
				if progress, ok := parseProgress(line, params.OutputPath); ok {
					onProgress(progress)
				}
			}
		}()
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("handbrake failed: %w", err)
	}

	return nil
}

// parseProgress extracts progress information from HandBrake output line
func parseProgress(line string, outputPath string) (EncodeProgress, bool) {
	progressRe := regexp.MustCompile(`Encoding: task \d+ of \d+, ([\d.]+) %(?:\s*\([^,]+,\s*avg\s+([\d.]+)\s*fps,\s*ETA\s+([^)]+)\))?`)

	matches := progressRe.FindStringSubmatch(line)
	if len(matches) < 2 {
		return EncodeProgress{}, false
	}

	percent, _ := strconv.ParseFloat(matches[1], 64)

	var fpsAvg float64
	var eta time.Duration

	// Check if we have FPS and ETA data
	if len(matches) >= 4 && matches[2] != "" {
		fpsAvg, _ = strconv.ParseFloat(matches[2], 64)
		etaStr := strings.TrimSpace(matches[3])
		eta, _ = time.ParseDuration(etaStr)
	}

	// Get current file size
	var currentSize int64
	if stat, err := os.Stat(outputPath); err == nil {
		currentSize = stat.Size()
	}

	return EncodeProgress{
		Percent:     math.Round(percent*10) / 10,
		FPSAvg:      fpsAvg,
		ETA:         eta,
		CurrentSize: currentSize,
	}, true
}

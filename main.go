package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"encz/ffmpeg"
	"encz/handbrake"
)

type cliArgs struct {
	VideoPath string
	OutputDir string
	Encoder   string
	Quality   float64
	Denoise   bool
	Is10Bit   bool
	FromTime  time.Duration
	ToTime    time.Duration
	Duration  time.Duration
	ExtraArgs []string
}

// parseArgs parses command line arguments
func parseArgs() cliArgs {
	var config cliArgs

	flag.StringVar(&config.Encoder, "encoder", "handbrake", "encoder engine (handbrake or ffmpeg)")
	flag.StringVar(&config.Encoder, "e", "handbrake", "encoder engine (handbrake or ffmpeg)")
	flag.Float64Var(&config.Quality, "quality", 35, "x265 quality factor")
	flag.Float64Var(&config.Quality, "q", 35, "x265 quality factor")
	flag.StringVar(&config.OutputDir, "output-dir", "", "directory to save encoded files")
	flag.StringVar(&config.OutputDir, "o", "", "directory to save encoded files")
	flag.BoolVar(&config.Denoise, "denoise", false, "enable denoise filter (HandBrake only)")
	flag.BoolVar(&config.Is10Bit, "10bit", true, "encode using 10-bit profile")
	// Handle 8bit flag to override 10bit
	eightBit := flag.Bool("8bit", false, "encode using 8-bit profile")

	flag.DurationVar(&config.FromTime, "from", 0, "start encoding from this time (e.g., 5m30s, 1h30m, 300s)")
	flag.DurationVar(&config.ToTime, "to", 0, "end encoding at this time (e.g., 10m, 1h30m, 420s)")
	flag.DurationVar(&config.Duration, "duration", 0, "encoding duration (e.g., 10m, 1h30m, 420s)")

	flag.Parse()

	if *eightBit {
		config.Is10Bit = false
	}

	args := flag.Args()
	if len(args) >= 1 {
		config.VideoPath = args[0]
		config.ExtraArgs = args[1:]
	}

	return config
}

// Validate validates the command line arguments
func (c *cliArgs) Validate() error {
	if c.VideoPath == "" {
		return fmt.Errorf("video path is required")
	}

	// Check that duration and to are mutually exclusive
	if c.Duration > 0 && c.ToTime > 0 {
		return fmt.Errorf("cannot specify both --duration and --to flags")
	}

	// Check that to time is after from time
	if c.ToTime > 0 && c.ToTime <= c.FromTime {
		return fmt.Errorf("--to time must be after --from time")
	}

	return nil
}

// generateFilename generates a new filename based on video properties
func generateFilename(ctx context.Context, videoPath string) (string, error) {
	probe, err := ffmpeg.Probe(ctx, videoPath)
	if err != nil {
		return "", err
	}

	maxLength := max(probe.Width, probe.Height)

	var resolution string
	switch {
	case maxLength >= 3000:
		resolution = "4K"
	case maxLength >= 1900 && maxLength <= 2000:
		resolution = "1080p"
	case maxLength >= 1200 && maxLength <= 1400:
		resolution = "720p"
	}

	baseName := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// Remove existing resolution tags
	re := regexp.MustCompile(`\[\d+[pk]\]`)
	newStem := strings.TrimSpace(re.ReplaceAllString(baseName, ""))

	if resolution != "" {
		newStem = fmt.Sprintf("%s [%s, x265]", newStem, resolution)
	} else {
		newStem = fmt.Sprintf("%s [x265]", newStem)
	}

	return newStem, nil
}

func run(ctx context.Context, args cliArgs) error {
	log.Ctx(ctx).Debug().
		Str("input", args.VideoPath).
		Str("encoder", args.Encoder).
		Float64("quality", args.Quality).
		Bool("10bit", args.Is10Bit).
		Dur("from", args.FromTime).
		Dur("duration", args.Duration).
		Dur("to", args.ToTime).
		Msg("Starting encoding")

	// Expand and resolve video path
	if strings.HasPrefix(args.VideoPath, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		args.VideoPath = filepath.Join(home, args.VideoPath[1:])
	}

	absPath, err := filepath.Abs(args.VideoPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}
	args.VideoPath = absPath

	log.Ctx(ctx).Debug().Str("resolved_path", args.VideoPath).Msg("Resolved input path")

	// Check if file exists
	if _, err := os.Stat(args.VideoPath); os.IsNotExist(err) {
		return fmt.Errorf("no such file: %s", args.VideoPath)
	}

	// Validate filename length
	if len(filepath.Base(args.VideoPath)) >= 128 {
		return fmt.Errorf("filename is too long")
	}

	// Generate output filename
	newStem, err := generateFilename(ctx, args.VideoPath)
	if err != nil {
		return fmt.Errorf("failed to generate filename: %w", err)
	}

	savePath := filepath.Join(filepath.Dir(args.VideoPath), newStem+".mp4")

	// Check if input and output are the same
	if args.VideoPath == savePath {
		ext := filepath.Ext(args.VideoPath)
		savePath = strings.TrimSuffix(args.VideoPath, ext) + ".reencoded" + ext
	}

	// Set up output directory
	outputDir := args.OutputDir
	if outputDir == "" {
		outputDir = filepath.Join(filepath.Dir(savePath), "_reenc")
	}

	if strings.HasPrefix(outputDir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		outputDir = filepath.Join(home, outputDir[1:])
	}

	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute output directory: %w", err)
	}
	outputDir = absOutputDir

	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	savePath = filepath.Join(outputDir, filepath.Base(savePath))

	log.Ctx(ctx).Debug().Str("output_path", savePath).Msg("Final output path determined")

	// Calculate duration if --to flag is used instead of --duration
	encodeDuration := args.Duration
	if args.ToTime > 0 {
		encodeDuration = args.ToTime - args.FromTime
		log.Ctx(ctx).Debug().Dur("calculated_duration", encodeDuration).Msg("Calculated duration from to-from")
	}

	// Start encoding
	if args.Encoder == "ffmpeg" {
		// Use ffmpeg package for encoding
		params := ffmpeg.EncodeParams{
			InputPath:  args.VideoPath,
			OutputPath: savePath,
			Quality:    args.Quality,
			Is10Bit:    args.Is10Bit,
			FromTime:   args.FromTime,
			Duration:   encodeDuration,
			ExtraArgs:  args.ExtraArgs,
		}

		return ffmpeg.Encode(ctx, params, func(p ffmpeg.EncodeProgress) {
			fmt.Printf("\rEncode: %3ffps, %3fMB/%3fMB (%.1f%%) ETA: %s",
				p.FPSAvg, p.EncodedMB(), p.EstimatedMB(), p.Percent, p.ETA)
		})
	} else {
		// Use HandBrake for encoding
		params := handbrake.EncodeParams{
			InputPath:  args.VideoPath,
			OutputPath: savePath,
			Quality:    args.Quality,
			Is10Bit:    args.Is10Bit,
			FromTime:   args.FromTime,
			Duration:   encodeDuration,
			Denoise:    args.Denoise,
			ExtraArgs:  args.ExtraArgs,
		}

		return handbrake.Encode(ctx, params, func(p handbrake.EncodeProgress) {
			fmt.Printf("\rEncode: %3.1ffps, %3.1fMB/%3.1fMB (%.1f%%) ETA: %s",
				p.FPSAvg, p.EncodedMB(), p.EstimatedMB(), p.Percent, p.ETA)
		})
	}
}

func main() {
	// Setup zerolog
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.DefaultContextLogger = &log.Logger

	args := parseArgs()

	// Validate the parsed arguments
	if err := args.Validate(); err != nil {
		log.Fatal().Err(err).Send()
		return
	}

	// Set up context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Run the main application logic
	if err := run(ctx, args); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Info().Msg("Encoding cancelled by user")
			os.Exit(1)
		}
		log.Fatal().Err(err).Msg("Encoding failed")
	}
}

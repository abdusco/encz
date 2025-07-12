package main

import (
	"cmp"
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
	Width     int
	Height    int
	Debug     bool
	ExtraArgs []string
	Version   bool
}

// parseArgs parses command line arguments
func parseArgs() cliArgs {
	var config cliArgs

	flag.BoolVar(&config.Version, "version", false, "show version information")
	flag.StringVar(&config.Encoder, "encoder", "handbrake", "encoder engine (handbrake or ffmpeg)")
	flag.Float64Var(&config.Quality, "quality", 35, "x265 quality factor")
	flag.StringVar(&config.OutputDir, "output-dir", "", "directory to save encoded files")
	flag.BoolVar(&config.Denoise, "denoise", false, "enable denoise filter (HandBrake only)")
	flag.BoolVar(&config.Is10Bit, "10bit", true, "encode using 10-bit profile")
	// Handle 8bit flag to override 10bit
	eightBit := flag.Bool("8bit", false, "encode using 8-bit profile")

	flag.DurationVar(&config.FromTime, "from", 0, "start encoding from this time (e.g., 5m30s, 1h30m, 300s)")
	flag.DurationVar(&config.ToTime, "to", 0, "end encoding at this time (e.g., 10m, 1h30m, 420s)")
	flag.DurationVar(&config.Duration, "duration", 0, "encoding duration (e.g., 10m, 1h30m, 420s)")

	// New flags for width and height
	flag.IntVar(&config.Width, "width", 0, "set output video width")
	flag.IntVar(&config.Height, "height", 0, "set output video height")

	flag.BoolVar(&config.Debug, "debug", false, "enable debug output")

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
	if c.Version {
		return nil
	}

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
func generateFilename(filePath string, sourceWidth, sourceHeight, requestedWidth, requestedHeight int) string {
	// Use provided dimensions if available, otherwise use original dimensions
	finalWidth := sourceWidth
	finalHeight := sourceHeight

	if requestedWidth > 0 || requestedHeight > 0 {
		if requestedWidth > 0 && requestedHeight > 0 {
			// Both specified - use exact dimensions
			finalWidth = requestedWidth
			finalHeight = requestedHeight
		} else if requestedWidth > 0 {
			// Only width specified - calculate height maintaining aspect ratio
			aspectRatio := float64(sourceHeight) / float64(sourceWidth)
			finalWidth = requestedWidth
			finalHeight = int(float64(requestedWidth) * aspectRatio)
		} else {
			// Only height specified - calculate width maintaining aspect ratio
			aspectRatio := float64(sourceWidth) / float64(sourceHeight)
			finalHeight = requestedHeight
			finalWidth = int(float64(requestedHeight) * aspectRatio)
		}
	}

	maxLength := max(finalWidth, finalHeight)

	var resolution string
	switch {
	case maxLength >= 3000:
		resolution = "4K"
	case maxLength >= 1900 && maxLength <= 2000:
		resolution = "1080p"
	case maxLength >= 1200 && maxLength <= 1400:
		resolution = "720p"
	}

	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	// Remove existing resolution tags
	re := regexp.MustCompile(`\[\d+[pk]\]`)
	newStem := strings.TrimSpace(re.ReplaceAllString(baseName, ""))

	if resolution != "" {
		newStem = fmt.Sprintf("%s [%s, x265]", newStem, resolution)
	} else {
		newStem = fmt.Sprintf("%s [x265]", newStem)
	}

	ext := filepath.Ext(filePath)

	return newStem + ext
}

func run(ctx context.Context, args cliArgs) error {
	log.Ctx(ctx).Debug().
		Interface("args", args).
		Msg("Starting encoding")

	absPath, err := filepath.Abs(args.VideoPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}
	args.VideoPath = absPath

	log.Ctx(ctx).Debug().Str("resolved_path", args.VideoPath).Msg("Resolved input path")

	if _, err := os.Stat(args.VideoPath); os.IsNotExist(err) {
		return fmt.Errorf("no such file: %s", args.VideoPath)
	}

	probe, err := ffmpeg.Probe(ctx, args.VideoPath)
	if err != nil {
		return fmt.Errorf("failed to probe video: %w", err)
	}

	args.OutputDir = cmp.Or(args.OutputDir, filepath.Dir(args.VideoPath))

	if err := os.MkdirAll(args.OutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	outputFilename := generateFilename(args.VideoPath, probe.Width, probe.Height, args.Width, args.Height)
	savePath := filepath.Join(args.OutputDir, outputFilename)

	// Prevent overwriting the input file
	if args.VideoPath == savePath {
		ext := filepath.Ext(args.VideoPath)
		savePath = strings.TrimSuffix(args.VideoPath, ext) + ".reencoded" + ext
	}

	log.Ctx(ctx).Debug().Str("output_path", savePath).Msg("Final output path determined")

	encodeDuration := args.Duration
	if args.ToTime > 0 {
		encodeDuration = args.ToTime - args.FromTime
		log.Ctx(ctx).Debug().Dur("calculated_duration", encodeDuration).Msg("Calculated duration from to-from")
	}

	if args.Encoder == "ffmpeg" {
		params := ffmpeg.EncodeParams{
			InputPath:  args.VideoPath,
			OutputPath: savePath,
			Quality:    args.Quality,
			Is10Bit:    args.Is10Bit,
			FromTime:   args.FromTime,
			Duration:   encodeDuration,
			Width:      args.Width,
			Height:     args.Height,
			ExtraArgs:  args.ExtraArgs,
		}

		return ffmpeg.Encode(ctx, params, func(p ffmpeg.EncodeProgress) {
			fmt.Printf("\r%s", p.String())
		})
	} else {
		params := handbrake.EncodeParams{
			InputPath:  args.VideoPath,
			OutputPath: savePath,
			Quality:    args.Quality,
			Is10Bit:    args.Is10Bit,
			FromTime:   args.FromTime,
			Duration:   encodeDuration,
			Denoise:    args.Denoise,
			Width:      args.Width,
			Height:     args.Height,
			ExtraArgs:  args.ExtraArgs,
		}

		return handbrake.Encode(ctx, params, func(p handbrake.EncodeProgress) {
			fmt.Printf("\r%s", p.String())
		})
	}
}

func main() {
	args := parseArgs()

	level := zerolog.InfoLevel
	if args.Debug {
		level = zerolog.DebugLevel
	}

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).Level(level)
	zerolog.DefaultContextLogger = &log.Logger

	if err := args.Validate(); err != nil {
		log.Fatal().Err(err).Send()
		return
	}

	if args.Version {
		fmt.Println(version)
		os.Exit(0)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, args); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Info().Msg("Encoding cancelled by user")
			os.Exit(1)
		}
		log.Fatal().Err(err).Msg("Encoding failed")
	}
}

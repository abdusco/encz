# encz

A command-line video encoder that supports both HandBrake and FFmpeg backends for H.265/x265 encoding.

## Features

- Dual encoder support (HandBrake and FFmpeg)
- H.265/x265 encoding with configurable quality
- 8-bit and 10-bit encoding profiles
- Video resizing and cropping
- Time-based encoding (start time, duration, end time)
- Denoise filtering (HandBrake only)
- Progress monitoring
- Automatic filename generation with resolution tags

## Installation

```bash
go build -o encz
```

## Usage

```bash
encz [flags] <video_path> [extra_args...]
```

### Basic Examples

```bash
# Encode with default settings (HandBrake, quality 35, 10-bit)
encz input.mp4

# Use FFmpeg encoder with custom quality
encz -encoder ffmpeg -quality 28 input.mp4

# Encode specific time segment
encz -from 1m30s -duration 5m input.mp4

# Resize video to 1080p width
encz -width 1920 input.mp4

# 8-bit encoding with denoise
encz -8bit -denoise input.mp4
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-encoder` | `handbrake` | Encoder engine (`handbrake` or `ffmpeg`) |
| `-quality` | `35` | x265 quality factor |
| `-output-dir` | `""` | Directory to save encoded files |
| `-10bit` | `true` | Enable 10-bit encoding |
| `-8bit` | `false` | Enable 8-bit encoding (overrides `-10bit`) |
| `-denoise` | `false` | Enable denoise filter (HandBrake only) |
| `-from` | `0` | Start encoding from time (e.g., `5m30s`, `1h30m`) |
| `-to` | `0` | End encoding at time |
| `-duration` | `0` | Encoding duration |
| `-width` | `0` | Output video width |
| `-height` | `0` | Output video height |
| `-debug` | `false` | Enable debug logging |
| `-version` | `false` | Show version information |

### Time Format

Time durations support Go's duration format:
- `30s` - 30 seconds
- `5m30s` - 5 minutes 30 seconds
- `1h30m` - 1 hour 30 minutes

### Output Naming

Files are automatically renamed with resolution and codec tags:
- `movie.mp4` → `movie [1080p, x265].mp4`
- `video.mkv` → `video [4K, x265].mkv`

## Requirements

- Go 1.24+
- HandBrake CLI (`HandBrakeCLI`) or FFmpeg installed and in PATH

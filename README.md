# pmvhaven-dl

Command-line HLS video downloader for PMVHaven written in Go.

## Features

- Extracts HLS streams from video pages and downloads MPEG-TS (`.ts`) segments concurrently.
- Supports resolution selection (`1080`, `720`, `480`, `360`, `best`, `low`).
- Resumes interrupted downloads from cached segment parts.
- Checks existing files via byte-range stream probes before downloading to skip duplicates without re-downloading the full video.

## Usage

```bash
./pmvhaven-dl [flags] <url>
```

### Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-output` | `./downloads` | Directory to save downloaded videos |
| `-quality` | `1080` | Target video quality (`1080`, `720`, `480`, `360`, `best`, `low`) |
| `-threads` | `4` | Number of concurrent segment downloads (`1`–`16`) |
| `-version` | `false` | Print application version and exit |

### Examples

```bash
# Download at default quality (up to 1080p)
./pmvhaven-dl "https://pmvhaven.com/video/example"

# Download at 480p with 6 concurrent workers into a custom directory
./pmvhaven-dl -quality 480 -threads 6 -output ./videos "https://pmvhaven.com/video/example"
```

## Building from Source

Requires Go 1.22+.

```bash
./build.sh
```

Or build directly with `go`:

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o pmvhaven-dl main.go
```

## License

Licensed under the [MIT License](LICENSE). Commercial and non-commercial use, modification, and distribution are permitted provided attribution to **Cat-Ling** is retained.

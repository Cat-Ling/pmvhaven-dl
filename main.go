package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/grafov/m3u8"
)

const (
	maxHTMLBytes     = 16 << 20  // 16 MiB
	maxPlaylistBytes = 8 << 20   // 8 MiB
	maxSegmentBytes  = 512 << 20 // 512 MiB
	probeSampleSize  = 16384     // 16 KiB
	mpegTSPacketSize = 188
	mpegTSSyncByte   = 0x47
	defaultThreads   = 4
	maxThreads       = 16
	maxRetries       = 3
	userAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var (
	version      = "v1.0.0"
	reMasterM3u8 = regexp.MustCompile(`https?://[^"'\s\\<>]*master\.m3u8(?:\?[^"'\s\\<>]*)?`)
	reAnyM3u8    = regexp.MustCompile(`https?://[^"'\s\\<>]*\.m3u8(?:\?[^"'\s\\<>]*)?`)
	reH1         = regexp.MustCompile(`(?is)<h1[^>]*text-xl[^>]*>(.*?)</h1>`)
	reTitle      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reHTMLTags   = regexp.MustCompile(`<[^>]*>`)
	reSiteSuffix = regexp.MustCompile(`(?i)\s*[-|]\s*(?:PMVHaven|Playlist)\s*$`)
	reSlug       = regexp.MustCompile(`/video/([^/?#]+?)(?:_[0-9a-fA-F]{24})?(?:[/?#]|$)`)
	reInvalidChr = regexp.MustCompile(`[<>:"/\\|?*]`)
	reWhitespace = regexp.MustCompile(`\s+`)
	reURIHeight  = regexp.MustCompile(`(?i)(?:^|[_/])(\d{3,4})p\.m3u8`)
)

type progressReader struct {
	io.Reader
	counter *uint64
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.Reader.Read(p)
	if n > 0 {
		atomic.AddUint64(pr.counter, uint64(n))
	}
	return
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatProgressBar(percent float64, length int) string {
	if percent < 0 {
		percent = 0
	}
	filled := int((percent / 100.0) * float64(length))
	if filled > length {
		filled = length
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", length-filled)
}

func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		if ip4[0] == 0 ||
			(ip4[0] == 100 && ip4[1]&0xC0 == 64) ||
			(ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0) ||
			(ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19)) ||
			ip4.Equal(net.IPv4bcast) {
			return true
		}
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

func newHTTPClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   maxThreads,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses found for host %s", host)
			}
			for _, ipAddr := range ips {
				if isDisallowedIP(ipAddr.IP) {
					return nil, fmt.Errorf("refusing connection to non-public IP %s for host %s", ipAddr.IP, host)
				}
			}
			var lastErr error
			for _, ipAddr := range ips {
				target := net.JoinHostPort(ipAddr.IP.String(), port)
				conn, err := dialer.DialContext(ctx, network, target)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}

	client := &http.Client{
		Transport: tr,
		Jar:       jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if _, err := validateRemoteURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}

	return client, nil
}

func validateRemoteURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL %q is missing host", rawURL)
	}
	return u, nil
}

func resolveURL(base *url.URL, ref string) (*url.URL, error) {
	refURL, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, fmt.Errorf("invalid reference URI %q: %w", ref, err)
	}
	resolved := base.ResolveReference(refURL)
	return validateRemoteURL(resolved.String())
}

func retryDelay(resp *http.Response, fallback time.Duration) time.Duration {
	if resp != nil {
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				d := time.Duration(secs) * time.Second
				if d > 30*time.Second {
					return 30 * time.Second
				}
				return d
			}
		}
	}
	return fallback
}

func shouldRetryStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
}

func fetchBytesWithRetry(ctx context.Context, client *http.Client, targetURL, referer string, maxBytes int64, timeout time.Duration) ([]byte, *url.URL, error) {
	waitTime := 2 * time.Second
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if attempt > 1 {
			select {
			case <-time.After(waitTime):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			waitTime *= 2
		}

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			wait := retryDelay(resp, waitTime)
			statusErr := fmt.Errorf("HTTP %s", resp.Status)
			retry := shouldRetryStatus(resp.StatusCode)
			resp.Body.Close()
			cancel()
			if !retry {
				return nil, nil, statusErr
			}
			lastErr = statusErr
			waitTime = wait
			continue
		}

		finalURL := resp.Request.URL
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		resp.Body.Close()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if int64(len(data)) > maxBytes {
			return nil, nil, fmt.Errorf("response body exceeded maximum allowed size of %d bytes", maxBytes)
		}
		return data, finalURL, nil
	}

	return nil, nil, lastErr
}

func decodeMediaPlaylist(data []byte) (*m3u8.MediaPlaylist, error) {
	segCount := uint(bytes.Count(data, []byte("#EXTINF"))) + 16
	if segCount < 1024 {
		segCount = 1024
	}
	pl, err := m3u8.NewMediaPlaylist(segCount, segCount)
	if err != nil {
		return nil, err
	}
	if err := pl.DecodeFrom(bytes.NewReader(data), true); err != nil {
		return nil, err
	}
	return pl, nil
}

func decodeHLSPlaylist(data []byte) (m3u8.Playlist, m3u8.ListType, error) {
	if bytes.Contains(data, []byte("#EXTINF")) {
		pl, err := decodeMediaPlaylist(data)
		if err != nil {
			return nil, 0, err
		}
		return pl, m3u8.MEDIA, nil
	}
	return m3u8.DecodeFrom(bytes.NewReader(data), true)
}

func extractTitle(bodyStr, pageURL string) string {
	title := ""
	if h1Match := reH1.FindStringSubmatch(bodyStr); len(h1Match) > 1 {
		title = h1Match[1]
	} else if titleMatch := reTitle.FindStringSubmatch(bodyStr); len(titleMatch) > 1 {
		title = titleMatch[1]
	}

	if title != "" {
		title = reHTMLTags.ReplaceAllString(title, "")
		title = html.UnescapeString(title)
		title = strings.TrimSpace(title)
		for {
			cleaned := reSiteSuffix.ReplaceAllString(title, "")
			if cleaned == title {
				break
			}
			title = strings.TrimSpace(cleaned)
		}
		if strings.EqualFold(title, "pmvhaven") {
			title = ""
		}
	}

	if title == "" {
		if slugMatch := reSlug.FindStringSubmatch(pageURL); len(slugMatch) > 1 {
			if unescaped, err := url.PathUnescape(slugMatch[1]); err == nil {
				title = unescaped
			} else {
				title = slugMatch[1]
			}
		} else if u, err := url.Parse(pageURL); err == nil {
			base := filepath.Base(u.Path)
			if len(base) > 5 && strings.EqualFold(base[len(base)-5:], ".m3u8") {
				base = base[:len(base)-5]
			}
			if unescaped, err := url.PathUnescape(base); err == nil {
				base = unescaped
			}
			if base != "" && base != "." && base != "/" && !strings.EqualFold(base, "master") && !strings.EqualFold(base, "index") && !strings.EqualFold(base, "playlist") {
				title = base
			}
		}
	}

	if title == "" {
		title = "pmvhaven_video"
	}

	return sanitizeFilename(title)
}

func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)

	name = reInvalidChr.ReplaceAllString(name, "_")
	name = reWhitespace.ReplaceAllString(name, " ")
	name = strings.Trim(name, " .")

	runes := []rune(name)
	if len(runes) > 200 {
		name = strings.Trim(string(runes[:200]), " .")
	}

	if name == "" || name == "." || name == ".." {
		return "pmvhaven_video"
	}

	upper := strings.ToUpper(name)
	switch upper {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		name = "_" + name
	}

	return name
}

func variantHeight(v *m3u8.Variant) int {
	if v == nil {
		return 0
	}

	var shortSide, longSide int
	parts := strings.Split(v.Resolution, "x")
	if len(parts) == 2 {
		w, errW := strconv.Atoi(parts[0])
		h, errH := strconv.Atoi(parts[1])
		if errW == nil && errH == nil && w > 0 && h > 0 {
			if w < h {
				shortSide, longSide = w, h
			} else {
				shortSide, longSide = h, w
			}
		}
	}

	var uriH int
	if m := reURIHeight.FindStringSubmatch(v.URI); len(m) > 1 {
		if h, err := strconv.Atoi(m[1]); err == nil && h > 0 {
			uriH = h
		}
	}

	if shortSide > 0 && uriH > 0 {
		// If URI reflects the long edge of a portrait video (e.g., 1080x1920 named 1920p.m3u8),
		// use the short side (1080). If URI is the nominal tier (e.g., 480p.m3u8 for 854x478), use uriH.
		if uriH == longSide && shortSide < longSide {
			return shortSide
		}
		return uriH
	}
	if shortSide > 0 {
		return shortSide
	}
	return uriH
}

func selectVariant(variants []*m3u8.Variant, qualityFlag string) *m3u8.Variant {
	var valid []*m3u8.Variant
	for _, v := range variants {
		if v != nil && strings.TrimSpace(v.URI) != "" {
			valid = append(valid, v)
		}
	}
	if len(valid) == 0 {
		return nil
	}

	qualityStr := strings.ToLower(strings.TrimSpace(qualityFlag))
	isBest := qualityStr == "best"
	isWorst := qualityStr == "worst" || qualityStr == "low"

	if isBest {
		best := valid[0]
		for _, v := range valid[1:] {
			vH, bH := variantHeight(v), variantHeight(best)
			if vH > bH || (vH == bH && v.Bandwidth > best.Bandwidth) {
				best = v
			}
		}
		return best
	}

	if isWorst {
		worst := valid[0]
		for _, v := range valid[1:] {
			vH, wH := variantHeight(v), variantHeight(worst)
			if vH > 0 && wH > 0 {
				if vH < wH || (vH == wH && v.Bandwidth < worst.Bandwidth) {
					worst = v
				}
			} else if v.Bandwidth < worst.Bandwidth {
				worst = v
			}
		}
		return worst
	}

	targetHeight := 1080
	qStr := strings.TrimSuffix(qualityStr, "p")
	if h, err := strconv.Atoi(qStr); err == nil && h > 0 {
		targetHeight = h
	}

	var bestUnder *m3u8.Variant
	var lowestAbove *m3u8.Variant

	for _, v := range valid {
		h := variantHeight(v)
		if h <= targetHeight {
			if bestUnder == nil {
				bestUnder = v
			} else {
				bH := variantHeight(bestUnder)
				if h > bH || (h == bH && v.Bandwidth > bestUnder.Bandwidth) {
					bestUnder = v
				}
			}
		} else {
			if lowestAbove == nil {
				lowestAbove = v
			} else {
				lH := variantHeight(lowestAbove)
				if h < lH || (h == lH && v.Bandwidth < lowestAbove.Bandwidth) {
					lowestAbove = v
				}
			}
		}
	}

	if bestUnder != nil {
		return bestUnder
	}
	if lowestAbove != nil {
		return lowestAbove
	}
	return valid[0]
}

func fetchRangeSample(ctx context.Context, client *http.Client, segURL *url.URL, referer, rangeHeader string, maxBytes int) ([]byte, int64, error) {
	waitTime := time.Second
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if attempt > 1 {
			select {
			case <-time.After(waitTime):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			waitTime *= 2
		}

		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, segURL.String(), nil)
		if err != nil {
			cancel()
			return nil, 0, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Referer", referer)
		req.Header.Set("Range", rangeHeader)

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			retry := shouldRetryStatus(resp.StatusCode)
			statusErr := fmt.Errorf("unexpected status %d", resp.StatusCode)
			resp.Body.Close()
			cancel()
			if !retry {
				return nil, 0, statusErr
			}
			lastErr = statusErr
			continue
		}

		if strings.HasPrefix(rangeHeader, "bytes=-") && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			cancel()
			return nil, 0, errors.New("server did not honor suffix range request")
		}

		var totalSize int64
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if idx := strings.LastIndex(cr, "/"); idx != -1 {
				if sz, err := strconv.ParseInt(cr[idx+1:], 10, 64); err == nil && sz > 0 {
					totalSize = sz
				}
			}
		} else if resp.ContentLength > 0 {
			totalSize = resp.ContentLength
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
		resp.Body.Close()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if len(data) == 0 {
			lastErr = errors.New("empty range sample")
			continue
		}
		return data, totalSize, nil
	}

	return nil, 0, lastErr
}

func fileMatchesSamples(path string, headSample, tailSample []byte, minExpectedSize int64) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	fileSize := info.Size()
	if fileSize < int64(len(headSample))+int64(len(tailSample)) || fileSize < minExpectedSize {
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	if len(headSample) > 0 {
		buf := make([]byte, len(headSample))
		if _, err := io.ReadFull(f, buf); err != nil || !bytes.Equal(buf, headSample) {
			return false
		}
	}

	if len(tailSample) > 0 {
		buf := make([]byte, len(tailSample))
		if _, err := f.ReadAt(buf, fileSize-int64(len(tailSample))); err != nil || !bytes.Equal(buf, tailSample) {
			return false
		}
	}

	return true
}

func findExistingMatch(ctx context.Context, client *http.Client, existingFiles []string, chunklistURL *url.URL, validSegments []*m3u8.MediaSegment, referer string) (string, error) {
	if len(existingFiles) == 0 || len(validSegments) == 0 {
		return "", nil
	}

	firstSegURL, err := resolveURL(chunklistURL, validSegments[0].URI)
	if err != nil {
		return "", err
	}

	headSample, firstSegSize, err := fetchRangeSample(ctx, client, firstSegURL, referer, fmt.Sprintf("bytes=0-%d", probeSampleSize-1), probeSampleSize)
	if err != nil {
		return "", err
	}

	minExpectedSize := firstSegSize
	var tailSample []byte

	if len(validSegments) > 1 {
		lastSegURL, err := resolveURL(chunklistURL, validSegments[len(validSegments)-1].URI)
		if err != nil {
			return "", err
		}
		sample, lastSegSize, err := fetchRangeSample(ctx, client, lastSegURL, referer, fmt.Sprintf("bytes=-%d", probeSampleSize), probeSampleSize)
		if err != nil {
			return "", err
		}
		tailSample = sample
		minExpectedSize += lastSegSize
	} else if firstSegSize > probeSampleSize {
		sample, _, err := fetchRangeSample(ctx, client, firstSegURL, referer, fmt.Sprintf("bytes=-%d", probeSampleSize), probeSampleSize)
		if err != nil {
			return "", err
		}
		tailSample = sample
	}

	for _, candidate := range existingFiles {
		if fileMatchesSamples(candidate, headSample, tailSample, minExpectedSize) {
			return candidate, nil
		}
	}
	return "", nil
}

func validateMediaPlaylistSupport(mediaPl *m3u8.MediaPlaylist, segments []*m3u8.MediaSegment) error {
	if !mediaPl.Closed {
		return errors.New("live or incomplete HLS playlists (missing #EXT-X-ENDLIST) are not supported")
	}
	if mediaPl.Key != nil && mediaPl.Key.Method != "" && !strings.EqualFold(mediaPl.Key.Method, "NONE") {
		return fmt.Errorf("encrypted HLS streams (%s) are not supported", mediaPl.Key.Method)
	}
	if mediaPl.Map != nil && mediaPl.Map.URI != "" {
		return errors.New("fMP4 initialization segments (#EXT-X-MAP) are not supported")
	}
	for _, seg := range segments {
		if seg.Key != nil && seg.Key.Method != "" && !strings.EqualFold(seg.Key.Method, "NONE") {
			return fmt.Errorf("encrypted HLS segments (%s) are not supported", seg.Key.Method)
		}
		if seg.Map != nil && seg.Map.URI != "" {
			return errors.New("fMP4 initialization segments (#EXT-X-MAP) are not supported")
		}
		if seg.Limit > 0 {
			return errors.New("byte-range HLS segments (#EXT-X-BYTERANGE) are not supported")
		}
	}
	return nil
}

func validateTSSegmentFile(path string) (int64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if stat.IsDir() {
		return 0, errors.New("segment path is a directory")
	}
	size := stat.Size()
	if size < mpegTSPacketSize || size%mpegTSPacketSize != 0 {
		return 0, fmt.Errorf("invalid MPEG-TS segment size %d", size)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var header [1]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, err
	}
	if header[0] != mpegTSSyncByte {
		return 0, fmt.Errorf("missing leading MPEG-TS sync byte (got 0x%02x)", header[0])
	}
	if size > mpegTSPacketSize {
		if _, err := f.ReadAt(header[:], size-mpegTSPacketSize); err != nil {
			return 0, err
		}
		if header[0] != mpegTSSyncByte {
			return 0, fmt.Errorf("missing trailing MPEG-TS sync byte (got 0x%02x)", header[0])
		}
	}
	return size, nil
}

func main() {
	outDir := flag.String("output", "./downloads", "Directory to save downloads")
	quality := flag.String("quality", "1080", "Video quality (e.g., 1080, 720, 480, best, low)")
	threads := flag.Int("threads", defaultThreads, "Number of concurrent segment downloads (1-16)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("pmvhaven-dl %s\n", version)
		return
	}

	args := flag.Args()
	if len(args) < 1 {
		log.Fatalf("Usage: pmvhaven-dl [flags] <url> (version %s)", version)
	}

	pageURL, err := validateRemoteURL(args[0])
	if err != nil {
		log.Fatalf("Invalid URL: %v", err)
	}
	urlStr := pageURL.String()

	workerCount := *threads
	if workerCount < 1 {
		workerCount = 1
	} else if workerCount > maxThreads {
		workerCount = maxThreads
	}

	cleanOutDir := filepath.Clean(*outDir)
	if err := os.MkdirAll(cleanOutDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	go func() {
		select {
		case <-sigChan:
			fmt.Println("\nAborting download gracefully...")
			cancel()
		case <-ctx.Done():
		}
	}()

	client, err := newHTTPClient()
	if err != nil {
		log.Fatalf("Failed to initialize HTTP client: %v", err)
	}

	var m3u8URL *url.URL
	var baseTitle string

	if strings.HasSuffix(strings.ToLower(pageURL.Path), ".m3u8") {
		m3u8URL = pageURL
		baseTitle = extractTitle("", urlStr)
	} else {
		bodyBytes, finalPageURL, err := fetchBytesWithRetry(ctx, client, urlStr, "", maxHTMLBytes, 30*time.Second)
		if err != nil {
			log.Fatalf("Failed to fetch page: %v", err)
		}
		if finalPageURL != nil {
			pageURL = finalPageURL
			urlStr = pageURL.String()
		}

		bodyStr := string(bodyBytes)
		normalizedStr := strings.NewReplacer(
			`\u002F`, "/",
			`\u002f`, "/",
			`\/`, "/",
			`\u0026`, "&",
			`&amp;`, "&",
		).Replace(bodyStr)

		m3u8Raw := reMasterM3u8.FindString(normalizedStr)
		if m3u8Raw == "" {
			m3u8Raw = reAnyM3u8.FindString(normalizedStr)
		}
		if m3u8Raw == "" {
			log.Fatalf("Could not find .m3u8 stream URL in the page source.")
		}

		m3u8URL, err = validateRemoteURL(m3u8Raw)
		if err != nil {
			log.Fatalf("Invalid m3u8 URL: %v", err)
		}

		baseTitle = extractTitle(bodyStr, urlStr)
	}

	m3u8Bytes, finalM3u8URL, err := fetchBytesWithRetry(ctx, client, m3u8URL.String(), urlStr, maxPlaylistBytes, 30*time.Second)
	if err != nil {
		log.Fatalf("Failed to fetch m3u8 playlist: %v", err)
	}
	if finalM3u8URL != nil {
		m3u8URL = finalM3u8URL
	}

	playlist, listType, err := decodeHLSPlaylist(m3u8Bytes)
	if err != nil {
		log.Fatalf("Failed to decode m3u8 playlist: %v", err)
	}

	var mediaPl *m3u8.MediaPlaylist
	var chunklistURL *url.URL
	selectedRes := "default"

	switch listType {
	case m3u8.MASTER:
		masterPl := playlist.(*m3u8.MasterPlaylist)
		bestVariant := selectVariant(masterPl.Variants, *quality)
		if bestVariant == nil {
			log.Fatalf("No valid stream variants found in master playlist")
		}
		if bestVariant.Resolution != "" {
			selectedRes = bestVariant.Resolution
		}

		for _, alt := range bestVariant.Alternatives {
			if alt != nil && strings.EqualFold(alt.Type, "AUDIO") && strings.TrimSpace(alt.URI) != "" {
				log.Printf("Warning: selected variant references a separate audio rendition (%s); output .ts may not include audio", alt.URI)
				break
			}
		}

		chunklistURL, err = resolveURL(m3u8URL, bestVariant.URI)
		if err != nil {
			log.Fatalf("Failed to resolve chunklist URL: %v", err)
		}

		chunkBytes, finalChunkURL, err := fetchBytesWithRetry(ctx, client, chunklistURL.String(), urlStr, maxPlaylistBytes, 30*time.Second)
		if err != nil {
			log.Fatalf("Failed to fetch chunklist: %v", err)
		}
		if finalChunkURL != nil {
			chunklistURL = finalChunkURL
		}

		mediaPl, err = decodeMediaPlaylist(chunkBytes)
		if err != nil {
			log.Fatalf("Failed to decode chunklist: %v", err)
		}

	case m3u8.MEDIA:
		mediaPl = playlist.(*m3u8.MediaPlaylist)
		chunklistURL = m3u8URL

	default:
		log.Fatalf("Unsupported playlist type: %v", listType)
	}

	var validSegments []*m3u8.MediaSegment
	for _, s := range mediaPl.Segments {
		if s != nil && strings.TrimSpace(s.URI) != "" {
			validSegments = append(validSegments, s)
		}
	}
	if len(validSegments) == 0 {
		log.Fatalf("No media segments found in playlist")
	}

	if err := validateMediaPlaylistSupport(mediaPl, validSegments); err != nil {
		log.Fatalf("Unsupported HLS stream format: %v", err)
	}

	var existingFiles []string
	title := baseTitle
	outputFile := filepath.Join(cleanOutDir, title+".ts")
	for counter := 1; ; counter++ {
		info, err := os.Stat(outputFile)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			log.Fatalf("Failed to inspect output path %s: %v", outputFile, err)
		}
		if !info.IsDir() && info.Size() > 0 {
			existingFiles = append(existingFiles, outputFile)
		}
		title = fmt.Sprintf("%s_%d", baseTitle, counter)
		outputFile = filepath.Join(cleanOutDir, title+".ts")
	}

	if len(existingFiles) > 0 {
		match, err := findExistingMatch(ctx, client, existingFiles, chunklistURL, validSegments, urlStr)
		if ctx.Err() != nil {
			log.Fatalf("\nDownload aborted.")
		}
		if err != nil {
			log.Printf("Warning: could not verify existing files (%v); proceeding with new file %s", err, outputFile)
		} else if match != "" {
			fmt.Printf("Already downloaded: %s\n", match)
			return
		}
	}

	stableStreamKey := strings.ToLower(chunklistURL.Host) + chunklistURL.Path
	streamSum := sha256.Sum256([]byte(stableStreamKey))
	streamID := hex.EncodeToString(streamSum[:4])
	partsDir := filepath.Join(cleanOutDir, fmt.Sprintf(".%s_%s_parts", baseTitle, streamID))
	if err := os.MkdirAll(partsDir, 0755); err != nil {
		log.Fatalf("Failed to create temporary parts directory: %v", err)
	}

	fmt.Printf("Downloading: %s\n", title)
	fmt.Printf("Downloading %d segments (Resolution: %s)...\n", len(validSegments), selectedRes)

	var completedChunks int32
	var totalDownloadedBytes uint64
	var activeWorkers int32

	jobs := make(chan int, len(validSegments))
	for i := range validSegments {
		partFile := filepath.Join(partsDir, fmt.Sprintf("%05d.ts", i))
		if size, err := validateTSSegmentFile(partFile); err == nil {
			atomic.AddInt32(&completedChunks, 1)
			atomic.AddUint64(&totalDownloadedBytes, uint64(size))
			continue
		} else if !os.IsNotExist(err) {
			_ = os.Remove(partFile)
		}
		jobs <- i
	}
	close(jobs)

	pendingCount := len(jobs)
	if pendingCount < workerCount {
		workerCount = pendingCount
	}

	uiCtx, uiCancel := context.WithCancel(context.Background())
	var uiWg sync.WaitGroup
	uiWg.Add(1)
	go func(initialBytes uint64) {
		defer uiWg.Done()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		lastBytes := initialBytes
		lastTime := time.Now()

		for {
			select {
			case <-uiCtx.Done():
				return
			case t := <-ticker.C:
				currentBytes := atomic.LoadUint64(&totalDownloadedBytes)
				completed := atomic.LoadInt32(&completedChunks)
				workers := atomic.LoadInt32(&activeWorkers)

				duration := t.Sub(lastTime).Seconds()
				var speed float64
				if duration > 0 && currentBytes >= lastBytes {
					speed = float64(currentBytes-lastBytes) / duration
				}

				lastBytes = currentBytes
				lastTime = t

				percent := float64(completed) / float64(len(validSegments)) * 100

				fmt.Printf("\r\033[K[%s] %.1f%% (%d/%d parts) | %s | %s/s | Workers: %d",
					formatProgressBar(percent, 20),
					percent, completed, len(validSegments),
					formatBytes(currentBytes),
					formatBytes(uint64(speed)),
					workers,
				)
			}
		}
	}(atomic.LoadUint64(&totalDownloadedBytes))

	downloadCtx, downloadCancel := context.WithCancel(ctx)
	defer downloadCancel()

	var wg sync.WaitGroup
	errChan := make(chan error, workerCount+1)

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if downloadCtx.Err() != nil {
					return
				}

				seg := validSegments[i]
				segURL, err := resolveURL(chunklistURL, seg.URI)
				if err != nil {
					select {
					case errChan <- fmt.Errorf("segment %d invalid URL: %w", i, err):
					default:
					}
					downloadCancel()
					return
				}

				partFile := filepath.Join(partsDir, fmt.Sprintf("%05d.ts", i))
				tmpPartFile := partFile + ".tmp"

				var lastErr error
				waitTime := 2 * time.Second

				atomic.AddInt32(&activeWorkers, 1)
				for attempt := 1; attempt <= maxRetries; attempt++ {
					if downloadCtx.Err() != nil {
						lastErr = downloadCtx.Err()
						break
					}

					if attempt > 1 {
						atomic.AddInt32(&activeWorkers, -1)
						select {
						case <-time.After(waitTime):
						case <-downloadCtx.Done():
							atomic.AddInt32(&activeWorkers, 1)
							lastErr = downloadCtx.Err()
							break
						}
						atomic.AddInt32(&activeWorkers, 1)
						waitTime *= 2
					}

					segCtx, segCancel := context.WithTimeout(downloadCtx, 90*time.Second)
					reqSeg, err := http.NewRequestWithContext(segCtx, http.MethodGet, segURL.String(), nil)
					if err != nil {
						segCancel()
						lastErr = err
						continue
					}
					reqSeg.Header.Set("User-Agent", userAgent)
					reqSeg.Header.Set("Referer", urlStr)
					reqSeg.Header.Set("Accept-Encoding", "identity")

					respSeg, err := client.Do(reqSeg)
					if err != nil {
						segCancel()
						lastErr = err
						continue
					}

					if respSeg.StatusCode != http.StatusOK {
						wait := retryDelay(respSeg, waitTime)
						retry := shouldRetryStatus(respSeg.StatusCode)
						statusErr := fmt.Errorf("unexpected HTTP status %d", respSeg.StatusCode)
						respSeg.Body.Close()
						segCancel()
						lastErr = statusErr
						if !retry {
							break
						}
						waitTime = wait
						continue
					}

					expectedLen := respSeg.ContentLength

					out, err := os.OpenFile(tmpPartFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
					if err != nil {
						respSeg.Body.Close()
						segCancel()
						lastErr = err
						continue
					}

					pr := &progressReader{
						Reader:  io.LimitReader(respSeg.Body, maxSegmentBytes+1),
						counter: &totalDownloadedBytes,
					}
					written, copyErr := io.Copy(out, pr)
					syncErr := out.Sync()
					closeErr := out.Close()
					respSeg.Body.Close()
					segCancel()

					var segErr error
					switch {
					case copyErr != nil:
						segErr = copyErr
					case syncErr != nil:
						segErr = syncErr
					case closeErr != nil:
						segErr = closeErr
					case written > maxSegmentBytes:
						segErr = fmt.Errorf("segment exceeded maximum size of %d bytes", maxSegmentBytes)
					case expectedLen > 0 && written != expectedLen:
						segErr = fmt.Errorf("incomplete segment read (%d of %d bytes)", written, expectedLen)
					default:
						_, segErr = validateTSSegmentFile(tmpPartFile)
					}

					if segErr != nil {
						if written > 0 {
							atomic.AddUint64(&totalDownloadedBytes, ^uint64(written-1))
						}
						_ = os.Remove(tmpPartFile)
						lastErr = segErr
						continue
					}

					if err := os.Rename(tmpPartFile, partFile); err != nil {
						if written > 0 {
							atomic.AddUint64(&totalDownloadedBytes, ^uint64(written-1))
						}
						_ = os.Remove(tmpPartFile)
						lastErr = err
						continue
					}

					lastErr = nil
					break
				}
				atomic.AddInt32(&activeWorkers, -1)

				if lastErr != nil {
					if ctx.Err() == nil {
						select {
						case errChan <- fmt.Errorf("segment %d failed: %w", i, lastErr):
						default:
						}
					}
					downloadCancel()
					return
				}

				atomic.AddInt32(&completedChunks, 1)
			}
		}()
	}

	wg.Wait()
	uiCancel()
	uiWg.Wait()
	close(errChan)

	if ctx.Err() != nil {
		log.Fatalf("\nDownload aborted.")
	}

	if err, ok := <-errChan; ok && err != nil {
		log.Fatalf("\nDownload failed: %v", err)
	}

	currentBytes := atomic.LoadUint64(&totalDownloadedBytes)
	fmt.Printf("\r\033[K[%s] 100.0%% (%d/%d parts) | %s | 0 B/s | Workers: 0\n",
		formatProgressBar(100.0, 20),
		len(validSegments), len(validSegments),
		formatBytes(currentBytes),
	)

	fmt.Printf("Stitching segments into %s...\n", outputFile)

	tmpOutputFile := outputFile + ".tmp"
	finalOut, err := os.OpenFile(tmpOutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatalf("Failed to create temporary output file: %v", err)
	}

	for i := range validSegments {
		if ctx.Err() != nil {
			_ = finalOut.Close()
			_ = os.Remove(tmpOutputFile)
			log.Fatalf("\nStitching aborted.")
		}

		partPath := filepath.Join(partsDir, fmt.Sprintf("%05d.ts", i))
		expectedSize, err := validateTSSegmentFile(partPath)
		if err != nil {
			_ = finalOut.Close()
			_ = os.Remove(tmpOutputFile)
			log.Fatalf("Invalid segment part %s: %v", partPath, err)
		}

		in, err := os.Open(partPath)
		if err != nil {
			_ = finalOut.Close()
			_ = os.Remove(tmpOutputFile)
			log.Fatalf("Failed to open part %s: %v", partPath, err)
		}

		copied, copyErr := io.Copy(finalOut, in)
		closeErr := in.Close()
		if copyErr != nil || closeErr != nil || copied != expectedSize {
			_ = finalOut.Close()
			_ = os.Remove(tmpOutputFile)
			if copyErr != nil {
				log.Fatalf("Failed to write part %s: %v", partPath, copyErr)
			}
			if closeErr != nil {
				log.Fatalf("Failed to close part %s: %v", partPath, closeErr)
			}
			log.Fatalf("Short write for part %s (%d of %d bytes)", partPath, copied, expectedSize)
		}
	}

	if err := finalOut.Sync(); err != nil {
		_ = finalOut.Close()
		_ = os.Remove(tmpOutputFile)
		log.Fatalf("Failed to flush output file to disk: %v", err)
	}

	if err := finalOut.Close(); err != nil {
		_ = os.Remove(tmpOutputFile)
		log.Fatalf("Failed to close output file: %v", err)
	}

	if err := os.Rename(tmpOutputFile, outputFile); err != nil {
		_ = os.Remove(tmpOutputFile)
		log.Fatalf("Failed to move completed file into place: %v", err)
	}

	if err := os.RemoveAll(partsDir); err != nil {
		log.Printf("Warning: failed to remove temporary parts directory %s: %v", partsDir, err)
	}

	fmt.Printf("Success: %s saved to %s\n", title, outputFile)
}

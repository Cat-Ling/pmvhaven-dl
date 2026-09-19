package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
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

	"github.com/grafov/m3u8"
)

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

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
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatProgressBar(percent float64, length int) string {
	filled := int((percent / 100.0) * float64(length))
	if filled > length {
		filled = length
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", length-filled)
	return bar
}

type SavedCookie struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

const cookieFile = "cookie.json"

func loadCookies() map[string]SavedCookie {
	cookies := make(map[string]SavedCookie)
	data, err := os.ReadFile(cookieFile)
	if err == nil {
		var saved []SavedCookie
		if err := json.Unmarshal(data, &saved); err == nil {
			now := time.Now()
			for _, c := range saved {
				if c.Expires.IsZero() || c.Expires.After(now) {
					cookies[c.Name] = c
				}
			}
		}
	}
	return cookies
}

func saveCookies(cookies map[string]SavedCookie) {
	var saved []SavedCookie
	for _, c := range cookies {
		saved = append(saved, c)
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err == nil {
		os.WriteFile(cookieFile, data, 0644)
	}
}

func main() {
	outDir := flag.String("output", "./downloads", "Directory to save downloads")
	quality := flag.String("quality", "1080", "Video quality (e.g., 1080, 720, best, low)")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		log.Fatalf("Usage: pmvhaven-dl-exp [flags] <url>")
	}
	urlStr := args[0]

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	userAgent := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	cookies := loadCookies()

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		log.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("User-Agent", userAgent)

	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}

	// Use a transport with sensible dial/TLS timeouts but no overall client timeout.
	// This prevents hanging on initial connection, while allowing the body to take as long as needed.
	tr := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
	}
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Failed to fetch URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Expected HTTP 200, got %s", resp.Status)
	}

	for _, c := range resp.Cookies() {
		expires := c.Expires
		if expires.IsZero() {
			expires = time.Now().Add(24 * time.Hour)
		}
		cookies[c.Name] = SavedCookie{
			Name:    c.Name,
			Value:   c.Value,
			Expires: expires,
		}
	}
	saveCookies(cookies)

	var cookieParts []string
	for _, c := range cookies {
		cookieParts = append(cookieParts, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	cookieString := strings.Join(cookieParts, "; ")

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response body: %v", err)
	}

	bodyStr := strings.ReplaceAll(string(bodyBytes), "\\u002F", "/")

	reM3u8 := regexp.MustCompile(`https?://[^"'\s]*master\.m3u8`)
	m3u8Url := reM3u8.FindString(bodyStr)
	if m3u8Url == "" {
		log.Fatalf("Could not find master.m3u8 in the page source.")
	}

	title := ""
	
	// First try to grab the title from the h1 above the video (this supports playlist pages)
	reH1 := regexp.MustCompile(`(?i)<h1[^>]*text-xl md:text-2xl font-bold text-white[^>]*>(.*?)</h1>`)
	if h1Match := reH1.FindStringSubmatch(bodyStr); len(h1Match) > 1 {
		title = strings.TrimSpace(h1Match[1])
	} else {
		// Fallback to page <title>
		reTitle := regexp.MustCompile(`(?i)<title>(.*?)</title>`)
		if titleMatch := reTitle.FindStringSubmatch(bodyStr); len(titleMatch) > 1 {
			title = strings.TrimSpace(titleMatch[1])
		}
	}

	if title != "" {
		reSite := regexp.MustCompile(`(?i)\s*[-|]\s*PMVHaven\s*$`)
		title = reSite.ReplaceAllString(title, "")
		
		rePlaylist := regexp.MustCompile(`(?i)\s*[-|]\s*Playlist\s*$`)
		title = rePlaylist.ReplaceAllString(title, "")

		if strings.ToLower(title) == "pmvhaven" {
			title = ""
		}
	}

	if title == "" {
		reSlug := regexp.MustCompile(`/video/([^_?]+)`)
		slugMatch := reSlug.FindStringSubmatch(urlStr)
		if len(slugMatch) > 1 {
			title = slugMatch[1]
		} else {
			title = "pmvhaven_video"
		}
	}

	reInvalid := regexp.MustCompile(`[<>:"/\\|?*]`)
	title = reInvalid.ReplaceAllString(title, "_")
	title = strings.TrimSpace(title)

	runes := []rune(title)
	if len(runes) > 200 {
		title = string(runes[:200])
	}
	title = strings.TrimSpace(title)

	baseTitle := title
	outputFile := filepath.Join(*outDir, title+".ts")
	counter := 0
	for {
		if _, err := os.Stat(outputFile); os.IsNotExist(err) {
			break
		}
		counter++
		title = fmt.Sprintf("%s_%d", baseTitle, counter)
		outputFile = filepath.Join(*outDir, title+".ts")
	}

	fmt.Printf("Downloading: %s\n", title)

	// Fetch Master Playlist
	reqM3u8, _ := http.NewRequest("GET", m3u8Url, nil)
	reqM3u8.Header.Set("User-Agent", userAgent)
	reqM3u8.Header.Set("Cookie", cookieString)
	reqM3u8.Header.Set("Referer", urlStr)

	respM3u8, err := client.Do(reqM3u8)
	if err != nil {
		log.Fatalf("Failed to fetch master m3u8: %v", err)
	}
	defer respM3u8.Body.Close()

	playlist, listType, err := m3u8.DecodeFrom(respM3u8.Body, true)
	if err != nil {
		log.Fatalf("Failed to decode master m3u8: %v", err)
	}

	if listType != m3u8.MASTER {
		log.Fatalf("Expected a master playlist, got %v", listType)
	}
	masterPl := playlist.(*m3u8.MasterPlaylist)

	// Determine quality
	qualityStr := strings.ToLower(strings.TrimSpace(*quality))
	targetHeight := 1080
	isBest := qualityStr == "best"
	isWorst := qualityStr == "worst" || qualityStr == "low"
	if !isBest && !isWorst {
		qStr := strings.TrimSuffix(qualityStr, "p")
		if h, err := strconv.Atoi(qStr); err == nil {
			targetHeight = h
		}
	}

	var bestVariant *m3u8.Variant
	for _, variant := range masterPl.Variants {
		if isBest {
			if bestVariant == nil || variant.Resolution != "" && (getRes(variant.Resolution) > getRes(bestVariant.Resolution) || variant.Bandwidth > bestVariant.Bandwidth) {
				bestVariant = variant
			}
		} else if isWorst {
			if bestVariant == nil || variant.Resolution != "" && (getRes(variant.Resolution) < getRes(bestVariant.Resolution) || variant.Bandwidth < bestVariant.Bandwidth) {
				bestVariant = variant
			}
		} else {
			h := getRes(variant.Resolution)
			// we want highest <= targetHeight
			if h <= targetHeight {
				if bestVariant == nil || getRes(bestVariant.Resolution) < h || (getRes(bestVariant.Resolution) == h && variant.Bandwidth > bestVariant.Bandwidth) {
					bestVariant = variant
				}
			}
		}
	}

	// fallback if target height not met
	if bestVariant == nil && len(masterPl.Variants) > 0 {
		bestVariant = masterPl.Variants[0]
	}

	if bestVariant == nil {
		log.Fatalf("No variants found in master playlist")
	}

	chunklistUrl, _ := url.Parse(m3u8Url)
	chunklistRef, _ := url.Parse(bestVariant.URI)
	chunklistUrl = chunklistUrl.ResolveReference(chunklistRef)

	reqChunklist, _ := http.NewRequest("GET", chunklistUrl.String(), nil)
	reqChunklist.Header.Set("User-Agent", userAgent)
	reqChunklist.Header.Set("Cookie", cookieString)
	reqChunklist.Header.Set("Referer", urlStr)

	respChunklist, err := client.Do(reqChunklist)
	if err != nil {
		log.Fatalf("Failed to fetch chunklist: %v", err)
	}
	defer respChunklist.Body.Close()

	chunklist, chunkType, err := m3u8.DecodeFrom(respChunklist.Body, true)
	if err != nil {
		log.Fatalf("Failed to decode chunklist: %v", err)
	}
	if chunkType != m3u8.MEDIA {
		log.Fatalf("Expected media playlist, got %v", chunkType)
	}

	mediaPl := chunklist.(*m3u8.MediaPlaylist)
	segments := mediaPl.Segments
	var validSegments []*m3u8.MediaSegment
	for _, s := range segments {
		if s != nil {
			validSegments = append(validSegments, s)
		}
	}

	fmt.Printf("Downloading %d segments (Resolution: %s)...\n", len(validSegments), bestVariant.Resolution)

	partsDir := filepath.Join(*outDir, "."+title+"_parts")
	os.MkdirAll(partsDir, 0755)

	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nAborting download gracefully...")
		cancel()
	}()

	var wg sync.WaitGroup
	errChan := make(chan error, len(validSegments))

	var downloadedSegments []string
	for i, _ := range validSegments {
		downloadedSegments = append(downloadedSegments, fmt.Sprintf("%05d.ts", i))
	}

	var completedChunks int32
	var totalDownloadedBytes uint64

	// Dynamic Concurrency (AIMD Congestion Control)
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	activeWorkers := 0
	targetConcurrency := 3 // start with 3 workers
	maxWorkers := 15

	acquireWorker := func() {
		mu.Lock()
		for activeWorkers >= targetConcurrency {
			cond.Wait()
		}
		activeWorkers++
		mu.Unlock()
	}

	releaseWorker := func() {
		mu.Lock()
		activeWorkers--
		cond.Signal()
		mu.Unlock()
	}

	decreaseConcurrency := func() {
		mu.Lock()
		targetConcurrency = targetConcurrency / 2
		if targetConcurrency < 1 {
			targetConcurrency = 1
		}
		mu.Unlock()
	}

	// AIMD Probing: Slowly increase concurrency if no errors
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				if targetConcurrency < maxWorkers {
					targetConcurrency++
					cond.Broadcast()
				}
				mu.Unlock()
			}
		}
	}()

	uiCtx, uiCancel := context.WithCancel(context.Background())
	// Smooth UI Ticker
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		var lastBytes uint64
		lastTime := time.Now()

		for {
			select {
			case <-uiCtx.Done():
				return
			case t := <-ticker.C:
				currentBytes := atomic.LoadUint64(&totalDownloadedBytes)
				completed := atomic.LoadInt32(&completedChunks)

				duration := t.Sub(lastTime).Seconds()
				speed := float64(currentBytes-lastBytes) / duration

				lastBytes = currentBytes
				lastTime = t

				percent := float64(completed) / float64(len(validSegments)) * 100
				if len(validSegments) == 0 {
					percent = 0
				}

				mu.Lock()
				workers := targetConcurrency
				mu.Unlock()

				fmt.Printf("\r\033[K[%s] %.1f%% (%d/%d parts) | %s | %s/s | Workers: %d",
					formatProgressBar(percent, 20),
					percent, completed, len(validSegments),
					formatBytes(currentBytes),
					formatBytes(uint64(speed)),
					workers,
				)
			}
		}
	}()

	for i, seg := range validSegments {
		wg.Add(1)
		go func(i int, seg *m3u8.MediaSegment) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			default:
			}

			acquireWorker()
			defer releaseWorker()

			partFile := filepath.Join(partsDir, fmt.Sprintf("%05d.ts", i))
			
			if stat, err := os.Stat(partFile); err == nil && stat.Size() > 0 {
				atomic.AddInt32(&completedChunks, 1)
				// optionally add stat.Size() to totalDownloadedBytes if we want to reflect resumed bytes
				// atomic.AddUint64(&totalDownloadedBytes, uint64(stat.Size()))
				return
			}

			segUrl, _ := url.Parse(chunklistUrl.String())
			segRef, _ := url.Parse(seg.URI)
			segUrl = segUrl.ResolveReference(segRef)

			var lastErr error
			waitTime := 2 * time.Second

			for attempt := 1; attempt <= 3; attempt++ {
				if ctx.Err() != nil {
					return
				}

				segCtx, segCancel := context.WithTimeout(ctx, 5*time.Minute)
				
				reqSeg, _ := http.NewRequestWithContext(segCtx, "GET", segUrl.String(), nil)
				reqSeg.Header.Set("User-Agent", userAgent)
				reqSeg.Header.Set("Cookie", cookieString)
				reqSeg.Header.Set("Referer", urlStr)

				respSeg, err := client.Do(reqSeg)
				if err != nil {
					segCancel()
					lastErr = err
					decreaseConcurrency()
					time.Sleep(waitTime)
					waitTime *= 2
					continue
				}

				if respSeg.StatusCode != 200 {
					respSeg.Body.Close()
					segCancel()
					lastErr = fmt.Errorf("unexpected status %d", respSeg.StatusCode)
					if respSeg.StatusCode == 429 {
						waitTime = waitTime * 2
					}
					decreaseConcurrency()
					time.Sleep(waitTime)
					waitTime *= 2
					continue
				}

				out, err := os.Create(partFile + ".tmp")
				if err != nil {
					respSeg.Body.Close()
					segCancel()
					lastErr = err
					time.Sleep(waitTime)
					continue
				}
				
				pr := &progressReader{Reader: respSeg.Body, counter: &totalDownloadedBytes}
				_, err = io.Copy(out, pr)
				
				out.Close()
				respSeg.Body.Close()
				segCancel()

				if err != nil {
					os.Remove(partFile + ".tmp")
					lastErr = err
					decreaseConcurrency()
					time.Sleep(waitTime)
					waitTime *= 2
					continue
				}

				os.Rename(partFile+".tmp", partFile)
				lastErr = nil
				break
			}

			if lastErr != nil {
				errChan <- fmt.Errorf("segment %d failed after 3 attempts: %v", i, lastErr)
				return
			}

			atomic.AddInt32(&completedChunks, 1)
		}(i, seg)
	}

	wg.Wait()
	uiCancel() // Stop the UI ticker
	
	// Force a final 100% render for a polished UX
	currentBytes := atomic.LoadUint64(&totalDownloadedBytes)
	fmt.Printf("\r\033[K[%s] 100.0%% (%d/%d parts) | %s | 0 B/s | Workers: 0\n",
		formatProgressBar(100.0, 20),
		len(validSegments), len(validSegments),
		formatBytes(currentBytes),
	)

	close(errChan)

	if ctx.Err() != nil {
		log.Fatalf("\nDownload aborted.")
	}

	var errs []error
	for e := range errChan {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		log.Fatalf("\nEncountered %d errors during download. First error: %v", len(errs), errs[0])
	}

	fmt.Printf("Stitching segments into %s...\n", outputFile)
	
	finalOut, err := os.Create(outputFile)
	if err != nil {
		log.Fatalf("Failed to create final file: %v", err)
	}
	defer finalOut.Close()

	for _, partName := range downloadedSegments {
		partPath := filepath.Join(partsDir, partName)
		in, err := os.Open(partPath)
		if err != nil {
			log.Fatalf("Failed to read part %s: %v", partPath, err)
		}
		io.Copy(finalOut, in)
		in.Close()
	}

	os.RemoveAll(partsDir)
	fmt.Printf("\nSuccess: %s saved to %s\n", title, outputFile)

	// Post-download byte-for-byte deduplication
	if counter > 0 {
		fmt.Printf("Comparing hash with previous downloads to check for exact duplicates...\n")
		newHash, err := hashFile(outputFile)
		if err == nil {
			isDuplicate := false
			duplicateOf := ""
			
			for c := 0; c < counter; c++ {
				prevFile := filepath.Join(*outDir, baseTitle+".ts")
				if c > 0 {
					prevFile = filepath.Join(*outDir, fmt.Sprintf("%s_%d.ts", baseTitle, c))
				}
				
				if prevHash, err := hashFile(prevFile); err == nil {
					if prevHash == newHash {
						isDuplicate = true
						duplicateOf = prevFile
						break
					}
				}
			}

			if isDuplicate {
				fmt.Printf("File is an exact byte-for-byte duplicate of %s. Removing %s...\n", duplicateOf, outputFile)
				os.Remove(outputFile)
			}
		}
	}
}

func getRes(resStr string) int {
	parts := strings.Split(resStr, "x")
	if len(parts) == 2 {
		if h, err := strconv.Atoi(parts[1]); err == nil {
			return h
		}
	}
	return 0
}

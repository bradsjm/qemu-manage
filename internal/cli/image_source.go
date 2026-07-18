package cli

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/therootcompany/xz"
)

const (
	downloadedImageFilename    = ".image-source"
	maxXZDictionaryBytes       = 64 << 20
	imageDialTimeout           = 30 * time.Second
	imageTLSHandshakeTimeout   = 15 * time.Second
	imageResponseHeaderTimeout = 30 * time.Second
	imageBodyIdleTimeout       = 2 * time.Minute
	imageMaxRedirects          = 10
)

var errImageBodyIdle = errors.New("image download made no progress for 2 minutes")

type imageCompression uint8

const (
	imageUncompressed imageCompression = iota
	imageXZ
	imageGzip
)

type imageSourceSpec struct {
	localPath   string
	remoteURL   *url.URL
	compression imageCompression
}

func parseImageSource(raw string) (imageSourceSpec, error) {
	if raw == "" {
		return imageSourceSpec{}, nil
	}
	if !strings.Contains(raw, "://") {
		return imageSourceSpec{localPath: raw}, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return imageSourceSpec{}, errors.New("invalid image URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return imageSourceSpec{}, fmt.Errorf("unsupported URL scheme %q; use a local path, http, or https", parsed.Scheme)
	}
	if parsed.Host == "" || parsed.Opaque != "" {
		return imageSourceSpec{}, errors.New("HTTP URL must be absolute and include a host")
	}

	compression := imageUncompressed
	switch strings.ToLower(filepath.Ext(parsed.Path)) {
	case ".xz":
		compression = imageXZ
	case ".gz":
		compression = imageGzip
	}
	return imageSourceSpec{remoteURL: parsed, compression: compression}, nil
}

func newImageHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: imageDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.TLSHandshakeTimeout = imageTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = imageResponseHeaderTimeout
	transport.DisableCompression = true
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= imageMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", imageMaxRedirects)
			}
			if request.URL.Scheme != "http" && request.URL.Scheme != "https" || request.URL.Host == "" {
				return errors.New("redirect target must be an absolute HTTP(S) URL")
			}
			return nil
		},
	}
}

func (a *App) materializeImage(ctx context.Context, source imageSourceSpec, vmDir string, progress io.Writer) (path string, temporary bool, err error) {
	if source.remoteURL == nil {
		if err := requireRegularSource(source.localPath); err != nil {
			return "", false, fmt.Errorf("source image: %w", err)
		}
		return source.localPath, false, nil
	}

	label := publicURL(source.remoteURL)
	action := "Downloading"
	if source.compression != imageUncompressed {
		action = "Downloading and decompressing"
	}
	if _, err := fmt.Fprintf(progress, "%s image from %s\n", action, label); err != nil {
		return "", false, fmt.Errorf("report image download: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.remoteURL.String(), nil)
	if err != nil {
		return "", false, fmt.Errorf("create image request for %s", label)
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "qemu-manage/1")
	client := a.HTTPClient
	if client == nil {
		client = newImageHTTPClient()
	}
	response, err := client.Do(request)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return "", false, fmt.Errorf("download image from %s: %w", label, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", false, fmt.Errorf("download image from %s: HTTP %d %s", label, response.StatusCode, http.StatusText(response.StatusCode))
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", false, fmt.Errorf("download image from %s: unsupported HTTP Content-Encoding %q", label, encoding)
	}

	guardedBody := newImageIdleReader(ctx, response.Body)
	defer guardedBody.Stop()
	reader, closeReader, err := decompressedImageReader(guardedBody, source.compression)
	if err != nil {
		return "", false, fmt.Errorf("decompress image from %s: %w", label, err)
	}
	if closeReader != nil {
		defer closeReader()
	}

	destination := filepath.Join(vmDir, downloadedImageFilename)
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, fmt.Errorf("create downloaded image: %w", err)
	}
	committed := false
	defer func() {
		_ = output.Close()
		if !committed {
			_ = os.Remove(destination)
		}
	}()
	if err := output.Chmod(0o600); err != nil {
		return "", false, fmt.Errorf("protect downloaded image: %w", err)
	}
	_, copyErr := io.Copy(output, reader)
	guardedBody.Stop()
	if copyErr != nil {
		return "", false, fmt.Errorf("write downloaded image: %w", copyErr)
	}
	if err := output.Sync(); err != nil {
		return "", false, fmt.Errorf("sync downloaded image: %w", err)
	}
	if err := output.Close(); err != nil {
		return "", false, fmt.Errorf("close downloaded image: %w", err)
	}
	committed = true
	return destination, true, nil
}

type imageIdleReader struct {
	ctx      context.Context
	reader   io.Reader
	body     io.Closer
	progress chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	failure  error
}

func newImageIdleReader(ctx context.Context, body io.ReadCloser) *imageIdleReader {
	reader := &imageIdleReader{
		ctx:      ctx,
		reader:   body,
		body:     body,
		progress: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go reader.watch()
	return reader
}

func (r *imageIdleReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		select {
		case r.progress <- struct{}{}:
		default:
		}
	}
	if err != nil {
		r.mu.Lock()
		failure := r.failure
		r.mu.Unlock()
		if failure != nil {
			return n, failure
		}
	}
	return n, err
}

func (r *imageIdleReader) Stop() {
	r.stopOnce.Do(func() {
		close(r.done)
	})
}

func (r *imageIdleReader) watch() {
	timer := time.NewTimer(imageBodyIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-r.progress:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(imageBodyIdleTimeout)
		case <-r.ctx.Done():
			r.fail(context.Cause(r.ctx))
			return
		case <-timer.C:
			r.fail(errImageBodyIdle)
			return
		case <-r.done:
			return
		}
	}
}

func (r *imageIdleReader) fail(err error) {
	r.mu.Lock()
	r.failure = err
	r.mu.Unlock()
	_ = r.body.Close()
}

func decompressedImageReader(input io.Reader, compression imageCompression) (io.Reader, func() error, error) {
	switch compression {
	case imageUncompressed:
		return input, nil, nil
	case imageXZ:
		reader, err := xz.NewReader(input, maxXZDictionaryBytes)
		return reader, nil, err
	case imageGzip:
		reader, err := gzip.NewReader(input)
		if err != nil {
			return nil, nil, err
		}
		return reader, reader.Close, nil
	default:
		return nil, nil, errors.New("unsupported image compression")
	}
}

func publicURL(source *url.URL) string {
	return source.Scheme + "://" + source.Host + "/..."
}

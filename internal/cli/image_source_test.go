package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/therootcompany/xz"
)

func TestParseImageSourcePreservesPathsAndValidatesURLs(t *testing.T) {
	for _, test := range []struct {
		name            string
		source          string
		wantLocal       string
		wantCompression imageCompression
		wantError       string
	}{
		{name: "ordinary path", source: "/images/disk.qcow2", wantLocal: "/images/disk.qcow2"},
		{name: "colon path", source: "disk:copy.qcow2", wantLocal: "disk:copy.qcow2"},
		{name: "uppercase suffix", source: "HTTPS://example.com/image.QCOW2.XZ?token=secret", wantCompression: imageXZ},
		{name: "HTTP-prefixed local path", source: "https:image.qcow2", wantLocal: "https:image.qcow2"},
		{name: "file URL", source: "file:///tmp/image.qcow2", wantError: "unsupported URL scheme"},
		{name: "FTP URL", source: "ftp://example.com/image.qcow2", wantError: "unsupported URL scheme"},
		{name: "missing host", source: "https:///image.qcow2", wantError: "include a host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseImageSource(test.source)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("parse error = %v, want it to contain %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.localPath != test.wantLocal || got.compression != test.wantCompression {
				t.Fatalf("parsed source = %+v, want local=%q compression=%d", got, test.wantLocal, test.wantCompression)
			}
		})
	}
}

func TestRedirectErrorDoesNotExposeURLSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", "ftp://redirect-user:redirect-pass@example.com/private-token?redirect-query=secret")
		response.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	source, err := parseImageSource(server.URL + "/initial-token.qcow2?initial-query=secret#initial-fragment")
	if err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	_, _, err = (&App{HTTPClient: newImageHTTPClient()}).materializeImage(context.Background(), source, t.TempDir(), &progress, true, false)
	if err == nil {
		t.Fatal("redirect to FTP unexpectedly succeeded")
	}
	output := progress.String() + err.Error()
	for _, secret := range []string{"initial-token", "initial-query", "initial-fragment", "redirect-user", "redirect-pass", "private-token", "redirect-query"} {
		if strings.Contains(output, secret) {
			t.Errorf("output exposed %q: %q", secret, output)
		}
	}
}

func TestCreateCancellationRollsBackPartialDownload(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("partial image"))
		response.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	a := testApp(t)
	a.HTTPClient = server.Client()
	firmwareCode, firmwareVars, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (string, string) { return firmwareCode, firmwareVars }
	a.RunExternal = func(_ context.Context, _ string, _ []string) error {
		t.Fatal("qemu-img ran after download cancellation")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		exit   int
		stderr string
	}
	done := make(chan runResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		exit := a.Run(ctx, []string{
			"create", "cancelled-download",
			"--image", server.URL + "/image.qcow2",
			"--qemu", qemuPath,
			"--qemu-img", qemuImgPath,
		}, strings.NewReader(""), &stdout, &stderr)
		done <- runResult{exit: exit, stderr: stderr.String()}
	}()
	<-started
	cancel()
	result := <-done
	if result.exit != 1 || !strings.Contains(result.stderr, context.Canceled.Error()) {
		t.Fatalf("create exit=%d stderr=%q", result.exit, result.stderr)
	}
	if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "cancelled-download")); !os.IsNotExist(err) {
		t.Fatalf("cancelled download left VM directory: %v", err)
	}
	for _, root := range []string{a.Store.RuntimeRoot, a.Store.LogRoot} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("cancelled download left entries in %s: %v", root, entries)
		}
	}
}

type blockingImageBody struct {
	started chan struct{}
	closed  chan struct{}
}

func (b *blockingImageBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, os.ErrClosed
}

func (b *blockingImageBody) Close() error {
	close(b.closed)
	return nil
}

func TestCompressedHeaderReadHonorsCancellation(t *testing.T) {
	for _, compression := range []imageCompression{imageXZ, imageGzip} {
		t.Run(fmt.Sprintf("compression-%d", compression), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			body := &blockingImageBody{started: make(chan struct{}), closed: make(chan struct{})}
			done := make(chan error, 1)
			guardedBody := newImageIdleReader(ctx, body)
			defer guardedBody.Stop()
			go func() {
				_, _, err := decompressedImageReader(guardedBody, compression)
				done <- err
			}()
			<-body.started
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("compressed header error = %v, want context cancellation", err)
			}
		})
	}
}

func TestMaterializeRemoteImageCompression(t *testing.T) {
	payload := []byte("source qcow2 fixture")
	for _, test := range []struct {
		name   string
		suffix string
		body   func(*testing.T, []byte) []byte
	}{
		{name: "plain", suffix: ".qcow2", body: func(_ *testing.T, data []byte) []byte { return data }},
		{name: "xz", suffix: ".qcow2.xz", body: xzFixture},
		{name: "gzip", suffix: ".qcow2.gz", body: gzipFixture},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := test.body(t, payload)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("Accept-Encoding = %q, want identity", request.Header.Get("Accept-Encoding"))
				}
				_, _ = response.Write(body)
			}))
			defer server.Close()

			source, err := parseImageSource(server.URL + "/image" + test.suffix + "?token=secret")
			if err != nil {
				t.Fatal(err)
			}
			var progress strings.Builder
			path, temporary, err := (&App{HTTPClient: server.Client()}).materializeImage(context.Background(), source, t.TempDir(), &progress, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if !temporary {
				t.Fatal("remote image was not marked temporary")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("materialized image = %q, want %q", got, payload)
			}
			if strings.Contains(progress.String(), "secret") {
				t.Fatalf("progress exposed URL query: %q", progress.String())
			}
		})
	}
}

func TestCreateDownloadsXZImageAndRemovesTemporarySource(t *testing.T) {
	payload := []byte("source qcow2 fixture")
	archive := xzFixture(t, payload)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()

	a := testApp(t)
	a.HTTPClient = server.Client()
	firmwareCode, firmwareVars, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (string, string) { return firmwareCode, firmwareVars }
	converted := false
	a.RunExternal = func(_ context.Context, _ string, args []string) error {
		if len(args) != 5 || args[0] != "convert" || args[1] != "-O" || args[2] != "qcow2" {
			return fmt.Errorf("unexpected qemu-img arguments: %v", args)
		}
		source, err := os.ReadFile(args[3])
		if err != nil {
			return err
		}
		if !bytes.Equal(source, payload) {
			return fmt.Errorf("qemu-img source = %q, want %q", source, payload)
		}
		converted = true
		return writeQcow2Fixture(args[4], 1<<30)
	}

	exit, _, stderr := runCLI(
		a,
		"create", "home-assistant",
		"--image", server.URL+"/haos_generic-aarch64.qcow2.xz?token=secret",
		"--qemu", qemuPath,
		"--qemu-img", qemuImgPath,
		"--disk-size", "1GiB",
	)
	if exit != 0 {
		t.Fatalf("create exited %d: %s", exit, stderr)
	}
	if !converted {
		t.Fatal("qemu-img conversion was not invoked")
	}
	if strings.Contains(stderr, "secret") || !strings.Contains(stderr, "Downloading and decompressing") {
		t.Fatalf("unexpected progress output: %q", stderr)
	}
	if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "home-assistant", downloadedImageFilename)); !os.IsNotExist(err) {
		t.Fatalf("temporary source remains after create: %v", err)
	}
	if _, err := a.Store.Load("home-assistant"); err != nil {
		t.Fatalf("load imported VM: %v", err)
	}
}

func TestCreateURLFailureRollsBackVMAndRedactsQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "upstream failed", http.StatusBadGateway)
	}))
	defer server.Close()

	a := testApp(t)
	a.HTTPClient = server.Client()
	firmwareCode, firmwareVars, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (string, string) { return firmwareCode, firmwareVars }
	a.RunExternal = func(_ context.Context, _ string, _ []string) error {
		t.Fatal("qemu-img ran after download failure")
		return nil
	}

	exit, _, stderr := runCLI(
		a,
		"create", "failed-download",
		"--image", server.URL+"/image.qcow2?token=secret",
		"--qemu", qemuPath,
		"--qemu-img", qemuImgPath,
	)
	if exit != 1 || !strings.Contains(stderr, "HTTP 502 Bad Gateway") {
		t.Fatalf("create exit=%d stderr=%q", exit, stderr)
	}
	if strings.Contains(stderr, "secret") {
		t.Fatalf("download error exposed URL query: %q", stderr)
	}
	if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "failed-download")); !os.IsNotExist(err) {
		t.Fatalf("failed download left VM directory: %v", err)
	}
}

func TestCreateRejectsOversizedXZDictionaryAndRollsBack(t *testing.T) {
	archive, err := base64.StdEncoding.DecodeString("/Td6WFoAAATm1rRGBMAFASEBHgAAAAAAAAAAAPDCvp4BAAB4AAAAAEWu74P47hYKAAEhAV6QHtsftvN9AQAAAAAEWVo=")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()

	a := testApp(t)
	a.HTTPClient = server.Client()
	firmwareCode, firmwareVars, qemuPath, qemuImgPath := createInputs(t)
	a.DiscoverFirmware = func() (string, string) { return firmwareCode, firmwareVars }
	a.RunExternal = func(_ context.Context, _ string, _ []string) error {
		t.Fatal("qemu-img ran for an image exceeding the XZ dictionary limit")
		return nil
	}

	exit, _, stderr := runCLI(
		a,
		"create", "oversized-xz",
		"--image", server.URL+"/image.qcow2.xz",
		"--qemu", qemuPath,
		"--qemu-img", qemuImgPath,
	)
	if exit != 1 || !strings.Contains(stderr, xz.ErrMemlimit.Error()) {
		t.Fatalf("create exit=%d stderr=%q", exit, stderr)
	}
	if _, err := os.Lstat(filepath.Join(a.Store.DataRoot, "oversized-xz")); !os.IsNotExist(err) {
		t.Fatalf("rejected XZ image left VM directory: %v", err)
	}
}

func TestXZDecoderEnforcesDictionaryLimit(t *testing.T) {
	archive, err := base64.StdEncoding.DecodeString("/Td6WFoAAATm1rRGBMAFASEBHgAAAAAAAAAAAPDCvp4BAAB4AAAAAEWu74P47hYKAAEhAV6QHtsftvN9AQAAAAAEWVo=")
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := decompressedImageReader(bytes.NewReader(archive), imageXZ)
	if err == nil {
		_, err = io.ReadAll(reader)
	}
	if !errors.Is(err, xz.ErrMemlimit) {
		t.Fatalf("oversized XZ dictionary error = %v, want %v", err, xz.ErrMemlimit)
	}
}

func xzFixture(t *testing.T, payload []byte) []byte {
	t.Helper()
	if string(payload) != "source qcow2 fixture" {
		t.Fatalf("XZ fixture requested for unexpected payload %q", payload)
	}
	archive, err := base64.StdEncoding.DecodeString("/Td6WFoAAATm1rRGBMAYFCEBFgAAAAAAAAAAAPrbCfUBABNzb3VyY2UgcWNvdzIgZml4dHVyZQBkNM2xnyh4pwABNBShknaBH7bzfQEAAAAABFla")
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func gzipFixture(t *testing.T, payload []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := gzip.NewWriter(&archive)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func writeQcow2Fixture(path string, virtualSize uint64) error {
	var header [32]byte
	copy(header[:4], "QFI\xfb")
	binary.BigEndian.PutUint32(header[4:8], 3)
	binary.BigEndian.PutUint64(header[24:32], virtualSize)
	return os.WriteFile(path, header[:], 0o600)
}

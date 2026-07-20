package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type countingByteProgress struct {
	total int
}

func (p *countingByteProgress) Add(count int) {
	p.total += count
}

func TestImageProgressReaderCountsNetworkBytesBeforeDecompression(t *testing.T) {
	payload := []byte("source qcow2 fixture")
	for _, test := range []struct {
		name        string
		compression imageCompression
		body        []byte
	}{
		{name: "uncompressed", compression: imageUncompressed, body: payload},
		{name: "gzip", compression: imageGzip, body: gzipFixture(t, payload)},
		{name: "xz", compression: imageXZ, body: xzFixture(t, payload)},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := &countingByteProgress{}
			reader, closeReader, err := decompressedImageReader(
				imageProgressReader{reader: bytes.NewReader(test.body), tracker: tracker},
				test.compression,
			)
			if err != nil {
				t.Fatal(err)
			}
			if closeReader != nil {
				defer func() {
					if err := closeReader(); err != nil {
						t.Fatalf("closeReader: %v", err)
					}
				}()
			}

			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("decompressed payload = %q, want %q", got, payload)
			}
			if tracker.total != len(test.body) {
				t.Fatalf("tracked %d network bytes, want %d", tracker.total, len(test.body))
			}
		})
	}
}

func TestWithByteProgressUsesIndeterminateModeForUnknownLength(t *testing.T) {
	var output bytes.Buffer
	called := false
	err := withByteProgress(
		&output,
		true,
		true,
		"Downloading image for home-assistant VM from https://example.com/...",
		"Downloaded image for home-assistant VM",
		0,
		func(progress byteProgress) error {
			called = true
			if progress != nil {
				t.Fatal("unknown content length unexpectedly received byte progress")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("operation was not called")
	}
	if output.Len() == 0 {
		t.Fatal("unknown-length progress produced no output")
	}
}

func TestMaterializeImageDisabledProgressIsSilent(t *testing.T) {
	payload := []byte("source qcow2 fixture")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Length", "20")
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	source, err := parseImageSource(server.URL + "/image.qcow2")
	if err != nil {
		t.Fatal(err)
	}

	var progress bytes.Buffer
	path, temporary, err := (&App{HTTPClient: server.Client()}).materializeImage(
		context.Background(),
		"home-assistant",
		source,
		t.TempDir(),
		&progress,
		false,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !temporary {
		t.Fatal("remote image was not marked temporary")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, payload) {
		t.Fatalf("materialized image = %q, want %q", saved, payload)
	}
	if progress.Len() != 0 {
		t.Fatalf("disabled progress wrote %q", progress.String())
	}
}

func TestMaterializeImageUnknownLengthCompletesWithoutLeakingSecrets(t *testing.T) {
	payload := []byte("source qcow2 fixture")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(payload[:8])
		response.(http.Flusher).Flush()
		_, _ = response.Write(payload[8:])
	}))
	defer server.Close()

	source, err := parseImageSource(server.URL + "/image.qcow2?token=secret#fragment")
	if err != nil {
		t.Fatal(err)
	}

	var progress bytes.Buffer
	path, temporary, err := (&App{HTTPClient: server.Client()}).materializeImage(
		context.Background(),
		"home-assistant",
		source,
		t.TempDir(),
		&progress,
		true,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !temporary {
		t.Fatal("remote image was not marked temporary")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, payload) {
		t.Fatalf("materialized image = %q, want %q", saved, payload)
	}
	if progress.Len() == 0 {
		t.Fatal("unknown-length progress produced no output")
	}
	if strings.Contains(progress.String(), "secret") || strings.Contains(progress.String(), "fragment") {
		t.Fatalf("progress exposed URL secret data: %q", progress.String())
	}
}

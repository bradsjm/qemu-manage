package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaterializeImageProgressUsesNetworkBytesAndDisabledSilence(t *testing.T) {
	payload := []byte("source qcow2 fixture")
	cases := []struct {
		name   string
		suffix string
		body   []byte
	}{
		{name: "uncompressed", suffix: ".qcow2", body: payload},
		{name: "compressed", suffix: ".qcow2.gz", body: gzipFixture(t, payload)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Length", fmt.Sprintf("%d", len(test.body)))
				_, _ = response.Write(test.body)
			}))
			defer server.Close()

			source, err := parseImageSource(server.URL + "/image" + test.suffix)
			if err != nil {
				t.Fatal(err)
			}
			var progress bytes.Buffer
			if _, _, err := (&App{HTTPClient: server.Client()}).materializeImage(context.Background(), source, t.TempDir(), &progress, true, true); err != nil {
				t.Fatal(err)
			}
			got := progress.String()
			if !strings.Contains(got, "complete") || !strings.Contains(got, fmt.Sprintf("%dB", len(test.body))) {
				t.Fatalf("progress=%q, want completed network-byte record for %d bytes", got, len(test.body))
			}

			progress.Reset()
			if _, _, err := (&App{HTTPClient: server.Client()}).materializeImage(context.Background(), source, t.TempDir(), &progress, false, false); err != nil {
				t.Fatal(err)
			}
			if progress.Len() != 0 {
				t.Fatalf("disabled progress wrote %q", progress.String())
			}
		})
	}
}

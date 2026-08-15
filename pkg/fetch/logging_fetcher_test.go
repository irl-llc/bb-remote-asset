package fetch_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	remoteasset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"github.com/buildbarn/bb-remote-asset/pkg/fetch"
	"github.com/buildbarn/bb-remote-asset/pkg/qualifier"
	"github.com/stretchr/testify/require"

	protostatus "google.golang.org/genproto/googleapis/rpc/status"
)

// The token bazel forwards under
// --experimental_remote_downloader_propagate_credentials. No part of it
// may appear in the log, in any qualifier, on any code path.
const sentinelToken = "Bearer ya29.SENTINEL-ACCESS-TOKEN"

// echoFetcher is a fetcher that succeeds without doing any I/O, so the
// logging fetcher is the only thing under test.
type echoFetcher struct{}

func (echoFetcher) FetchBlob(ctx context.Context, req *remoteasset.FetchBlobRequest) (*remoteasset.FetchBlobResponse, error) {
	return &remoteasset.FetchBlobResponse{Uri: req.Uris[0], Status: &protostatus.Status{}}, nil
}

func (echoFetcher) FetchDirectory(ctx context.Context, req *remoteasset.FetchDirectoryRequest) (*remoteasset.FetchDirectoryResponse, error) {
	return &remoteasset.FetchDirectoryResponse{Uri: req.Uris[0], Status: &protostatus.Status{}}, nil
}

func (echoFetcher) CheckQualifiers(qualifiers qualifier.Set) qualifier.Set {
	return qualifier.Set{}
}

// captureLog redirects the standard logger for the duration of f and
// returns everything it wrote.
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)
	f()
	return buf.String()
}

func credentialQualifiers() []*remoteasset.Qualifier {
	return []*remoteasset.Qualifier{
		{Name: "http_header_url:0:Authorization", Value: sentinelToken},
		{Name: "http_header:Authorization", Value: sentinelToken},
		{Name: "bazel.auth_headers", Value: `{"https://example.com/":{"Authorization":"` + sentinelToken + `"}}`},
		{Name: "checksum.sri", Value: "sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="},
	}
}

func TestLoggingFetcherFetchBlobRedactsCredentials(t *testing.T) {
	output := captureLog(t, func() {
		_, err := fetch.NewLoggingFetcher(echoFetcher{}).FetchBlob(
			context.Background(),
			&remoteasset.FetchBlobRequest{
				Uris:       []string{"https://example.com/artifact.tar.gz"},
				Qualifiers: credentialQualifiers(),
			})
		require.NoError(t, err)
	})

	require.NotContains(t, output, "ya29.")
	require.NotContains(t, output, sentinelToken)

	// The line still has to be worth logging: the URI, the names of the
	// headers that were applied, and the checksum all survive.
	require.Contains(t, output, "https://example.com/artifact.tar.gz")
	require.Contains(t, output, "http_header_url:0:Authorization")
	require.Contains(t, output, "http_header:Authorization")
	require.Contains(t, output, "bazel.auth_headers")
	require.Contains(t, output, "sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=")
	require.Equal(t, 3, strings.Count(output, fetch.RedactedQualifierValue))
}

func TestLoggingFetcherFetchDirectoryRedactsCredentials(t *testing.T) {
	output := captureLog(t, func() {
		_, err := fetch.NewLoggingFetcher(echoFetcher{}).FetchDirectory(
			context.Background(),
			&remoteasset.FetchDirectoryRequest{
				Uris:       []string{"https://example.com/tree"},
				Qualifiers: credentialQualifiers(),
			})
		require.NoError(t, err)
	})

	require.NotContains(t, output, "ya29.")
	require.Contains(t, output, "https://example.com/tree")
	require.Contains(t, output, "http_header_url:0:Authorization")
}

// Redaction must not mutate the request the inner fetcher goes on to
// use, or the header would be redacted out of the actual HTTP fetch.
func TestLoggingFetcherLeavesRequestQualifiersIntact(t *testing.T) {
	req := &remoteasset.FetchBlobRequest{
		Uris:       []string{"https://example.com/artifact.tar.gz"},
		Qualifiers: credentialQualifiers(),
	}
	captureLog(t, func() {
		_, err := fetch.NewLoggingFetcher(echoFetcher{}).FetchBlob(context.Background(), req)
		require.NoError(t, err)
	})

	require.Equal(t, sentinelToken, req.Qualifiers[0].Value)
	require.Equal(t, sentinelToken, req.Qualifiers[1].Value)
}

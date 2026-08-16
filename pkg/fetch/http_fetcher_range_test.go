package fetch_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/buildbarn/bb-remote-asset/pkg/fetch"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/blobstore/slicing"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"

	remoteasset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// apkSizeBytes, controlRange and dataRange are taken from a real entry in
// one of this organisation's apko lock files -- wolfi-baselayout
// 20230201-r28. rules_apko fetches an apk as three concatenated gzip
// segments, each by byte range from the same URL, because the signature
// segment has no stable checksum and so the whole .apk cannot be pinned.
const (
	apkSizeBytes = 13894
	controlRange = "bytes=0-1874"
	dataRange    = "bytes=1875-13893"
	// probeRange is what rules_apko's _check_initial_setup sends before
	// fetching any package. It hard-fails the build unless exactly one
	// byte comes back, which is why a rejected range is a broken build
	// rather than a cache miss.
	probeRange = "bytes=0-0"
)

// maxTestBlobSizeBytes bounds the reads a test makes out of the CAS. It is
// far above anything these tests store.
const maxTestBlobSizeBytes = 1 << 20

// memoryCAS is a content addressable store holding blobs in memory. It
// stands in for the cluster's CAS so a test can read back the exact bytes
// a fetch stored: for a ranged fetch that is the property that matters,
// since the wrong bytes under a key naming a slice are served to every
// later client that asks for that slice.
type memoryCAS struct {
	lock  sync.Mutex
	blobs map[digest.Digest][]byte
}

func newMemoryCAS() *memoryCAS {
	return &memoryCAS{blobs: map[digest.Digest][]byte{}}
}

func (c *memoryCAS) Put(ctx context.Context, blobDigest digest.Digest, b buffer.Buffer) error {
	data, err := b.ToByteSlice(maxTestBlobSizeBytes)
	if err != nil {
		return err
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.blobs[blobDigest] = data
	return nil
}

func (c *memoryCAS) Get(ctx context.Context, blobDigest digest.Digest) buffer.Buffer {
	c.lock.Lock()
	defer c.lock.Unlock()
	data, ok := c.blobs[blobDigest]
	if !ok {
		return buffer.NewBufferFromError(status.Errorf(codes.NotFound, "Blob %s not found", blobDigest))
	}
	return buffer.NewValidatedBufferFromByteSlice(data)
}

func (c *memoryCAS) GetFromComposite(ctx context.Context, parentDigest, childDigest digest.Digest, slicer slicing.BlobSlicer) buffer.Buffer {
	return buffer.NewBufferFromError(status.Error(codes.Unimplemented, "The fetcher does not read composite blobs"))
}

func (c *memoryCAS) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	missing := digest.NewSetBuilder()
	for _, blobDigest := range digests.Items() {
		if _, ok := c.blobs[blobDigest]; !ok {
			missing.Add(blobDigest)
		}
	}
	return missing.Build(), nil
}

func (c *memoryCAS) GetCapabilities(ctx context.Context, instanceName digest.InstanceName) (*remoteexecution.ServerCapabilities, error) {
	return &remoteexecution.ServerCapabilities{}, nil
}

func (c *memoryCAS) blobCount() int {
	c.lock.Lock()
	defer c.lock.Unlock()
	return len(c.blobs)
}

// storedBlob returns the bytes a completed fetch put into the CAS.
func (c *memoryCAS) storedBlob(t *testing.T, response *remoteasset.FetchBlobResponse) []byte {
	t.Helper()

	blobDigest, err := testDigestFunction(t).NewDigestFromProto(response.BlobDigest)
	require.NoError(t, err)

	c.lock.Lock()
	defer c.lock.Unlock()
	data, ok := c.blobs[blobDigest]
	require.True(t, ok, "the fetch reported digest %s, which was never stored", blobDigest)
	return data
}

func testDigestFunction(t *testing.T) digest.Function {
	t.Helper()
	digestFunction, err := util.Must(digest.NewInstanceName("")).GetDigestFunction(remoteexecution.DigestFunction_SHA256, 0)
	require.NoError(t, err)
	return digestFunction
}

// apkLikeContent returns a representation to serve ranges out of. The fill
// is pseudo-random and deterministic so that no slice of it can compare
// equal to a different slice, which is what makes an assertion on the
// stored bytes meaningful.
func apkLikeContent() []byte {
	content := make([]byte, apkSizeBytes)
	rand.New(rand.NewSource(20260816)).Read(content)
	return content
}

// newRangeServingOrigin serves content the way an object store does, with
// the RFC-correct range handling of http.ServeContent. packages.wolfi.dev
// answers the same way: measured 2026-08-16 against a real .apk,
// "Range: bytes=0-0" returns 206 and "Content-Range: bytes 0-0/13894",
// and a range running past the end is clipped rather than refused.
func newRangeServingOrigin(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "package.apk", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(server.Close)
	return server
}

// newMisbehavingOrigin serves whatever handler describes, for the origins
// that answer a range request with something other than the bytes asked
// for.
func newMisbehavingOrigin(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func rangeQualifiers(rangeHeader string, extra ...*remoteasset.Qualifier) []*remoteasset.Qualifier {
	// bazel emits repository_ctx.download(headers = ...) as http_header:*
	// qualifiers whenever the remote downloader is enabled, and
	// removeVolatileQualifiers keeps them, so the range below is part of
	// the asset cache key.
	return append([]*remoteasset.Qualifier{{Name: "http_header:Range", Value: rangeHeader}}, extra...)
}

func fetchBlob(ctx context.Context, cas *memoryCAS, uri string, qualifiers []*remoteasset.Qualifier) (*remoteasset.FetchBlobResponse, error) {
	return fetch.NewHTTPFetcher(http.DefaultClient, cas).FetchBlob(ctx, &remoteasset.FetchBlobRequest{
		InstanceName:   "",
		Uris:           []string{uri},
		Qualifiers:     qualifiers,
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	})
}

func checksumSri(t *testing.T, content []byte) *remoteasset.Qualifier {
	t.Helper()
	sum := sha256.Sum256(content)
	return &remoteasset.Qualifier{
		Name:  "checksum.sri",
		Value: "sha256-" + base64.StdEncoding.EncodeToString(sum[:]),
	}
}

// A ranged fetch must store exactly the bytes the range names. Before this
// was handled, every one of these returned NOT_FOUND, because a 206 was
// treated as a failed request.
func TestHTTPFetcherFetchBlobServesRangedFetches(t *testing.T) {
	content := apkLikeContent()
	origin := newRangeServingOrigin(t, content)
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		rangeHeader string
		want        []byte
	}{
		{"InitialSetupProbe", probeRange, content[0:1]},
		{"ControlSegment", controlRange, content[0:1875]},
		{"DataSegment", dataRange, content[1875:13894]},
		{"OpenEndedRange", "bytes=1875-", content[1875:]},
		{"SuffixRange", "bytes=-100", content[apkSizeBytes-100:]},
		// The origin clips a range running past the end of the file
		// rather than refusing it, and the clipped bytes are the bytes
		// that range names.
		{"RangePastEndOfFile", "bytes=0-999999", content},
		// An apk signature segment has no range, and bazel sends an empty
		// header for it. The origin answers 200 with the whole object.
		{"EmptySignatureRange", "", content},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cas := newMemoryCAS()

			response, err := fetchBlob(ctx, cas, origin.URL, rangeQualifiers(tc.rangeHeader))
			require.NoError(t, err)
			require.Equal(t, int32(codes.OK), response.Status.Code)
			require.Equal(t, tc.want, cas.storedBlob(t, response))
			require.Equal(t, int64(len(tc.want)), response.BlobDigest.SizeBytes)
		})
	}
}

// A header name is case-insensitive, and http.Header.Set canonicalises it,
// so http_header:range puts the very same Range on the wire as
// http_header:Range. Matching the qualifier name exactly would refuse
// every fetch a rule spelled that way.
func TestHTTPFetcherFetchBlobMatchesTheRangeQualifierWithoutRegardToCase(t *testing.T) {
	content := apkLikeContent()
	cas := newMemoryCAS()
	origin := newRangeServingOrigin(t, content)

	response, err := fetchBlob(context.Background(), cas, origin.URL, []*remoteasset.Qualifier{
		{Name: "http_header:range", Value: controlRange},
	})
	require.NoError(t, err)
	require.Equal(t, content[0:1875], cas.storedBlob(t, response))
}

// Two ranges of one URL are two different assets. They key apart because
// the range stays in the cache key, and they must resolve to different
// blobs.
func TestHTTPFetcherFetchBlobKeepsRangesDistinct(t *testing.T) {
	content := apkLikeContent()
	origin := newRangeServingOrigin(t, content)
	ctx := context.Background()
	cas := newMemoryCAS()

	control, err := fetchBlob(ctx, cas, origin.URL, rangeQualifiers(controlRange))
	require.NoError(t, err)
	data, err := fetchBlob(ctx, cas, origin.URL, rangeQualifiers(dataRange))
	require.NoError(t, err)

	require.NotEqual(t, control.BlobDigest.Hash, data.BlobDigest.Hash)
	require.Equal(t, content[0:1875], cas.storedBlob(t, control))
	require.Equal(t, content[1875:13894], cas.storedBlob(t, data))
	require.Equal(t, 2, cas.blobCount())
}

// rules_apko pins the control and data segments individually, so the
// checksum has to be computed over the slice rather than the whole object.
func TestHTTPFetcherFetchBlobVerifiesChecksumOfRangedFetch(t *testing.T) {
	content := apkLikeContent()
	origin := newRangeServingOrigin(t, content)
	ctx := context.Background()

	t.Run("Matching", func(t *testing.T) {
		cas := newMemoryCAS()
		response, err := fetchBlob(ctx, cas, origin.URL, rangeQualifiers(controlRange, checksumSri(t, content[0:1875])))
		require.NoError(t, err)
		require.Equal(t, content[0:1875], cas.storedBlob(t, response))
	})

	t.Run("PinnedToADifferentSlice", func(t *testing.T) {
		cas := newMemoryCAS()
		_, err := fetchBlob(ctx, cas, origin.URL, rangeQualifiers(controlRange, checksumSri(t, content[1875:13894])))
		require.Equal(t, codes.Internal, status.Code(err))
		require.Contains(t, err.Error(), "did not match checksum.sri qualifier")
		require.Zero(t, cas.blobCount())
	})
}

// An origin that answers a slice request with anything other than that
// slice must not reach the CAS. Whatever is stored is served to every
// later client that asks for the same slice, so a wrong body here is a
// poisoned cache entry rather than one bad fetch.
func TestHTTPFetcherFetchBlobRefusesMisservedRanges(t *testing.T) {
	content := apkLikeContent()
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		rangeHeader string
		handler     http.HandlerFunc
		wantMessage string
	}{
		{
			// The failure mode of an origin with no range support: it
			// serves the whole representation, which is a superset of the
			// slice that was asked for.
			name:        "OriginIgnoresRange",
			rangeHeader: controlRange,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(content)))
				w.WriteHeader(http.StatusOK)
				w.Write(content)
			},
			wantMessage: "returned the whole representation rather than the requested range",
		},
		{
			name:        "OriginServesADifferentRange",
			rangeHeader: dataRange,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-99/%d", len(content)))
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusPartialContent)
				w.Write(content[0:100])
			},
			wantMessage: "Origin served bytes 0-99",
		},
		{
			// A 206 answering a multi-range request describes its ranges
			// inside a multipart body, so the body is not the asset.
			name:        "OriginOmitsContentRange",
			rangeHeader: controlRange,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusPartialContent)
				w.Write(content[0:1875])
			},
			wantMessage: "uninterpretable Content-Range",
		},
		{
			// The Content-Range is the one that was asked for, but fewer
			// bytes than it names arrive.
			name:        "OriginBodyIsShorterThanItsContentRange",
			rangeHeader: controlRange,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-1874/%d", len(content)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(content[0:500])
			},
			wantMessage: "Origin served 500 bytes for a range it said was 1875 bytes long",
		},
		{
			name:        "PartialResponseToAnUnrangedRequest",
			rangeHeader: "",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-99/%d", len(content)))
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusPartialContent)
				w.Write(content[0:100])
			},
			wantMessage: "asked for no byte range",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cas := newMemoryCAS()
			origin := newMisbehavingOrigin(t, tc.handler)

			_, err := fetchBlob(ctx, cas, origin.URL, rangeQualifiers(tc.rangeHeader))
			require.Equal(t, codes.NotFound, status.Code(err))
			require.Contains(t, err.Error(), tc.wantMessage)
			require.Zero(t, cas.blobCount())
		})
	}
}

// An origin with no range support answers 200 with the whole
// representation. That is still the content that was asked for when the
// range covered all of it, and refusing it would cost the cache every
// fetch bazel resumes from the start.
func TestHTTPFetcherFetchBlobAcceptsWholeRepresentationForACoveringRange(t *testing.T) {
	content := apkLikeContent()
	cas := newMemoryCAS()
	origin := newMisbehavingOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	})

	response, err := fetchBlob(context.Background(), cas, origin.URL, rangeQualifiers("bytes=0-"))
	require.NoError(t, err)
	require.Equal(t, content, cas.storedBlob(t, response))
}

// A range only reaches the asset cache key through an http_header:*
// qualifier. The two qualifiers below also put one on the wire, but
// removeVolatileQualifiers strips them before keying, so a slice fetched
// through either would be stored as though it were the whole asset.
func TestHTTPFetcherFetchBlobRefusesRangesMissingFromTheCacheKey(t *testing.T) {
	content := apkLikeContent()
	origin := newRangeServingOrigin(t, content)

	for _, tc := range []struct {
		name       string
		qualifiers []*remoteasset.Qualifier
	}{
		{
			name: "PerURLQualifier",
			qualifiers: []*remoteasset.Qualifier{
				{Name: "http_header_url:0:Range", Value: controlRange},
			},
		},
		{
			name: "LegacyAuthHeadersQualifier",
			qualifiers: []*remoteasset.Qualifier{
				{Name: "bazel.auth_headers", Value: `{"` + origin.URL + `": {"Range": "` + controlRange + `"}}`},
			},
		},
		{
			// The mirror image: getAuthHeaders returns as soon as it sees
			// bazel.auth_headers, so the http_header:Range beside it never
			// reaches the wire. The origin serves the whole object while
			// the cache key names a slice of it.
			name: "LegacyAuthHeadersSwallowingTheKeyedRange",
			qualifiers: []*remoteasset.Qualifier{
				{Name: "bazel.auth_headers", Value: `{"` + origin.URL + `": {"Authorization": "Bearer letmein"}}`},
				{Name: "http_header:Range", Value: controlRange},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cas := newMemoryCAS()

			_, err := fetchBlob(context.Background(), cas, origin.URL, tc.qualifiers)
			require.Equal(t, codes.NotFound, status.Code(err))
			require.Zero(t, cas.blobCount())
		})
	}
}

// A multi-range request is answered with multipart/byteranges by a
// compliant origin, so the body is a MIME document rather than the asset.
// This runs against the real range-serving origin, which is what produces
// that shape.
func TestHTTPFetcherFetchBlobRefusesMultiRangeRequests(t *testing.T) {
	cas := newMemoryCAS()
	origin := newRangeServingOrigin(t, apkLikeContent())

	_, err := fetchBlob(context.Background(), cas, origin.URL, rangeQualifiers("bytes=0-9,20-29"))
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Contains(t, err.Error(), "cannot be checked against what it served")
	require.Zero(t, cas.blobCount())
}

// A range the origin cannot satisfy at all is still an error, and one that
// names the status rather than being folded into a bare NOT_FOUND.
func TestHTTPFetcherFetchBlobReportsUnsatisfiableRange(t *testing.T) {
	cas := newMemoryCAS()
	origin := newRangeServingOrigin(t, apkLikeContent())

	_, err := fetchBlob(context.Background(), cas, origin.URL, rangeQualifiers("bytes=99999-999999"))
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Contains(t, err.Error(), "416 Requested Range Not Satisfiable")
	require.Zero(t, cas.blobCount())
}

package fetch

import (
	"net/http"
	"strconv"
	"strings"

	remoteasset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rangeUnitBytes is the only range unit HTTP itself defines
// (RFC 9110 section 14.1).
const rangeUnitBytes = "bytes"

// qualifierHTTPHeaderRange is the qualifier a client sends a byte range
// under. bazel emits repository_ctx.download(headers = ...) this way
// whenever the remote downloader is enabled.
const qualifierHTTPHeaderRange = QualifierHTTPHeaderPrefix + "Range"

// byteRange is a single byte-range-spec from a Range request header, in
// either of the two forms RFC 9110 section 14.1.1 allows:
//
//   - an offset range, "bytes=<first>-<last>", or its open-ended form
//     "bytes=<first>-", which runs to the end of the representation;
//   - a suffix range, "bytes=-<suffixLength>", asking for the final
//     suffixLength bytes without knowing how long the representation is.
type byteRange struct {
	// suffix distinguishes a suffix range from an offset range, and
	// selects which of the fields below carry the request.
	suffix bool
	// suffixLength is the length of a suffix range.
	suffixLength int64
	// first is the first byte position of an offset range.
	first int64
	// last is the last byte position of an offset range, or -1 when the
	// range is open-ended.
	last int64
}

// parseByteRange interprets a Range request header.
//
// It returns nil for any header that does not reduce to a single byte
// range: an absent or empty one -- bazel sends "Range:" with no value for
// apk signature segments, which have no range -- as well as a multi-range
// request, a unit other than "bytes", and anything malformed. A nil result
// means "the response cannot be checked against what was asked for", not
// "nothing was asked for"; checkResponse handles it as the former.
func parseByteRange(header string) *byteRange {
	spec, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(header)), rangeUnitBytes+"=")
	if !ok {
		return nil
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok || strings.Contains(last, ",") {
		// Either no "-" at all, or a multi-range request. A 206 answering
		// the latter carries a multipart/byteranges body rather than the
		// bytes of the asset, so it is never something to cache.
		return nil
	}
	if first == "" {
		suffixLength, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffixLength <= 0 {
			return nil
		}
		return &byteRange{suffix: true, suffixLength: suffixLength}
	}
	firstPos, err := strconv.ParseInt(first, 10, 64)
	if err != nil || firstPos < 0 {
		return nil
	}
	if last == "" {
		return &byteRange{first: firstPos, last: -1}
	}
	lastPos, err := strconv.ParseInt(last, 10, 64)
	if err != nil || lastPos < firstPos {
		return nil
	}
	return &byteRange{first: firstPos, last: lastPos}
}

// resolve returns the first and last byte positions this range designates
// in a representation completeLength bytes long, applying the clipping
// rules of RFC 9110 section 14.1.1: an offset range running past the end
// stops at the end, and a suffix longer than the representation selects
// all of it. An empty representation resolves to an empty span, first 0
// and last -1.
func (r *byteRange) resolve(completeLength int64) (int64, int64) {
	if r.suffix {
		return max(0, completeLength-r.suffixLength), completeLength - 1
	}
	if r.last < 0 || r.last >= completeLength {
		return r.first, completeLength - 1
	}
	return r.first, r.last
}

// coversWholeRepresentation reports whether this range selects every byte
// of a representation completeLength bytes long, which is negative when
// the origin did not say. An origin that does not implement Range answers
// with the whole representation and a 200, and that is the content that
// was asked for only when this holds.
func (r *byteRange) coversWholeRepresentation(completeLength int64) bool {
	if completeLength < 0 {
		// Nothing is known about the length, so "bytes=0-" is the only
		// range that can be shown to cover it.
		return !r.suffix && r.first == 0 && r.last < 0
	}
	first, last := r.resolve(completeLength)
	return first == 0 && last == completeLength-1
}

// checkContentRange reports whether the bytes an origin says it served
// are exactly the bytes this range asked for.
func (r *byteRange) checkContentRange(served *contentRange) error {
	if served.completeLength >= 0 {
		first, last := r.resolve(served.completeLength)
		if served.first != first || served.last != last {
			return status.Errorf(codes.Internal, "Origin served bytes %d-%d of the requested range, but %d-%d were asked for", served.first, served.last, first, last)
		}
		return nil
	}

	// The origin would not say how long the whole representation is, so a
	// range legitimately clipped at the end cannot be told from a wrong
	// one. Accept only what can still be checked without that length.
	switch {
	case r.suffix:
		if servedLength := served.last - served.first + 1; servedLength != r.suffixLength {
			return status.Errorf(codes.Internal, "Origin served %d bytes for a suffix range of %d bytes", servedLength, r.suffixLength)
		}
	case r.last < 0:
		// Nothing pins the end of an open-ended range, so the start is
		// all there is to check; the origin asserts the rest runs to the
		// end of the representation.
		if served.first != r.first {
			return status.Errorf(codes.Internal, "Origin served bytes starting at %d, but %d was asked for", served.first, r.first)
		}
	default:
		if served.first != r.first || served.last != r.last {
			return status.Errorf(codes.Internal, "Origin served bytes %d-%d, but %d-%d were asked for", served.first, served.last, r.first, r.last)
		}
	}
	return nil
}

// contentRange is a Content-Range response header,
// "bytes <first>-<last>/<completeLength>", naming the bytes an origin
// served and how long the whole representation is.
type contentRange struct {
	first int64
	last  int64
	// completeLength is -1 when the origin sent "*", meaning it does not
	// know the length of the whole representation.
	completeLength int64
}

// parseContentRange interprets a Content-Range response header. A 206
// answering a multi-range request has no such header at all -- the ranges
// are described inside its multipart body instead -- so an empty header
// is an error like any other unparseable one.
func parseContentRange(header string) (*contentRange, error) {
	malformed := func() error {
		return status.Errorf(codes.Internal, "Origin returned an uninterpretable Content-Range %#v", header)
	}
	unit, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(unit, rangeUnitBytes) {
		return nil, malformed()
	}
	span, complete, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, malformed()
	}
	firstText, lastText, ok := strings.Cut(span, "-")
	if !ok {
		return nil, malformed()
	}
	first, err := strconv.ParseInt(firstText, 10, 64)
	if err != nil {
		return nil, malformed()
	}
	last, err := strconv.ParseInt(lastText, 10, 64)
	if err != nil {
		return nil, malformed()
	}
	completeLength := int64(-1)
	if complete != "*" {
		if completeLength, err = strconv.ParseInt(complete, 10, 64); err != nil {
			return nil, malformed()
		}
	}
	// The last clause rejects a span so long that last-first+1 overflows,
	// which would otherwise turn the body-length check into a no-op.
	if first < 0 || last < first || (completeLength >= 0 && last >= completeLength) || last-first+1 <= 0 {
		return nil, malformed()
	}
	return &contentRange{first: first, last: last, completeLength: completeLength}, nil
}

// cacheKeyRangeHeader returns the Range header that will form part of the
// asset cache key for a request, which is not always the Range header that
// goes out on the wire. A range can also reach the wire through the
// http_header_url:<i>:Range and bazel.auth_headers qualifiers, and
// removeVolatileQualifiers strips both before keying -- so a slice fetched
// that way would be stored as though it were the whole asset. Keying is
// what this range has to agree with, so it is what a response is checked
// against; a wire range that came from anywhere else disagrees with it and
// is refused.
//
// Reading it back out of removeVolatileQualifiers rather than re-deriving
// the rule keeps the two from drifting apart.
func cacheKeyRangeHeader(qualifiers []*remoteasset.Qualifier) string {
	for _, qualifier := range removeVolatileQualifiers(qualifiers) {
		// Matched without regard to case because a header name is
		// case-insensitive and http.Header.Set canonicalises it, so
		// http_header:range puts the very same header on the wire.
		if strings.EqualFold(qualifier.Name, qualifierHTTPHeaderRange) {
			return qualifier.Value
		}
	}
	return ""
}

// checkResponse reports whether resp is an acceptable answer to a request
// for rangeHeader, and returns the number of bytes resp's body must then
// contain, or -1 when the response carries no byte-range span to check a
// body against. A 200's Content-Length needs no such check: the HTTP client
// already fails a body that does not match it.
//
// A ranged request needs more than the status check a plain fetch needs.
// The range is part of the asset cache key, so whatever is stored under it
// is served to every later client asking for that same slice. An origin
// that answers a slice request with the whole representation, or with a
// different slice than the one asked for, must therefore not be cached at
// all.
//
// The errors below quote the requested range, which loggingFetcher would
// redact were it a qualifier value: RedactQualifiers covers http_header:*
// because such a value is routinely an Authorization header. A Range is
// the one header whose value is the diagnosis -- "which slice was refused"
// is the whole question -- and it carries no credential, so it is quoted
// rather than redacted.
func checkResponse(rangeHeader string, resp *http.Response) (int64, error) {
	requested := parseByteRange(rangeHeader)

	switch resp.StatusCode {
	case http.StatusOK:
		// The origin served the whole representation, either because
		// nothing else was asked for or because it does not implement
		// Range. An unparseable range lands here too, and is accepted:
		// the body then is the whole representation, which is what the
		// cache key -- a key naming a range no origin honoured -- will
		// go on to describe.
		// Stated without blame: the origin may have declined the range, or
		// may never have been sent it, as happens when bazel.auth_headers
		// makes getAuthHeaders return before it reaches http_header:Range.
		if requested != nil && !requested.coversWholeRepresentation(resp.ContentLength) {
			return -1, status.Errorf(codes.Internal, "Origin returned the whole representation rather than the requested range %#v", rangeHeader)
		}
		return -1, nil
	case http.StatusPartialContent:
		if rangeHeader == "" {
			return -1, status.Error(codes.Internal, "Origin returned a partial response to a request that asked for no byte range")
		}
		if requested == nil {
			return -1, status.Errorf(codes.Internal, "Origin returned a partial response to the range %#v, which cannot be checked against what it served", rangeHeader)
		}
		served, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return -1, err
		}
		if err := requested.checkContentRange(served); err != nil {
			return -1, err
		}
		return served.last - served.first + 1, nil
	default:
		return -1, status.Errorf(codes.Internal, "HTTP request failed with status %#v", resp.Status)
	}
}

package fetch

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseByteRange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   *byteRange
	}{
		// The shapes rules_apko sends. An apk is three concatenated gzip
		// segments, and it fetches each by range from the same URL;
		// signatures have no range at all, so bazel sends an empty header.
		{"Absent", "", nil},
		{"EmptySignatureRange", "  ", nil},
		{"ControlSegment", "bytes=0-1874", &byteRange{first: 0, last: 1874}},
		{"DataSegment", "bytes=1875-13893", &byteRange{first: 1875, last: 13893}},
		{"InitialSetupProbe", "bytes=0-0", &byteRange{first: 0, last: 0}},

		// The shape bazel adds itself when resuming a truncated fetch.
		{"OpenEnded", "bytes=4384-", &byteRange{first: 4384, last: -1}},

		{"Suffix", "bytes=-500", &byteRange{suffix: true, suffixLength: 500}},
		{"UnitIsCaseInsensitive", "BYTES=0-9", &byteRange{first: 0, last: 9}},
		{"SurroundingSpace", " bytes=0-9 ", &byteRange{first: 0, last: 9}},

		// Everything below is a range this server cannot check a response
		// against, and so must never accept a 206 for.
		{"MultipleRanges", "bytes=0-9,20-29", nil},
		{"UnknownUnit", "items=0-9", nil},
		{"NoUnit", "0-9", nil},
		{"NoHyphen", "bytes=5", nil},
		{"EmptySpec", "bytes=", nil},
		{"NonNumericFirst", "bytes=a-9", nil},
		{"NonNumericLast", "bytes=0-z", nil},
		{"NegativeFirst", "bytes=-5-9", nil},
		{"LastBeforeFirst", "bytes=9-0", nil},
		{"ZeroLengthSuffix", "bytes=-0", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parseByteRange(tc.header))
		})
	}
}

func TestByteRangeResolve(t *testing.T) {
	for _, tc := range []struct {
		name           string
		header         string
		completeLength int64
		wantFirst      int64
		wantLast       int64
	}{
		{"Exact", "bytes=1875-13893", 13894, 1875, 13893},
		// RFC 9110 section 14.1.1: a range running past the end of the
		// representation is clipped to it. packages.wolfi.dev does exactly
		// this -- measured 2026-08-16, "bytes=0-999999" against a 13894
		// byte .apk answers "Content-Range: bytes 0-13893/13894".
		{"ClippedAtEndOfRepresentation", "bytes=0-999999", 13894, 0, 13893},
		{"OpenEndedRunsToEnd", "bytes=1875-", 13894, 1875, 13893},
		{"SuffixCountsBack", "bytes=-100", 13894, 13794, 13893},
		{"SuffixLongerThanRepresentation", "bytes=-99999", 13894, 0, 13893},
		{"EmptyRepresentation", "bytes=0-9", 0, 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, last := parseByteRange(tc.header).resolve(tc.completeLength)
			require.Equal(t, tc.wantFirst, first)
			require.Equal(t, tc.wantLast, last)
		})
	}
}

func TestByteRangeCoversWholeRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		header         string
		completeLength int64
		want           bool
	}{
		{"Slice", "bytes=0-1874", 13894, false},
		{"SliceOfUnknownLength", "bytes=0-1874", -1, false},
		{"ExactWholeRepresentation", "bytes=0-13893", 13894, true},
		{"PastTheEnd", "bytes=0-999999", 13894, true},
		{"OpenEndedFromZero", "bytes=0-", 13894, true},
		// "bytes=0-" asks for everything however long the representation
		// turns out to be, so it holds even when the origin does not say.
		{"OpenEndedFromZeroOfUnknownLength", "bytes=0-", -1, true},
		{"OpenEndedFromOffset", "bytes=1-", 13894, false},
		{"OpenEndedFromOffsetOfUnknownLength", "bytes=1-", -1, false},
		{"SuffixShorterThanRepresentation", "bytes=-100", 13894, false},
		{"SuffixLongerThanRepresentation", "bytes=-99999", 13894, true},
		{"SuffixOfUnknownLength", "bytes=-99999", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parseByteRange(tc.header).coversWholeRepresentation(tc.completeLength))
		})
	}
}

func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   *contentRange
	}{
		{"Complete", "bytes 0-1874/13894", &contentRange{first: 0, last: 1874, completeLength: 13894}},
		{"SingleByte", "bytes 0-0/13894", &contentRange{first: 0, last: 0, completeLength: 13894}},
		{"UnknownCompleteLength", "bytes 0-1874/*", &contentRange{first: 0, last: 1874, completeLength: -1}},
		{"UnitIsCaseInsensitive", "BYTES 0-1874/13894", &contentRange{first: 0, last: 1874, completeLength: 13894}},

		// A 206 answering a multi-range request describes its ranges inside
		// a multipart body and carries no Content-Range header at all.
		{"Absent", "", nil},
		{"UnknownUnit", "items 0-1874/13894", nil},
		{"Unsatisfied", "bytes */13894", nil},
		{"NoCompleteLength", "bytes 0-1874", nil},
		{"NonNumeric", "bytes a-b/13894", nil},
		{"LastBeforeFirst", "bytes 100-0/13894", nil},
		{"RunsPastCompleteLength", "bytes 0-13894/13894", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContentRange(tc.header)
			if tc.want == nil {
				require.Error(t, err)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// An origin that does not state the complete length leaves a range clipped
// at the end indistinguishable from a wrong one, so only what can still be
// checked without that length is accepted.
func TestByteRangeCheckContentRangeWithoutCompleteLength(t *testing.T) {
	for _, tc := range []struct {
		name         string
		header       string
		contentRange string
		wantAccepted bool
	}{
		{"ExactMatch", "bytes=1875-13893", "bytes 1875-13893/*", true},
		{"ShortenedWithoutProofOfTheEnd", "bytes=1875-13893", "bytes 1875-9999/*", false},
		{"DifferentStart", "bytes=1875-13893", "bytes 0-12018/*", false},
		{"OpenEndedKeepsItsStart", "bytes=1875-", "bytes 1875-13893/*", true},
		{"OpenEndedWithWrongStart", "bytes=1875-", "bytes 0-13893/*", false},
		{"SuffixOfTheRightLength", "bytes=-100", "bytes 13794-13893/*", true},
		{"SuffixOfTheWrongLength", "bytes=-100", "bytes 13794-13800/*", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served, err := parseContentRange(tc.contentRange)
			require.NoError(t, err)

			err = parseByteRange(tc.header).checkContentRange(served)
			if tc.wantAccepted {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

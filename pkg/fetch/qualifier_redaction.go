package fetch

import (
	"strings"

	remoteasset "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
)

// RedactedQualifierValue is substituted for the value of a qualifier
// that carries a caller-supplied HTTP header. Bazel's
// --experimental_remote_downloader_propagate_credentials forwards the
// client's credentials this way, so such a value is routinely an
// Authorization header holding a live bearer token.
const RedactedQualifierValue = "<redacted>"

// qualifierCarriesHeaders reports whether a qualifier's value is a
// caller-supplied HTTP header, or a bundle of them, as opposed to
// metadata describing the asset itself such as checksum.sri.
func qualifierCarriesHeaders(name string) bool {
	if name == QualifierLegacyBazelHTTPHeaders {
		return true
	}
	return strings.HasPrefix(name, QualifierHTTPHeaderPrefix) ||
		strings.HasPrefix(name, QualifierHTTPHeaderURLPrefix)
}

// RedactQualifiers returns a copy of qualifiers whose header-carrying
// values are replaced with RedactedQualifierValue, for use where the
// qualifier list is written somewhere a credential must not go.
//
// Qualifier NAMES are preserved: a name encodes the header and, for
// http_header_url, the URI it applies to, which is the part with
// diagnostic value. A name never holds the header's value.
//
// This is deliberately broader than removeVolatileQualifiers, which
// governs cache keys and must leave http_header:* in place because such
// a header (Accept, for one) can select which representation the origin
// serves.
func RedactQualifiers(qualifiers []*remoteasset.Qualifier) []*remoteasset.Qualifier {
	redacted := make([]*remoteasset.Qualifier, 0, len(qualifiers))
	for _, q := range qualifiers {
		if !qualifierCarriesHeaders(q.Name) {
			redacted = append(redacted, q)
			continue
		}
		redacted = append(redacted, &remoteasset.Qualifier{
			Name:  q.Name,
			Value: RedactedQualifierValue,
		})
	}
	return redacted
}

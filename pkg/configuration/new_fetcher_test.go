package configuration

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCompileOriginOnlyURIRegexes(t *testing.T) {
	quayToken := "https://quay.io/v2/auth?scope=repository:oauth2-proxy/oauth2-proxy:pull&service=quay.io"

	t.Run("EmptyListFallsBackToTheBuiltInList", func(t *testing.T) {
		// Act
		compiled, err := compileOriginOnlyURIRegexes(nil)

		// Assert
		require.NoError(t, err)
		require.True(t, matchesAny(compiled, quayToken))
	})

	t.Run("ConfiguredListReplacesTheBuiltInList", func(t *testing.T) {
		// Act
		compiled, err := compileOriginOnlyURIRegexes([]string{`^https://registry\.example\.com/login$`})

		// Assert
		require.NoError(t, err)
		require.True(t, matchesAny(compiled, "https://registry.example.com/login"))
		require.False(t, matchesAny(compiled, quayToken))
	})

	t.Run("InvalidRegexIsAnInvalidArgument", func(t *testing.T) {
		// Act
		_, err := compileOriginOnlyURIRegexes([]string{`(`})

		// Assert
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}

func matchesAny(compiled []*regexp.Regexp, uri string) bool {
	for _, re := range compiled {
		if re.MatchString(uri) {
			return true
		}
	}
	return false
}

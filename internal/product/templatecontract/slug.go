package templatecontract

import (
	"fmt"
	"regexp"
)

// slugPattern is the spec §8.1 slug constraint: lowercase,
// [a-z0-9][a-z0-9-_]{0,63} — at most 64 characters, starting with a
// lowercase letter or digit.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]{0,63}$`)

// ValidateSlug enforces the spec §8.1 slug constraint. Accepted:
// "financial-news". Rejected: uppercase, spaces, empty, >64 characters,
// leading separators, and any character outside [a-z0-9-_].
func ValidateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return &Error{
			Code:    CodeSlugInvalid,
			Message: fmt.Sprintf("invalid template slug %q: must match [a-z0-9][a-z0-9-_]{0,63} (lowercase, max 64 chars)", slug),
		}
	}
	return nil
}

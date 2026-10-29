package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const identifierRegexPattern = `^[a-z0-9][a-z0-9_-]{0,62}$`

var validIdentifierPattern = regexp.MustCompile(identifierRegexPattern)

var ErrInvalidIdentifier = errors.New("invalid resource identifier")

func ValidateName(identifier string) error {
	if !validIdentifierPattern.MatchString(identifier) {
		return fmt.Errorf("%w %q: use 1-63 lowercase alphanumeric characters, hyphens, or underscores starting with a letter or digit", ErrInvalidIdentifier, identifier)
	}
	return nil
}

func SafeJoinPath(baseDir string, segments ...string) (string, error) {
	if strings.TrimSpace(baseDir) == "" {
		return "", errors.New("resolve safe path: base directory is empty; provide a non-empty root directory")
	}
	absBase, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return "", fmt.Errorf("resolve base directory %q: %w", baseDir, err)
	}
	for _, segment := range segments {
		if err := validatePathSegment(segment); err != nil {
			return "", err
		}
	}
	pathParts := append([]string{absBase}, segments...)
	resolvedTarget := filepath.Clean(filepath.Join(pathParts...))
	requiredPrefix := absBase + string(filepath.Separator)
	if !strings.HasPrefix(resolvedTarget, requiredPrefix) {
		return "", fmt.Errorf("resolve safe path %q: target escapes base directory %q; remove traversal segments", resolvedTarget, absBase)
	}
	return resolvedTarget, nil
}

func validatePathSegment(segment string) error {
	if strings.ContainsRune(segment, 0) {
		return errors.New("validate path segment: contains null byte; provide printable path segments")
	}
	if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, `/\`) {
		return fmt.Errorf("validate path segment %q: must be a single relative name without path separators or traversal dots", segment)
	}
	return nil
}
